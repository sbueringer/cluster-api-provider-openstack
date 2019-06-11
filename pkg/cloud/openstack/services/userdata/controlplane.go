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

package userdata

import (
	"encoding/base64"
	"strings"

	"github.com/pkg/errors"
)

const (
	controlPlaneIgnition = `
    - path: /etc/kubernetes/pki/ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .CACert | Indent 10}}

    - path: /etc/kubernetes/pki/ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .CAKey | Indent 10}}

    - path: /etc/kubernetes/pki/etcd/ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .EtcdCACert | Indent 10}}

    - path: /etc/kubernetes/pki/etcd/ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .EtcdCAKey | Indent 10}}

    - path: /etc/kubernetes/pki/front-proxy-ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .FrontProxyCACert | Indent 10}}

    - path: /etc/kubernetes/pki/front-proxy-ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .FrontProxyCAKey | Indent 10}}

    - path: /etc/kubernetes/pki/sa.pub
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .SaCert | Indent 10}}

    - path: /etc/kubernetes/pki/sa.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .SaKey | Indent 10}}

    - path: /etc/kubernetes/kubeadm_config.yaml
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640  
      contents:
        inline: |
{{.ClusterConfiguration | Indent 10}}
          ---
{{.InitConfiguration | Indent 10}}
`

	controlPlaneJoinIgnition = `
    - path: /etc/kubernetes/pki/ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .CACert | Indent 10}}

    - path: /etc/kubernetes/pki/ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .CAKey | Indent 10}}

    - path: /etc/kubernetes/pki/etcd/ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .EtcdCACert | Indent 10}}

    - path: /etc/kubernetes/pki/etcd/ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .EtcdCAKey | Indent 10}}

    - path: /etc/kubernetes/pki/front-proxy-ca.crt
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .FrontProxyCACert | Indent 10}}

    - path: /etc/kubernetes/pki/front-proxy-ca.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .FrontProxyCAKey | Indent 10}}

    - path: /etc/kubernetes/pki/sa.pub
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640
      contents:
        inline: |
{{ .SaCert | Indent 10}}

    - path: /etc/kubernetes/pki/sa.key
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0600
      contents:
        inline: |
{{ .SaKey | Indent 10}}

    - path: /etc/kubernetes/kubeadm_config.yaml
      filesystem: root
      user:
        id: 0
      group:
        id: 0
      mode: 0640  
      contents:
        inline: |
{{.JoinConfiguration | Indent 10}}
`
)

func isKeyPairValid(cert, key string) bool {
	return cert != "" && key != ""
}

// ControlPlaneInput defines the context to generate a controlplane instance user data.
type ControlPlaneInput struct {
	CACert               string
	CAKey                string
	EtcdCACert           string
	EtcdCAKey            string
	FrontProxyCACert     string
	FrontProxyCAKey      string
	SaCert               string
	SaKey                string
	ClusterConfiguration string
	InitConfiguration    string
}

// ContolPlaneJoinInput defines context to generate controlplane instance user data for controlplane node join.
type ContolPlaneJoinInput struct {
	CACert            string
	CAKey             string
	EtcdCACert        string
	EtcdCAKey         string
	FrontProxyCACert  string
	FrontProxyCAKey   string
	SaCert            string
	SaKey             string
	BootstrapToken    string
	ELBAddress        string
	JoinConfiguration string
}

func (cpi *ControlPlaneInput) validateCertificates() error {
	if !isKeyPairValid(cpi.CACert, cpi.CAKey) {
		return errors.New("CA cert material in the ControlPlaneInput is missing cert/key")
	}

	if !isKeyPairValid(cpi.EtcdCACert, cpi.EtcdCAKey) {
		return errors.New("ETCD CA cert material in the ControlPlaneInput is  missing cert/key")
	}

	if !isKeyPairValid(cpi.FrontProxyCACert, cpi.FrontProxyCAKey) {
		return errors.New("FrontProxy CA cert material in ControlPlaneInput is  missing cert/key")
	}

	if !isKeyPairValid(cpi.SaCert, cpi.SaKey) {
		return errors.New("ServiceAccount cert material in ControlPlaneInput is  missing cert/key")
	}

	return nil
}

func (cpi *ContolPlaneJoinInput) validateCertificates() error {
	if !isKeyPairValid(cpi.CACert, cpi.CAKey) {
		return errors.New("CA cert material in the ContolPlaneJoinInput is  missing cert/key")
	}

	if !isKeyPairValid(cpi.EtcdCACert, cpi.EtcdCAKey) {
		return errors.New("ETCD cert material in the ContolPlaneJoinInput is  missing cert/key")
	}

	if !isKeyPairValid(cpi.FrontProxyCACert, cpi.FrontProxyCAKey) {
		return errors.New("FrontProxy cert material in ContolPlaneJoinInput is  missing cert/key")
	}

	if !isKeyPairValid(cpi.SaCert, cpi.SaKey) {
		return errors.New("ServiceAccount cert material in ContolPlaneJoinInput is  missing cert/key")
	}

	return nil
}

// NewControlPlane returns the user data string to be used on a controlplane instance.
func NewControlPlane(input *ControlPlaneInput) (string, error) {
	if err := input.validateCertificates(); err != nil {
		return "", errors.Wrapf(err, "ControlPlaneInput is invalid")
	}

	fMap := map[string]interface{}{
		"Base64Encode": templateBase64Encode,
		"Indent":       templateYAMLIndent,
	}

	userData, err := generateWithFuncs("controlplane", controlPlaneIgnition, funcMap(fMap), input)
	if err != nil {
		return "", errors.Wrapf(err, "failed to generate user data for new control plane machine")
	}

	return userData, err
}

// JoinControlPlane returns the user data string to be used on a new contrplplane instance.
func JoinControlPlane(input *ContolPlaneJoinInput) (string, error) {
	if err := input.validateCertificates(); err != nil {
		return "", errors.Wrapf(err, "ControlPlaneInput is invalid")
	}

	fMap := map[string]interface{}{
		"Base64Encode": templateBase64Encode,
		"Indent":       templateYAMLIndent,
	}

	userData, err := generateWithFuncs("controlplane", controlPlaneJoinIgnition, funcMap(fMap), input)
	if err != nil {
		return "", errors.Wrapf(err, "failed to generate user data for machine joining control plane")
	}
	return userData, err
}

func templateBase64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func templateYAMLIndent(i int, input string) string {
	split := strings.Split(input, "\n")
	ident := "\n" + strings.Repeat(" ", i)
	return strings.Repeat(" ", i) + strings.Join(split, ident)
}
