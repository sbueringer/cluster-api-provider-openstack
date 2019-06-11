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

// Copied from https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/8818da08964e48f0fddd16d12265ec3b86d1e7cb/pkg/cloud/aws/services/certificates/certificates.go
package clients

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog"
	"math"
	"math/big"
	"net"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	openstackconfigv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	providerv1 "sigs.k8s.io/cluster-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	rsaKeySize     = 2048
	duration365d   = time.Hour * 24 * 365
	clusterCA      = "cluster-ca"
	etcdCA         = "etcd-ca"
	frontProxyCA   = "front-proxy-ca"
	serviceAccount = "service-account"
)

// CertificateService will generate our certs
type CertificateService struct {
}

// NewNetworkService returns an instance for the OpenStack Networking API
func NewCertificateService() (*CertificateService, error) {
	return &CertificateService{}, nil
}

// NewPrivateKey creates an RSA private key
func NewPrivateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, rsaKeySize)
}

// AltNames contains the domain names and IP addresses that will be added
// to the API Server's x509 certificate SubAltNames field. The values will
// be passed directly to the x509.Certificate object.
type AltNames struct {
	DNSNames []string
	IPs      []net.IP
}

// Config contains the basic fields required for creating a certificate
type Config struct {
	CommonName   string
	Organization []string
	AltNames     AltNames
	Usages       []x509.ExtKeyUsage
}

// ReconcileCertificates generate certificates if none exists.
func (s *CertificateService) ReconcileCertificates(clusterName string, providerSpec *providerv1.OpenstackClusterProviderSpec) error {
	klog.Infof("Reconciling certificates for cluster %s", clusterName)

	if !providerSpec.CAKeyPair.HasCertAndKey() {
		klog.Info("Generating keypair for", "user", clusterCA)
		clusterCAKeyPair, err := generateCACert(&providerSpec.CAKeyPair, clusterCA)
		if err != nil {
			return errors.Wrapf(err, "Failed to generate certs for %q", clusterCA)
		}
		providerSpec.CAKeyPair = clusterCAKeyPair
	}

	if !providerSpec.EtcdCAKeyPair.HasCertAndKey() {
		klog.Info("Generating keypair", "user", etcdCA)
		etcdCAKeyPair, err := generateCACert(&providerSpec.EtcdCAKeyPair, etcdCA)
		if err != nil {
			return errors.Wrapf(err, "Failed to generate certs for %q", etcdCA)
		}
		providerSpec.EtcdCAKeyPair = etcdCAKeyPair
	}
	if !providerSpec.FrontProxyCAKeyPair.HasCertAndKey() {
		klog.Info("Generating keypair", "user", frontProxyCA)
		fpCAKeyPair, err := generateCACert(&providerSpec.FrontProxyCAKeyPair, frontProxyCA)
		if err != nil {
			return errors.Wrapf(err, "Failed to generate certs for %q", frontProxyCA)
		}
		providerSpec.FrontProxyCAKeyPair = fpCAKeyPair
	}

	if !providerSpec.SAKeyPair.HasCertAndKey() {
		klog.Info("Generating service account keys", "user", serviceAccount)
		saKeyPair, err := generateServiceAccountKeys(&providerSpec.SAKeyPair, serviceAccount)
		if err != nil {
			return errors.Wrapf(err, "Failed to generate keyPair for %q", serviceAccount)
		}
		providerSpec.SAKeyPair = saKeyPair
	}
	return nil
}

func generateCACert(kp *v1alpha1.KeyPair, user string) (v1alpha1.KeyPair, error) {
	x509Cert, privKey, err := NewCertificateAuthority()
	if err != nil {
		return v1alpha1.KeyPair{}, errors.Wrapf(err, "failed to generate CA cert for %q", user)
	}
	if kp == nil {
		return v1alpha1.KeyPair{
			Cert: EncodeCertPEM(x509Cert),
			Key:  EncodePrivateKeyPEM(privKey),
		}, nil
	}
	kp.Cert = EncodeCertPEM(x509Cert)
	kp.Key = EncodePrivateKeyPEM(privKey)
	return *kp, nil
}

func generateServiceAccountKeys(kp *v1alpha1.KeyPair, user string) (v1alpha1.KeyPair, error) {
	saCreds, err := NewPrivateKey()
	if err != nil {
		return v1alpha1.KeyPair{}, errors.Wrapf(err, "failed to create service account public and private keys")
	}
	saPub, err := EncodePublicKeyPEM(&saCreds.PublicKey)
	if err != nil {
		return v1alpha1.KeyPair{}, errors.Wrapf(err, "failed to encode service account public key to PEM")
	}
	if kp == nil {
		return v1alpha1.KeyPair{
			Cert: saPub,
			Key:  EncodePrivateKeyPEM(saCreds),
		}, nil
	}
	kp.Cert = saPub
	kp.Key = EncodePrivateKeyPEM(saCreds)
	return *kp, nil
}

// NewSignedCert creates a signed certificate using the given CA certificate and key
func (cfg *Config) NewSignedCert(key *rsa.PrivateKey, caCert *x509.Certificate, caKey *rsa.PrivateKey) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64))
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate random integer for signed cerficate")
	}

	if len(cfg.CommonName) == 0 {
		return nil, errors.New("must specify a CommonName")
	}

	if len(cfg.Usages) == 0 {
		return nil, errors.New("must specify at least one ExtKeyUsage")
	}

	tmpl := x509.Certificate{
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		DNSNames:     cfg.AltNames.DNSNames,
		IPAddresses:  cfg.AltNames.IPs,
		SerialNumber: serial,
		NotBefore:    caCert.NotBefore,
		NotAfter:     time.Now().Add(duration365d).UTC(),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  cfg.Usages,
	}

	b, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create signed certificate: %+v", tmpl)
	}

	return x509.ParseCertificate(b)
}

// NewCertificateAuthority creates new certificate and private key for the certificate authority
func NewCertificateAuthority() (*x509.Certificate, *rsa.PrivateKey, error) {
	key, err := NewPrivateKey()
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to create private key")
	}

	cert, err := NewSelfSignedCACert(key)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to create self-signed certificate")
	}

	return cert, key, nil
}

// NewSelfSignedCACert creates a CA certificate.
func NewSelfSignedCACert(key *rsa.PrivateKey) (*x509.Certificate, error) {
	cfg := Config{
		CommonName: "kubernetes",
	}

	now := time.Now().UTC()

	tmpl := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		NotBefore:             now,
		NotAfter:              now.Add(duration365d * 10),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		MaxPathLenZero:        true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		IsCA:                  true,
	}

	b, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, key.Public(), key)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create self signed CA certificate: %+v", tmpl)
	}

	return x509.ParseCertificate(b)
}

// TODO check if aws variant makes sense
// NewKubeconfig creates a new Kubeconfig where endpoint is the ELB endpoint.
func NewKubeconfigAWS(clusterName, endpoint string, caCert *x509.Certificate, caKey *rsa.PrivateKey) (*api.Config, error) {
	cfg := &Config{
		CommonName:   "kubernetes-admin",
		Organization: []string{"system:masters"},
		Usages:       []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientKey, err := NewPrivateKey()
	if err != nil {
		return nil, errors.Wrap(err, "unable to create private key")
	}

	clientCert, err := cfg.NewSignedCert(clientKey, caCert, caKey)
	if err != nil {
		return nil, errors.Wrap(err, "unable to sign certificate")
	}

	userName := "kubernetes-admin"
	contextName := fmt.Sprintf("%s@%s", userName, clusterName)

	return &api.Config{
		Clusters: map[string]*api.Cluster{
			clusterName: {
				Server:                   endpoint,
				CertificateAuthorityData: EncodeCertPEM(caCert),
			},
		},
		Contexts: map[string]*api.Context{
			contextName: {
				Cluster:  clusterName,
				AuthInfo: userName,
			},
		},
		AuthInfos: map[string]*api.AuthInfo{
			userName: {
				ClientKeyData:         EncodePrivateKeyPEM(clientKey),
				ClientCertificateData: EncodeCertPEM(clientCert),
			},
		},
		CurrentContext: contextName,
	}, nil
}

func GetKubeClient(cluster *clusterv1.Cluster) (client.Client, error) {

	cfg, err := NewKubeconfig(cluster)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate a kubeconfig")
	}

	mgr, err := manager.New(cfg, manager.Options{})
	if err != nil {
		return nil, fmt.Errorf("unable to create manager for restConfig: %v", err)
	}

	return mgr.GetClient(), nil
}

// NewKubeconfig creates a new Kubeconfig where endpoint is the ELB endpoint.
func NewKubeconfig(cluster *clusterv1.Cluster) (*rest.Config, error) {

	// Load provider config.
	config, err := openstackconfigv1.ClusterSpecFromProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return nil, errors.Errorf("failed to load cluster provider status: %v", err)
	}

	cert, err := DecodeCertPEM(config.CAKeyPair.Cert)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode CA Cert")
	} else if cert == nil {
		return nil, errors.New("certificate not found in config")
	}

	key, err := DecodePrivateKeyPEM(config.CAKeyPair.Key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode private key")
	} else if key == nil {
		return nil, errors.New("key not found in status")
	}

	server := fmt.Sprintf("https://%s", config.ClusterConfiguration.ControlPlaneEndpoint)

	cfg := &Config{
		CommonName:   "kubernetes-admin",
		Organization: []string{"system:masters"},
		Usages:       []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientKey, err := NewPrivateKey()
	if err != nil {
		return nil, errors.Wrap(err, "unable to create private key")
	}

	clientCert, err := cfg.NewSignedCert(clientKey, cert, key)
	if err != nil {
		return nil, errors.Wrap(err, "unable to sign certificate")
	}

	return &rest.Config{
		Host: server,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   EncodeCertPEM(cert),
			KeyData:  EncodePrivateKeyPEM(clientKey),
			CertData: EncodeCertPEM(clientCert),
		},
	}, nil
}

// EncodeCertPEM returns PEM-endcoded certificate data.
func EncodeCertPEM(cert *x509.Certificate) []byte {
	block := pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	}
	return pem.EncodeToMemory(&block)
}

// EncodePrivateKeyPEM returns PEM-encoded private key data.
func EncodePrivateKeyPEM(key *rsa.PrivateKey) []byte {
	block := pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}

	return pem.EncodeToMemory(&block)
}

// EncodePublicKeyPEM returns PEM-encoded public key data.
func EncodePublicKeyPEM(key *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return []byte{}, err
	}
	block := pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}
	return pem.EncodeToMemory(&block), nil
}

// DecodeCertPEM attempts to return a decoded certificate or nil
// if the encoded input does not contain a certificate.
func DecodeCertPEM(encoded []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(encoded)
	if block == nil {
		return nil, nil
	}

	return x509.ParseCertificate(block.Bytes)
}

// DecodePrivateKeyPEM attempts to return a decoded key or nil
// if the encoded input does not contain a private key.
func DecodePrivateKeyPEM(encoded []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(encoded)
	if block == nil {
		return nil, nil
	}

	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// GenerateCertificateHash returns the encoded sha256 hash for the certificate provided
func GenerateCertificateHash(encoded []byte) (string, error) {
	cert, err := DecodeCertPEM(encoded)
	if err != nil || cert == nil {
		return "", errors.Errorf("failed to parse PEM block containing the public key")
	}

	certHash := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + strings.ToLower(hex.EncodeToString(certHash[:])), nil
}
