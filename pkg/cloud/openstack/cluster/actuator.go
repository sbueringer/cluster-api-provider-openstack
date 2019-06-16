package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/patch"

	"gopkg.in/yaml.v2"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"k8s.io/klog"
	providerv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	providerv1openstack "sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack/clients"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

const (
	CloudsSecretKey = "clouds.yaml"
	CaSecretKey     = "cacert"
)

// Actuator controls cluster related infrastructure.
type Actuator struct {
	params providerv1openstack.ActuatorParams
}

// NewActuator creates a new Actuator
func NewActuator(params providerv1openstack.ActuatorParams) (*Actuator, error) {
	res := &Actuator{params: params}
	return res, nil
}

// Reconcile creates or applies updates to the cluster.
func (a *Actuator) Reconcile(cluster *clusterv1.Cluster) error {
	if cluster == nil {
		return fmt.Errorf("the cluster is nil, check your cluster configuration")
	}

	// ClusterCopy is used for patch generation during storeCluster
	clusterCopy := cluster.DeepCopy()

	klog.Infof("Reconciling cluster %v.", cluster.Name)
	clusterName := fmt.Sprintf("%s-%s", cluster.Namespace, cluster.Name)

	clusterMachines, err := a.params.ClusterClient.Machines(cluster.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve machines in cluster %q", cluster.Name)
	}

	certificateService, err := clients.NewCertificateService()
	if err != nil {
		return err
	}
	client, err := a.getNetworkClient(cluster)
	if err != nil {
		return err
	}
	networkService, err := clients.NewNetworkService(client)
	if err != nil {
		return err
	}

	secGroupService, err := clients.NewSecGroupService(client)
	if err != nil {
		return err
	}

	addonService, err := clients.NewAddonService()
	if err != nil {
		return err
	}

	// Load provider config.
	desired, err := providerv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return errors.Errorf("failed to load cluster provider spec: %v", err)
	}

	// Load provider status.
	status, err := providerv1.ClusterStatusFromProviderStatus(cluster.Status.ProviderStatus)
	if err != nil {
		return errors.Errorf("failed to load cluster provider status: %v", err)
	}

	defer func() {
		if err := a.storeCluster(cluster, clusterCopy, desired, status); err != nil {
			klog.Errorf("failed to store provider status for cluster %q in namespace %q: %v", cluster.Name, cluster.Namespace, err)
		}
	}()

	err = certificateService.ReconcileCertificates(clusterName, desired)
	if err != nil {
		return errors.Errorf("failed to reconcile certificates: %v", err)
	}

	err = networkService.ReconcileNetwork(clusterName, *desired, status)
	if err != nil {
		return errors.Errorf("failed to reconcile network: %v", err)
	}

	err = networkService.ReconcileLoadBalancers(clusterName, *desired, status, clusterMachines)
	if err != nil {
		return errors.Errorf("failed to reconcile network: %v", err)
	}

	err = secGroupService.Reconcile(clusterName, *desired, status)
	if err != nil {
		return errors.Errorf("failed to reconcile security groups: %v", err)
	}

	err = addonService.ReconcileAddons(cluster, desired, status)
	if err != nil {
		return errors.Errorf("failed to reconcile addons: %v", err)
	}

	return nil
}

// Delete deletes a cluster and is invoked by the Cluster Controller
func (a *Actuator) Delete(cluster *clusterv1.Cluster) error {
	klog.Infof("Deleting cluster %v.", cluster.Name)

	client, err := a.getNetworkClient(cluster)
	if err != nil {
		return err
	}
	_, err = clients.NewNetworkService(client)
	if err != nil {
		return err
	}

	secGroupService, err := clients.NewSecGroupService(client)
	if err != nil {
		return err
	}

	// Load provider config.
	_, err = providerv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return errors.Errorf("failed to load cluster provider config: %v", err)
	}

	// Load provider status.
	providerStatus, err := providerv1.ClusterStatusFromProviderStatus(cluster.Status.ProviderStatus)
	if err != nil {
		return errors.Errorf("failed to load cluster provider status: %v", err)
	}

	// Delete other things

	if providerStatus.GlobalSecurityGroup != nil {
		klog.Infof("Deleting global security group %q", providerStatus.GlobalSecurityGroup.Name)
		err := secGroupService.Delete(providerStatus.GlobalSecurityGroup)
		if err != nil {
			return errors.Errorf("failed to delete security group: %v", err)
		}
	}

	if providerStatus.ControlPlaneSecurityGroup != nil {
		klog.Infof("Deleting control plane security group %q", providerStatus.ControlPlaneSecurityGroup.Name)
		err := secGroupService.Delete(providerStatus.ControlPlaneSecurityGroup)
		if err != nil {
			return errors.Errorf("failed to delete security group: %v", err)
		}
	}

	return nil
}

func (a *Actuator) storeCluster(cluster *clusterv1.Cluster, clusterCopy *clusterv1.Cluster, spec *providerv1.OpenstackClusterProviderSpec, status *providerv1.OpenstackClusterProviderStatus) error {

	ext, err := providerv1.EncodeClusterSpec(spec)
	if err != nil {
		return fmt.Errorf("failed to update cluster spec for cluster %q in namespace %q: %v", cluster.Name, cluster.Namespace, err)
	}
	newStatus, err := providerv1.EncodeClusterStatus(status)
	if err != nil {
		return fmt.Errorf("failed to update cluster status for cluster %q in namespace %q: %v", cluster.Name, cluster.Namespace, err)
	}

	cluster.Spec.ProviderSpec.Value = ext

	// Build a patch and marshal that patch to something the client will understand.
	p, err := patch.NewJSONPatch(clusterCopy, cluster)
	if err != nil {
		return fmt.Errorf("failed to create new JSONPatch: %v", err)
	}

	clusterClient := a.params.ClusterClient.Clusters(cluster.Namespace)

	// Do not update Cluster if nothing has changed
	if len(p) != 0 {
		pb, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to json marshal patch: %v", err)
		}
		klog.Infof("Patching cluster %s", cluster.Name)
		result, err := clusterClient.Patch(cluster.Name, types.JSONPatchType, pb)
		if err != nil {
			return fmt.Errorf("failed to patch cluster: %v", err)
		}
		// Keep the resource version updated so the status update can succeed
		cluster.ResourceVersion = result.ResourceVersion
	}

	// Check if API endpoints is not set or has changed.
	// TOCLARIFY why append in aws instead of replace?
	if status.Network != nil && status.Network.APIServerLoadBalancer != nil &&
		(cluster.Status.APIEndpoints == nil || cluster.Status.APIEndpoints[0].Host != status.Network.APIServerLoadBalancer.IP) {
		cluster.Status.APIEndpoints = []clusterv1.APIEndpoint{
			{
				Host: status.Network.APIServerLoadBalancer.IP,
				Port: status.Network.APIServerLoadBalancer.Port,
			},
		}
	}
	cluster.Status.ProviderStatus = newStatus

	if !reflect.DeepEqual(cluster.Status, clusterCopy.Status) {
		klog.Infof("Updating cluster status %s", cluster.Name)
		if _, err := clusterClient.UpdateStatus(cluster); err != nil {
			return fmt.Errorf("failed to update cluster status: %v", err)
		}
	}

	return nil
}

func GetCloudFromSecret(kubeClient kubernetes.Interface, namespace string, secretName string, cloudName string) (clientconfig.Cloud, []byte, error) {
	emptyCloud := clientconfig.Cloud{}

	if secretName == "" {
		return emptyCloud, nil, nil
	}

	if secretName != "" && cloudName == "" {
		return emptyCloud, nil, fmt.Errorf("Secret name set to %v but no cloud was specified. Please set cloud_name in your machine spec.", secretName)
	}

	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return emptyCloud, nil, err
	}

	content, ok := secret.Data[CloudsSecretKey]
	if !ok {
		return emptyCloud, nil, fmt.Errorf("OpenStack credentials secret %v did not contain key %v",
			secretName, CloudsSecretKey)
	}
	var clouds clientconfig.Clouds
	err = yaml.Unmarshal(content, &clouds)
	if err != nil {
		return emptyCloud, nil, fmt.Errorf("failed to unmarshal clouds credentials stored in secret %v: %v", secretName, err)
	}

	// get cacert
	cacert, ok := secret.Data[CaSecretKey]
	if !ok {
		return emptyCloud, nil, err
	}

	return clouds.Clouds[cloudName], cacert, nil
}

// getNetworkClient returns an gophercloud.ServiceClient provided by openstack.NewNetworkV2
// TODO(chrigl) currently ignoring cluster, but in the future we might store OS-Credentials
// as secrets referenced by the cluster.
// See https://github.com/kubernetes-sigs/cluster-api-provider-openstack/pull/136
func (a *Actuator) getNetworkClient(cluster *clusterv1.Cluster) (*gophercloud.ServiceClient, error) {
	kubeClient := a.params.KubeClient
	clusterSpec, err := providerv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return nil, errors.Errorf("failed to load cluster provider spec: %v", err)
	}
	cloud := clientconfig.Cloud{}
	var cacert []byte

	if clusterSpec.CloudsSecret != nil && clusterSpec.CloudsSecret.Name != "" {
		namespace := clusterSpec.CloudsSecret.Namespace
		if namespace == "" {
			namespace = cluster.Namespace
		}
		cloud, cacert, err = GetCloudFromSecret(kubeClient, namespace, clusterSpec.CloudsSecret.Name, clusterSpec.CloudName)
		if err != nil {
			return nil, err
		}
	}

	clientOpts := new(clientconfig.ClientOpts)
	var opts *gophercloud.AuthOptions

	if cloud.AuthInfo != nil {
		clientOpts.AuthInfo = cloud.AuthInfo
		clientOpts.AuthType = cloud.AuthType
		clientOpts.Cloud = cloud.Cloud
		clientOpts.RegionName = cloud.RegionName
	} else {
		clientOpts.Cloud = cluster.Name
		cloud.Cloud = cluster.Name
	}

	opts, err = clientconfig.AuthOptions(clientOpts)
	if err != nil {
		return nil, err
	}

	opts.AllowReauth = true

	provider, err := openstack.NewClient(opts.IdentityEndpoint)
	if err != nil {
		return nil, fmt.Errorf("create providerClient err: %v", err)
	}

	config := &tls.Config{}
	cloudFromYaml, err := clientconfig.GetCloudFromYAML(clientOpts)
	if cloudFromYaml.CACertFile != "" {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(cacert)
		config.RootCAs = caCertPool
	}

	config.InsecureSkipVerify = !*cloudFromYaml.Verify
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: config}
	provider.HTTPClient.Transport = transport

	err = openstack.Authenticate(provider, *opts)
	if err != nil {
		return nil, fmt.Errorf("providerClient authentication err: %v", err)
	}

	client, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: clientOpts.RegionName,
	})
	if err != nil {
		return nil, err
	}

	return client, nil
}
