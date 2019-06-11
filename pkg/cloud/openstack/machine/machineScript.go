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
	"bytes"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack/clients"
	"text/template"

	openstackconfigv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

type setupParams struct {
	Cluster     *clusterv1.Cluster
	ClusterSpec *openstackconfigv1.OpenstackClusterProviderSpec
	CACert      string
	CAKey       string

	CloudConf    clients.CloudConf
	KubeadmFiles string

	Machine     *clusterv1.Machine
	MachineSpec *openstackconfigv1.OpenstackProviderSpec

	PodCIDR     string
	ServiceCIDR string
}

func startupScript(machine *clusterv1.Machine, cloudConf clients.CloudConf, kubeadmFiles string, script string) (string, error) {

	params := setupParams{
		Machine:      machine,
		CloudConf:    cloudConf,
		KubeadmFiles: kubeadmFiles,
	}

	startUpScript := template.Must(template.New("startUp").Parse(script))

	var buf bytes.Buffer
	if err := startUpScript.Execute(&buf, params); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Just a temporary hack to grab a single range from the config.
