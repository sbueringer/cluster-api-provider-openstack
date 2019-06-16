/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"k8s.io/client-go/kubernetes"
	"net"
	"os"
	"reflect"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack/services/kubeadm"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack/services/userdata"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	tokenutil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/klog"
	kubeadmv1beta1 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta1"
	openstackconfigv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	providerv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/bootstrap"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack/clients"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/record"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	apierrors "sigs.k8s.io/cluster-api/pkg/errors"
	"sigs.k8s.io/cluster-api/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clconfig "github.com/coreos/container-linux-config-transpiler/config"
)

const (
	UserDataKey          = "userData"
	DisableTemplatingKey = "disableTemplating"
	PostprocessorKey     = "postprocessor"

	localIPV4Lookup = "${COREOS_OPENSTACK_IPV4_LOCAL}"

	cloudProvider       = "openstack"
	cloudProviderConfig = "/etc/kubernetes/cloud.conf"

	nodeRole = "node-role.kubernetes.io/node="

	// containerdSocket is the path to containerd socket.
	containerdSocket = "/var/run/containerd/containerd.sock"

	// dockerdSocket is the path to dockerd socket.
	dockerdSocket = "/var/run/dockershim.sock"

	TimeoutInstanceCreate       = 5
	TimeoutInstanceDelete       = 5
	RetryIntervalInstanceStatus = 10 * time.Second

	TokenTTL = 60 * time.Minute
)

type OpenstackClient struct {
	params openstack.ActuatorParams
	scheme *runtime.Scheme
	client client.Client
	*openstack.DeploymentClient
}

func NewActuator(params openstack.ActuatorParams) (*OpenstackClient, error) {
	return &OpenstackClient{
		params:           params,
		client:           params.Client,
		scheme:           params.Scheme,
		DeploymentClient: openstack.NewDeploymentClient(),
	}, nil
}

func getTimeout(name string, timeout int) time.Duration {
	if v := os.Getenv(name); v != "" {
		timeout, err := strconv.Atoi(v)
		if err == nil {
			return time.Duration(timeout)
		}
	}
	return time.Duration(timeout)
}

func (oc *OpenstackClient) Create(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if cluster == nil {
		return fmt.Errorf("the cluster is nil, check your cluster configuration")
	}

	machineService, err := clients.NewInstanceServiceFromMachine(oc.params.KubeClient, cluster.Name, machine)
	if err != nil {
		return err
	}

	networkService, err := clients.NewNetworkService(machineService.NetworkClient)
	if err != nil {
		return err
	}

	// Load provider status.
	providerStatus, err := providerv1.ClusterStatusFromProviderStatus(cluster.Status.ProviderStatus)
	if err != nil {
		return errors.Errorf("failed to load cluster provider status: %v", err)
	}

	err = oc.createInstance(ctx, machineService, cluster, machine)
	if err != nil {
		return err
	}

	return networkService.CreateLBMember(machineService.NetworkClient, machine, providerStatus)
}

func (oc *OpenstackClient) createInstance(ctx context.Context, machineService *clients.InstanceService, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {

	kubeClient := oc.params.KubeClient

	clusterProviderSpec, err := openstackconfigv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("cannot unmarshal cluster providerSpec field: %v", err)
	}

	clusterProviderStatus, err := openstackconfigv1.ClusterStatusFromProviderStatus(cluster.Status.ProviderStatus)
	if err != nil {
		return fmt.Errorf("cannot unmarshal cluster providerStatus field: %v", err)
	}

	providerSpec, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return oc.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err))
	}

	if verr := oc.validateMachine(machine, providerSpec); verr != nil {
		return oc.handleMachineError(machine, verr)
	}

	instance, err := oc.instanceExists(cluster, machine)
	if err != nil {
		return err
	}
	if instance != nil {
		klog.Infof("Skipped creating a VM that already exists.\n")
		return nil
	}

	// get machine startup script
	var userData string
	var disableTemplating bool
	var postprocessor string
	if providerSpec.UserDataSecret != nil {
		userData, disableTemplating, postprocessor, err = getUserDataFromSecret(providerSpec, machine, kubeClient)
		if err != nil {
			return err
		}
	} else {
		if oc.params.Config.UserDataControlPlane != "" && oc.params.Config.UserDataWorker != "" {
			userData, disableTemplating, postprocessor, err = getUserDataFromConfig(oc.params.Config, machine)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("no user data provided")
		}
	}

	var userDataRendered string
	if len(userData) > 0 && !disableTemplating {

		clusterMachines, err := oc.params.ClusterClient.Machines(cluster.Namespace).List(metav1.ListOptions{})
		if err != nil {
			return errors.Wrapf(err, "failed to retrieve machines in cluster %q", cluster.Name)
		}

		controlPlaneMachines := GetControlPlaneMachines(clusterMachines)

		isNodeJoin, err := oc.isNodeJoin(cluster, machine, controlPlaneMachines)
		if err != nil {
			return errors.Wrapf(err, "failed to determine whether machine %q should join cluster %q", machine.Name, cluster.Name)
		}

		var bootstrapToken string
		if isNodeJoin {
			klog.Info("Creating bootstrap token")
			bootstrapToken, err = oc.createBootstrapToken(cluster)
			if err != nil {
				return oc.handleMachineError(machine, apierrors.CreateMachine(
					"error creating Openstack instance: %v", err))
			}
		}

		kubeadmFiles, err := generateKubeadmFiles(util.IsControlPlaneMachine(machine), bootstrapToken, cluster, machine, providerSpec, clusterProviderSpec, clusterProviderStatus)
		if err != nil {
			return oc.handleMachineError(machine, apierrors.CreateMachine(
				"error generating kubeadm files: %v", err))
		}

		userDataRendered, err = startupScript(machine, machineService.CloudConf, kubeadmFiles, string(userData))
		if err != nil {
			return oc.handleMachineError(machine, apierrors.CreateMachine(
				"error creating Openstack instance: %v", err))
		}

		if util.IsControlPlaneMachine(machine) && isNodeJoin {
			userDataRendered = strings.ReplaceAll(userDataRendered, "init --skip-phases=addon", "kubeadm join")
		}
	} else {
		userDataRendered = string(userData)
	}

	switch postprocessor {
	// Postprocess with the Container Linux ct transpiler.
	case "ct":
		clcfg, ast, report := clconfig.Parse([]byte(userDataRendered))
		if len(report.Entries) > 0 {
			return fmt.Errorf("postprocessor error: %s", report.String())
		}

		ignCfg, report := clconfig.Convert(clcfg, "openstack-metadata", ast)
		if len(report.Entries) > 0 {
			return fmt.Errorf("postprocessor error: %s", report.String())
		}

		ud, err := json.Marshal(&ignCfg)
		if err != nil {
			return fmt.Errorf("postprocessor error: %s", err)
		}

		userDataRendered = string(ud)
	case "":
		// no post processor set > don't post process
	default:
		return fmt.Errorf("postprocessor error: unknown postprocessor: '%s'", postprocessor)
	}

	clusterSpec, err := openstackconfigv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return oc.handleMachineError(machine, apierrors.CreateMachine(
			"error creating Openstack instance: %v", err))
	}
	clusterName := fmt.Sprintf("%s-%s", cluster.ObjectMeta.Namespace, cluster.Name)
	instance, err = machineService.InstanceCreate(clusterName, machine.Name, clusterSpec, providerSpec, userDataRendered, providerSpec.KeyName)

	if err != nil {
		return oc.handleMachineError(machine, apierrors.CreateMachine(
			"error creating Openstack instance: %v", err))
	}
	instanceCreateTimeout := getTimeout("CLUSTER_API_OPENSTACK_INSTANCE_CREATE_TIMEOUT", TimeoutInstanceCreate)
	instanceCreateTimeout = instanceCreateTimeout * time.Minute
	err = util.PollImmediate(RetryIntervalInstanceStatus, instanceCreateTimeout, func() (bool, error) {
		instance, err := machineService.GetInstance(instance.ID)
		if err != nil {
			return false, nil
		}
		return instance.Status == "ACTIVE", nil
	})
	if err != nil {
		return oc.handleMachineError(machine, apierrors.CreateMachine(
			"error creating Openstack instance: %v", err))
	}

	if providerSpec.FloatingIP != "" {
		err := machineService.AssociateFloatingIP(providerSpec, instance.ID, providerSpec.FloatingIP)
		if err != nil {
			return oc.handleMachineError(machine, apierrors.CreateMachine(
				"Associate floatingIP err: %v", err))
		}

	}

	record.Eventf(machine, "CreatedInstance", "Created new instance with id: %s", instance.ID)
	return oc.updateAnnotation(cluster, machine, instance.ID)
}

func getUserDataFromConfig(config openstack.ActuatorConfig, machine *clusterv1.Machine) (string, bool, string, error) {
	var templatePath string
	if util.IsControlPlaneMachine(machine) {
		templatePath = config.UserDataControlPlane
	} else {
		templatePath = config.UserDataWorker
	}
	userData, err := ioutil.ReadFile(templatePath)
	if err != nil {
		return "", false, "", err
	}
	return string(userData), false, config.UserDataPostprocessor, nil
}

func getUserDataFromSecret(providerSpec *openstackconfigv1.OpenstackProviderSpec, machine *clusterv1.Machine, client kubernetes.Interface) (string, bool, string, error) {
	namespace := providerSpec.UserDataSecret.Namespace
	if namespace == "" {
		namespace = machine.Namespace
	}

	if providerSpec.UserDataSecret.Name == "" {
		return "", false, "", fmt.Errorf("UserDataSecret name must be provided")
	}

	userDataSecret, err := client.CoreV1().Secrets(namespace).Get(providerSpec.UserDataSecret.Name, metav1.GetOptions{})
	if err != nil {
		return "", false, "", err
	}

	userData, ok := userDataSecret.Data[UserDataKey]
	if !ok {
		return "", false, "", fmt.Errorf("machine's userdata secret %v in namespace %v did not contain key %v", providerSpec.UserDataSecret.Name, namespace, UserDataKey)
	}

	_, disableTemplating := userDataSecret.Data[DisableTemplatingKey]

	postprocessor, _ := userDataSecret.Data[PostprocessorKey]

	return string(userData), disableTemplating, string(postprocessor), nil
}

// TODO fix this. (it looks like you can specify the port in the cluster.yaml but actually only 6443 works)
const apiServerBindPort = 6443

func generateKubeadmFiles(isControlPlane bool, bootstrapToken string, cluster *clusterv1.Cluster, machine *clusterv1.Machine, machineProviderSpec *openstackconfigv1.OpenstackProviderSpec, clusterProviderSpec *openstackconfigv1.OpenstackClusterProviderSpec, clusterProviderStatus *openstackconfigv1.OpenstackClusterProviderStatus) (string, error) {

	caCertHash, err := clients.GenerateCertificateHash(clusterProviderSpec.CAKeyPair.Cert)
	if err != nil {
		return "", err
	}

	if clusterProviderStatus.Network == nil || clusterProviderStatus.Network.APIServerLoadBalancer == nil {
		return "", fmt.Errorf("load balancer has not been created yet, waiting until load balancer exists")
	}

	if isControlPlane {

		var userData string

		if bootstrapToken == "" {
			klog.Info("Machine is the first control plane machine for the cluster")
			if !clusterProviderSpec.CAKeyPair.HasCertAndKey() {
				return "", fmt.Errorf("failed to run controlplane, missing CAPrivateKey")
			}

			// TODO required as long as our Openstack doesn't allow access to the LB IP
			clusterConfigurationCopy := clusterProviderSpec.ClusterConfiguration.DeepCopy()
			clusterConfigurationCopy.ControlPlaneEndpoint = fmt.Sprintf("%s:%d", localIPV4Lookup, apiServerBindPort)

			kubeadm.SetClusterConfigurationOptions(
				clusterConfigurationCopy,
				kubeadm.WithKubernetesVersion(machine.Spec.Versions.ControlPlane),
				kubeadm.WithAPIServerCertificateSANs(localIPV4Lookup, clusterProviderStatus.Network.APIServerLoadBalancer.IP, clusterProviderStatus.Network.APIServerLoadBalancer.InternalIP),
				kubeadm.WithAPIServerExtraArgs(map[string]string{"cloud-provider": cloudProvider}),
				kubeadm.WithAPIServerExtraArgs(map[string]string{"cloud-config": cloudProviderConfig}),
				kubeadm.WithAPIServerExtraVolumes([]kubeadmv1beta1.HostPathMount{
					{
						Name:      "cloud",
						HostPath:  "/etc/kubernetes/cloud.conf",
						MountPath: "/etc/kubernetes/cloud.conf",
					},
				}),
				kubeadm.WithControllerManagerExtraArgs(map[string]string{"cloud-provider": cloudProvider}),
				kubeadm.WithControllerManagerExtraArgs(map[string]string{"cloud-config": cloudProviderConfig}),
				kubeadm.WithControllerManagerExtraVolumes([]kubeadmv1beta1.HostPathMount{
					{
						Name:      "cloud",
						HostPath:  "/etc/kubernetes/cloud.conf",
						MountPath: "/etc/kubernetes/cloud.conf",
					},
				}),
				kubeadm.WithClusterNetworkFromClusterNetworkingConfig(cluster.Spec.ClusterNetwork),
				kubeadm.WithKubernetesVersion(machine.Spec.Versions.ControlPlane),
			)
			clusterConfigYAML, err := kubeadm.ConfigurationToYAML(clusterConfigurationCopy)
			if err != nil {
				return "", err
			}

			kubeadm.SetInitConfigurationOptions(
				&machineProviderSpec.KubeadmConfiguration.Init,
				kubeadm.WithNodeRegistrationOptions(
					kubeadm.NewNodeRegistration(
						kubeadm.WithTaints(machine.Spec.Taints),
						kubeadm.WithCRISocket(dockerdSocket),
						kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-provider": cloudProvider}),
						kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-config": cloudProviderConfig}),
						kubeadm.WithKubeletExtraArgs(map[string]string{"node-labels": "dhc-type=master,dhc-version=TODO," + nodeRole}),
					),
				),
				kubeadm.WithInitLocalAPIEndpointAndPort(localIPV4Lookup, apiServerBindPort),
			)
			initConfigYAML, err := kubeadm.ConfigurationToYAML(&machineProviderSpec.KubeadmConfiguration.Init)
			if err != nil {
				return "", err
			}

			userData, err = userdata.NewControlPlane(&userdata.ControlPlaneInput{
				CACert:               string(clusterProviderSpec.CAKeyPair.Cert),
				CAKey:                string(clusterProviderSpec.CAKeyPair.Key),
				EtcdCACert:           string(clusterProviderSpec.EtcdCAKeyPair.Cert),
				EtcdCAKey:            string(clusterProviderSpec.EtcdCAKeyPair.Key),
				FrontProxyCACert:     string(clusterProviderSpec.FrontProxyCAKeyPair.Cert),
				FrontProxyCAKey:      string(clusterProviderSpec.FrontProxyCAKeyPair.Key),
				SaCert:               string(clusterProviderSpec.SAKeyPair.Cert),
				SaKey:                string(clusterProviderSpec.SAKeyPair.Key),
				ClusterConfiguration: clusterConfigYAML,
				InitConfiguration:    initConfigYAML,
			})
			if err != nil {
				return "", err
			}
			return userData, nil

		} else {
			klog.Info("Allowing a machine to join the control plane")

			kubeadm.SetJoinConfigurationOptions(
				&machineProviderSpec.KubeadmConfiguration.Join,
				kubeadm.WithBootstrapTokenDiscovery(
					kubeadm.NewBootstrapTokenDiscovery(
						// TODO required as long as our Openstack doesn't allow access to the LB IP
						kubeadm.WithAPIServerEndpoint(fmt.Sprintf("%s:%d", clusterProviderStatus.Network.APIServerLoadBalancer.InternalIP, apiServerBindPort)),
						// normal solution:
						//kubeadm.WithAPIServerEndpoint(clusterProviderSpec.ClusterConfiguration.ControlPlaneEndpoint),
						kubeadm.WithToken(bootstrapToken),
						kubeadm.WithCACertificateHash(caCertHash),
					),
				),
				kubeadm.WithJoinNodeRegistrationOptions(
					kubeadm.NewNodeRegistration(
						kubeadm.WithTaints(machine.Spec.Taints),
						kubeadm.WithCRISocket(dockerdSocket),
						kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-provider": cloudProvider}),
						kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-config": cloudProviderConfig}),
						kubeadm.WithKubeletExtraArgs(map[string]string{"node-labels": "dhc-type=master,dhc-version=TODO," + nodeRole}),
					),
				),
				// this also creates .controlPlane
				kubeadm.WithLocalAPIEndpointAndPort(localIPV4Lookup, apiServerBindPort),
			)
			joinConfigurationYAML, err := kubeadm.ConfigurationToYAML(&machineProviderSpec.KubeadmConfiguration.Join)
			if err != nil {
				return "", err
			}

			userData, err = userdata.JoinControlPlane(&userdata.ContolPlaneJoinInput{
				CACert:            string(clusterProviderSpec.CAKeyPair.Cert),
				CAKey:             string(clusterProviderSpec.CAKeyPair.Key),
				EtcdCACert:        string(clusterProviderSpec.EtcdCAKeyPair.Cert),
				EtcdCAKey:         string(clusterProviderSpec.EtcdCAKeyPair.Key),
				FrontProxyCACert:  string(clusterProviderSpec.FrontProxyCAKeyPair.Cert),
				FrontProxyCAKey:   string(clusterProviderSpec.FrontProxyCAKeyPair.Key),
				SaCert:            string(clusterProviderSpec.SAKeyPair.Cert),
				SaKey:             string(clusterProviderSpec.SAKeyPair.Key),
				JoinConfiguration: joinConfigurationYAML,
			})
			if err != nil {
				return "", err
			}
			return userData, nil
		}
	} else {
		klog.Info("Joining a worker node to the cluster")

		kubeadm.SetJoinConfigurationOptions(
			&machineProviderSpec.KubeadmConfiguration.Join,
			kubeadm.WithBootstrapTokenDiscovery(
				kubeadm.NewBootstrapTokenDiscovery(
					// TODO required as long as our Openstack doesn't allow access to the LB IP
					kubeadm.WithAPIServerEndpoint(fmt.Sprintf("%s:%d", clusterProviderStatus.Network.APIServerLoadBalancer.InternalIP, apiServerBindPort)),
					// normal solution:
					//kubeadm.WithAPIServerEndpoint(clusterProviderSpec.ClusterConfiguration.ControlPlaneEndpoint),
					kubeadm.WithToken(bootstrapToken),
					kubeadm.WithCACertificateHash(caCertHash),
					// needed?
					// * tlsBootstrapToken (same as token)
					// * unsafeSkipCAVerification (should be fixed through ca hash_
				),
			),
			kubeadm.WithJoinNodeRegistrationOptions(
				kubeadm.NewNodeRegistration(
					kubeadm.WithTaints(machine.Spec.Taints),
					kubeadm.WithCRISocket(dockerdSocket),
					kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-provider": cloudProvider}),
					kubeadm.WithKubeletExtraArgs(map[string]string{"cloud-config": cloudProviderConfig}),
					kubeadm.WithKubeletExtraArgs(map[string]string{"node-labels": nodeRole}),
					// TODO ingress nodes
					kubeadm.WithKubeletExtraArgs(map[string]string{"node-labels": "dhc-type=node,dhc-version=TODO,node.caas.daimler.com/ingress=true," + nodeRole}),
				),
			),
		)
		joinConfigurationYAML, err := kubeadm.ConfigurationToYAML(&machineProviderSpec.KubeadmConfiguration.Join)
		if err != nil {
			return "", err
		}

		kubeadmFiles, err := userdata.NewNode(&userdata.NodeInput{
			JoinConfiguration: joinConfigurationYAML,
		})
		if err != nil {
			return "", err
		}
		return kubeadmFiles, nil
	}
}

// GetControlPlaneMachines retrieves all control plane nodes from a MachineList
func GetControlPlaneMachines(machineList *clusterv1.MachineList) []*clusterv1.Machine {
	var cpm []*clusterv1.Machine
	for _, m := range machineList.Items {
		if m.Spec.Versions.ControlPlane != "" {
			cpm = append(cpm, m.DeepCopy())
		}
	}
	return cpm
}

// isNodeJoin determines if a machine should join of the cluster.
// TODO impl it on-par with AWS
// TODO: Make this thread safe kubernetes-sigs/cluster-api#925
// https://github.com/kubernetes-sigs/cluster-api-provider-aws/pull/745#discussion_r280506890
func (oc *OpenstackClient) isNodeJoin(cluster *clusterv1.Cluster, machine *clusterv1.Machine, controlPlaneMachines []*clusterv1.Machine) (bool, error) {
	if !util.IsControlPlaneMachine(machine) {
		// Worker machines, not part of the controlplane, will always join the cluster.
		return true, nil
	} else {
		// Controlplane machines will join the cluster if the cluster has an existing control plane.

		for _, cm := range controlPlaneMachines {

			instance, err := oc.instanceExists(cluster, cm)
			if err != nil {
				return false, errors.Wrapf(err, "failed to verify existence of machine %s", cm.Name)
			}
			if instance != nil {
				klog.Infof("Found control plane machine \n")
				return true, nil
			}
		}

		klog.Infof("Could not find control plane machine: creating first control plane machine \n")
		return false, nil
	}
}

func (oc *OpenstackClient) Delete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	machineService, err := clients.NewInstanceServiceFromMachine(oc.params.KubeClient, cluster.Name, machine)
	if err != nil {
		return err
	}

	networkService, err := clients.NewNetworkService(machineService.NetworkClient)
	if err != nil {
		return err
	}

	// Load provider status.
	providerStatus, err := providerv1.ClusterStatusFromProviderStatus(cluster.Status.ProviderStatus)
	if err != nil {
		return errors.Errorf("failed to load cluster provider status: %v", err)
	}

	err = networkService.DeleteLBMember(machineService.NetworkClient, machine, providerStatus)
	if err != nil {
		return err
	}

	instance, err := oc.instanceExists(cluster, machine)
	if err != nil {
		return err
	}

	if instance == nil {
		klog.Infof("Skipped deleting %s that is already deleted.\n", machine.Name)
		return nil
	}

	id := machine.ObjectMeta.Annotations[openstack.OpenstackIdAnnotationKey]
	err = machineService.InstanceDelete(id)
	if err != nil {
		return oc.handleMachineError(machine, apierrors.DeleteMachine(
			"error deleting Openstack instance: %v", err))
	}
	record.Eventf(machine, "DeletedInstance", "Deleted instance with id: %s", instance.ID)
	return nil
}

func (oc *OpenstackClient) Update(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if cluster == nil {
		return fmt.Errorf("The cluster is nil, check your cluster configuration")
	}

	status, err := oc.instanceStatus(machine)
	if err != nil {
		return err
	}

	currentMachine := (*clusterv1.Machine)(status)
	if currentMachine == nil {
		instance, err := oc.instanceExists(cluster, machine)
		if err != nil {
			return err
		}
		if instance != nil && instance.Status == "ACTIVE" {
			klog.Infof("Populating current state for boostrap machine %v", machine.ObjectMeta.Name)
			return oc.updateAnnotation(cluster, machine, instance.ID)
		} else {
			return fmt.Errorf("Cannot retrieve current state to update machine %v", machine.ObjectMeta.Name)
		}
	}

	if !oc.requiresUpdate(currentMachine, machine) {
		return nil
	}

	if util.IsControlPlaneMachine(currentMachine) {
		// TODO: add master inplace
		klog.Errorf("master inplace update failed: not support master in place update now")
	} else {
		klog.Infof("re-creating machine %s for update.", currentMachine.ObjectMeta.Name)
		err = oc.Delete(ctx, cluster, currentMachine)
		if err != nil {
			klog.Errorf("delete machine %s for update failed: %v", currentMachine.ObjectMeta.Name, err)
		} else {
			instanceDeleteTimeout := getTimeout("CLUSTER_API_OPENSTACK_INSTANCE_DELETE_TIMEOUT", TimeoutInstanceDelete)
			instanceDeleteTimeout = instanceDeleteTimeout * time.Minute
			err = util.PollImmediate(RetryIntervalInstanceStatus, instanceDeleteTimeout, func() (bool, error) {
				instance, err := oc.instanceExists(cluster, machine)
				if err != nil {
					return false, nil
				}
				return instance == nil, nil
			})
			if err != nil {
				return oc.handleMachineError(machine, apierrors.DeleteMachine(
					"error deleting Openstack instance: %v", err))
			}

			err = oc.Create(ctx, cluster, machine)
			if err != nil {
				klog.Errorf("create machine %s for update failed: %v", machine.ObjectMeta.Name, err)
			}
			klog.Infof("Successfully updated machine %s", currentMachine.ObjectMeta.Name)
		}
	}

	return nil
}

func (oc *OpenstackClient) Exists(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (bool, error) {
	instance, err := oc.instanceExists(cluster, machine)
	if err != nil {
		return false, err
	}
	return instance != nil, err
}

func getIPFromInstance(instance *clients.Instance) (string, error) {
	if instance.AccessIPv4 != "" && net.ParseIP(instance.AccessIPv4) != nil {
		return instance.AccessIPv4, nil
	}
	type networkInterface struct {
		Address string  `json:"addr"`
		Version float64 `json:"version"`
		Type    string  `json:"OS-EXT-IPS:type"`
	}
	var addrList []string

	for _, b := range instance.Addresses {
		list, err := json.Marshal(b)
		if err != nil {
			return "", fmt.Errorf("extract IP from instance err: %v", err)
		}
		var networks []interface{}
		json.Unmarshal(list, &networks)
		for _, network := range networks {
			var netInterface networkInterface
			b, _ := json.Marshal(network)
			json.Unmarshal(b, &netInterface)
			if netInterface.Version == 4.0 {
				if netInterface.Type == "floating" {
					return netInterface.Address, nil
				}
				addrList = append(addrList, netInterface.Address)
			}
		}
	}
	if len(addrList) != 0 {
		return addrList[0], nil
	}
	return "", fmt.Errorf("extract IP from instance err")
}

// If the OpenstackClient has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (oc *OpenstackClient) handleMachineError(machine *clusterv1.Machine, err *apierrors.MachineError) error {
	if oc.client != nil {
		reason := err.Reason
		message := err.Message
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message
		if err := oc.client.Update(nil, machine); err != nil {
			return fmt.Errorf("unable to update machine status: %v", err)
		}
	}

	klog.Errorf("Machine error %s: %v", machine.Name, err.Message)
	return err
}

func (oc *OpenstackClient) updateAnnotation(cluster *clusterv1.Cluster, machine *clusterv1.Machine, id string) error {
	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[openstack.OpenstackIdAnnotationKey] = id
	instance, _ := oc.instanceExists(cluster, machine)
	ip, err := getIPFromInstance(instance)
	if err != nil {
		return err
	}
	machine.ObjectMeta.Annotations[openstack.OpenstackIPAnnotationKey] = ip
	if err := oc.client.Update(nil, machine); err != nil {
		return err
	}
	return oc.updateInstanceStatus(machine)
}

func (oc *OpenstackClient) requiresUpdate(a *clusterv1.Machine, b *clusterv1.Machine) bool {
	if a == nil || b == nil {
		return true
	}
	// Do not want status changes. Do want changes that impact machine provisioning
	return !reflect.DeepEqual(a.Spec.ObjectMeta, b.Spec.ObjectMeta) ||
		!reflect.DeepEqual(a.Spec.ProviderSpec, b.Spec.ProviderSpec) ||
		!reflect.DeepEqual(a.Spec.Versions, b.Spec.Versions) ||
		a.ObjectMeta.Name != b.ObjectMeta.Name
}

func (oc *OpenstackClient) instanceExists(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (instance *clients.Instance, err error) {
	machineSpec, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, err
	}
	opts := &clients.InstanceListOpts{
		Name:   machine.Name,
		Image:  machineSpec.Image,
		Flavor: machineSpec.Flavor,
	}

	machineService, err := clients.NewInstanceServiceFromMachine(oc.params.KubeClient, cluster.Name, machine)
	if err != nil {
		return nil, err
	}

	instanceList, err := machineService.GetInstanceList(opts)
	if err != nil {
		return nil, err
	}
	if len(instanceList) == 0 {
		return nil, nil
	}
	return instanceList[0], nil
}

func (oc *OpenstackClient) createBootstrapToken(cluster *clusterv1.Cluster) (string, error) {
	token, err := tokenutil.GenerateBootstrapToken()
	if err != nil {
		return "", err
	}

	expiration := time.Now().UTC().Add(TokenTTL)
	tokenSecret, err := bootstrap.GenerateTokenSecret(token, expiration)
	if err != nil {
		panic(fmt.Sprintf("unable to create token. there might be a bug somwhere: %v", err))
	}

	kubeClient, err := clients.GetKubeClient(cluster)
	if err != nil {
		return "", err
	}

	err = kubeClient.Create(context.TODO(), tokenSecret)
	if err != nil {
		return "", err
	}

	return tokenutil.TokenFromIDAndSecret(
		string(tokenSecret.Data[tokenapi.BootstrapTokenIDKey]),
		string(tokenSecret.Data[tokenapi.BootstrapTokenSecretKey]),
	), nil
}

func (oc *OpenstackClient) validateMachine(machine *clusterv1.Machine, config *openstackconfigv1.OpenstackProviderSpec) *apierrors.MachineError {
	// TODO: other validate of openstackCloud
	return nil
}
