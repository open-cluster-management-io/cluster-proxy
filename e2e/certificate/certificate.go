package certificate

import (
	"context"
	"crypto/x509"
	"encoding/pem"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

const certificateTestBasename = "certificate"

var _ = Describe("Certificate rotation Test",
	func() {
		f := framework.NewE2EFramework(certificateTestBasename)

		It("Certificate SAN customizing should work",
			func() {
				c := f.HubRuntimeClient()
				proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
				err := c.Get(context.TODO(), types.NamespacedName{
					Name: "cluster-proxy",
				}, proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				expectedSAN := "foo"
				proxyConfiguration.Spec.Authentication.Signer.SelfSigned = &proxyv1alpha1.AuthenticationSelfSigned{}
				proxyConfiguration.Spec.Authentication.Signer.SelfSigned.AdditionalSANs = []string{
					expectedSAN,
				}
				err = c.Update(context.TODO(), proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				Eventually(
					func() (bool, error) {
						signedNames, err := extractSANsFromSecret(
							f.HubNativeClient(),
							proxyConfiguration.Spec.ProxyServer.Namespace,
							proxyConfiguration.Spec.Authentication.Dump.Secrets.SigningAgentServerSecretName)
						if err != nil {
							return false, err
						}
						return contains(signedNames, expectedSAN), nil
					})
				Eventually(
					func() (bool, error) {
						signedNames, err := extractSANsFromSecret(
							f.HubNativeClient(),
							proxyConfiguration.Spec.ProxyServer.Namespace,
							proxyConfiguration.Spec.Authentication.Dump.Secrets.SigningProxyServerSecretName)
						if err != nil {
							return false, err
						}
						return contains(signedNames, expectedSAN), nil
					})
			})
	})

func extractSANsFromSecret(c kubernetes.Interface, namespace, name string) ([]string, error) {
	agentServerSecret, err := c.CoreV1().
		Secrets(namespace).
		Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	certData, ok := agentServerSecret.Data["tls.crt"]
	if !ok {
		return nil, nil
	}
	pemBlock, _ := pem.Decode(certData)
	cert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return cert.DNSNames, nil // TODO: add IP SANs
}

func contains(values []string, target string) bool {
	exists := false
	for _, v := range values {
		exists = exists || (v == target)
	}
	return exists
}
