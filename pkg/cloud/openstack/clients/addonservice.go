package clients

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/pkg/errors"
	"io"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	"os"
	"path/filepath"
	"reflect"
	providerv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"strings"
	"text/template"
)

func init() {
	_ = apiextensionsv1.AddToScheme(scheme.Scheme)
}

// CertificateService will generate our certs
type AddonService struct {
}

// NewAddonService returns an instance for the OpenStack Networking API
func NewAddonService() (*AddonService, error) {
	return &AddonService{}, nil
}

var basePath = "/home/fedora/code/gopath/src/github.com/sbueringer/clusterapi-setup/deploy/addons/files"

// ReconcileCertificates generate certificates if none exists.
func (s *AddonService) ReconcileAddons(cluster *clusterv1.Cluster, providerSpec *providerv1.OpenstackClusterProviderSpec, providerStatus *providerv1.OpenstackClusterProviderStatus, ) error {

	cfg, err := NewKubeconfig(cluster)
	if err != nil {
		return errors.Wrap(err, "failed to generate a kubeconfig")
	}

	mgr, err := manager.New(cfg, manager.Options{})
	if err != nil {
		return fmt.Errorf("unable to create manager for restConfig: %v", err)
	}

	return filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && strings.HasSuffix(path, ".yaml") {
			err = applyK8sResourcesFromYaml(cluster, providerSpec, providerStatus, mgr.GetClient(), path)
			if err != nil {
				klog.Warningf("cannot apply objects from file %s: %v\n", path, err)
			}
		}
		return nil
	})
}

// applyK8sResourcesFromYaml runs applyK8sResource() for each k8s object of a multi-document yaml
func applyK8sResourcesFromYaml(cluster *clusterv1.Cluster, providerSpec *providerv1.OpenstackClusterProviderSpec, providerStatus *providerv1.OpenstackClusterProviderStatus, kubeClient client.Client, filename string) error {

	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("cannot open file %s: %v", filename, err)
	}

	reader := utilyaml.NewYAMLReader(bufio.NewReader(file))
	for {
		// Read one YAML document at a time, until io.EOF is returned
		b, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			//return nil, err
			return fmt.Errorf("unable to read YAML file: %v", err)
		}
		if len(b) == 0 {
			break
		}

		templatedObject, err := applyTemplate(cluster, providerSpec, providerStatus, string(b))
		if err != nil {
			return err
		}

		k8sObject, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(templatedObject), nil, nil)
		if err != nil {
			fmt.Printf("unable to deserialize YAML file: %v", err)
		}

		err = applyK8sResource(kubeClient, k8sObject)
		if err != nil {
			return fmt.Errorf("cannot apply object: %v", err)
		}
	}
	return nil
}

type params struct {
	Cluster        *clusterv1.Cluster
	ProviderSpec   *providerv1.OpenstackClusterProviderSpec
	ProviderStatus *providerv1.OpenstackClusterProviderStatus
}

func applyTemplate(cluster *clusterv1.Cluster, providerSpec *providerv1.OpenstackClusterProviderSpec, providerStatus *providerv1.OpenstackClusterProviderStatus, object string) (string, error) {
	params := params{
		Cluster:        cluster,
		ProviderSpec:   providerSpec,
		ProviderStatus: providerStatus,
	}

	startUpScript := template.Must(template.New("object").Parse(object))

	var buf bytes.Buffer
	if err := startUpScript.Execute(&buf, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// applyK8sResource applies a k8s resource with 10 retries
func applyK8sResource(kubeClient client.Client, k8sObject runtime.Object) error {

	accessor := meta.NewAccessor()
	name, err := accessor.Name(k8sObject)
	if err != nil {
		return fmt.Errorf("error retrieving name from resource %v: %v", k8sObject, err)
	}

	klog.Infof("Applying resource %s/%s", k8sObject.GetObjectKind().GroupVersionKind().String(), name)

	// try to get resource
	objectKey, err := client.ObjectKeyFromObject(k8sObject)
	if err != nil {
		return fmt.Errorf("error creating object key from resource %s: %v", name, err)
	}

	return wait.ExponentialBackoff(retry.DefaultRetry, func() (bool, error) {
		var found unstructured.Unstructured
		found.SetGroupVersionKind(k8sObject.GetObjectKind().GroupVersionKind())
		err = kubeClient.Get(context.TODO(), objectKey, &found)

		// error executing get
		if err != nil && !apiErrors.IsNotFound(err) {
			return false, fmt.Errorf("error getting resource %s: %v", name, err)
		}

		// update resource if found
		if err == nil {
			err = accessor.SetResourceVersion(k8sObject, found.GetResourceVersion())
			if err != nil {
				return false, fmt.Errorf("error setting resourceVersion on %s: %v", name, err)
			}
			if reflect.DeepEqual(found, k8sObject) {
				klog.Infof("Resource %s is already up-to-date", name)
				return true, nil
			}

			err = kubeClient.Update(context.TODO(), k8sObject)
			if err != nil {
				// this happens e.g. when we try to change an immutable field
				// in this case we just deleted the resource and retry the create
				if apiErrors.IsInvalid(err) {
					err = kubeClient.Delete(context.TODO(), found.DeepCopyObject(), client.PropagationPolicy(metav1.DeletePropagationBackground))
					if err != nil {
						return false, fmt.Errorf("error deleting resource %s: %v", name, err)
					}
					klog.Infof("Deleted resource %s", name)
					return false, fmt.Errorf("error updating resource %s because it was invalid (so we deleted the resource)", name)
				}
				if apiErrors.IsConflict(err) {
					return false, fmt.Errorf("detected conflict error when updating resource %s: %v", name, err)
				}
				return false, fmt.Errorf("error updating resource %s: %v", name, err)
			}
			klog.Infof("Updated resource %s", name)
			return true, nil
		}

		// create resource otherwise
		// create calls with resourceVersion != "" are not allowed, so we clear it
		err = accessor.SetResourceVersion(k8sObject, "")
		if err != nil {
			return false, fmt.Errorf("error setting resourceVersion empty: %v", err)
		}
		err = kubeClient.Create(context.TODO(), k8sObject)
		if err != nil {
			return false, fmt.Errorf("error creating resource %s: %v", name, err)
		}
		klog.Infof("Created resource %s", name)
		return true, nil
	})
}
