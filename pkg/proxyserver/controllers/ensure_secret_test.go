package controllers

import (
	"crypto/x509"
	"testing"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	"open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
)

func TestEnsureSecretRotation(t *testing.T) {
	receivingServiceNamespace := ""
	receivingSANs := make([]string, 0)

	r := &ManagedProxyConfigurationReconciler{
		newCertRotatorFunc: func(namespace, name string, sans ...string) selfsigned.CertRotation {
			receivingServiceNamespace = namespace
			receivingSANs = sans
			return dummyRotator{}
		},
		CAPair: &openshiftcrypto.CA{
			Config: &openshiftcrypto.TLSCertificateConfig{},
		},
	}
	expectedEntrypoint := "foo"
	expectedServiceName := "tik"
	expectedNamespace := "bar"
	cfg := &v1alpha1.ManagedProxyConfiguration{
		Spec: v1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: v1alpha1.ManagedProxyConfigurationProxyServer{
				InClusterServiceName: expectedServiceName,
				Namespace:            expectedNamespace,
				Entrypoint: &v1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
					Type: v1alpha1.EntryPointTypeHostname,
					Hostname: &v1alpha1.EntryPointHostname{
						Value: "example.com",
					},
				},
			},
		},
	}
	err := r.ensureRotation(cfg, expectedEntrypoint)

	assert.NoError(t, err)
	assert.Equal(t, expectedNamespace, receivingServiceNamespace)
	assert.Equal(t, []string{
		"127.0.0.1",
		"localhost",
		"tik.bar",
		"tik.bar.svc",
		"example.com",
		"foo",
	}, receivingSANs)
}

var _ selfsigned.CertRotation = &dummyRotator{}

type dummyRotator struct{}

func (d dummyRotator) EnsureTargetCertKeyPair(
	signingCertKeyPair *openshiftcrypto.CA,
	caBundleCerts []*x509.Certificate,
	fns ...openshiftcrypto.CertificateExtensionFunc) error {
	return nil
}
