package controllers

import (
	"crypto/x509"
	"testing"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	"open-cluster-management.io/cluster-proxy/pkg/addon/operator/authentication/selfsigned"
	"open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

func TestEnsureSecretRotation(t *testing.T) {
	receivingServiceNamespace := ""
	receivingSANs := make([]string, 0)

	r := &ClusterManagementAddonReconciler{
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
			},
		},
	}
	err := r.ensureRotation(cfg, expectedEntrypoint)

	assert.NoError(t, err)
	assert.Equal(t, expectedNamespace, receivingServiceNamespace)
	assert.Equal(t, []string{
		"127.0.0.1",
		"localhost",
		"foo",
		"tik.bar",
		"tik.bar.svc",
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
