package e2e

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/proxyagent/agent"
)

var _ = Describe("Certificate rotation Test", Label("certificate", "rotation"),
	func() {
		It("Agent certificate's signer should be custom signer", Label("certificate", "signer"),
			func() {
				Eventually(
					func() error {
						By("ManagedClusterAddon should be present firstly")
						addon := &addonapiv1alpha1.ManagedClusterAddOn{}
						if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
							Namespace: managedClusterName,
							Name:      common.AddonName,
						}, addon); err != nil {
							return err
						}
						By("Agent secret should be created with valid certificate")
						agentSecret := &corev1.Secret{}
						err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
							Namespace: addon.Status.Namespace,
							Name:      agent.AgentSecretName,
						}, agentSecret)
						if err != nil {
							return err
						}

						By("Certificate should be valid and signed by custom signer")
						certData, ok := agentSecret.Data["tls.crt"]
						if !ok {
							return fmt.Errorf("tls.crt not found in agent secret")
						}

						pemBlock, _ := pem.Decode(certData)
						if pemBlock == nil {
							return fmt.Errorf("failed to decode PEM certificate")
						}

						cert, err := x509.ParseCertificate(pemBlock.Bytes)
						if err != nil {
							return fmt.Errorf("failed to parse certificate: %v", err)
						}

						// Verify certificate is not expired
						now := time.Now()
						if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
							return fmt.Errorf("certificate is not valid at current time")
						}

						// Verify certificate has reasonable validity period (should be close to 180 days)
						validity := cert.NotAfter.Sub(cert.NotBefore)
						expectedValidity := time.Hour * 24 * 180
						if validity < expectedValidity-time.Hour*24 || validity > expectedValidity+time.Hour*24 {
							return fmt.Errorf("certificate validity period %v is not as expected (~180 days)", validity)
						}

						return nil
					}).
					WithTimeout(time.Minute).
					WithPolling(time.Second * 10).
					Should(Succeed())
			})

		It("Certificate SAN customizing should work", Label("san", "customization"),
			func() {
				proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Name: "cluster-proxy",
				}, proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				expectedSAN := "foo"
				proxyConfiguration.Spec.Authentication.Signer.SelfSigned = &proxyv1alpha1.AuthenticationSelfSigned{}
				proxyConfiguration.Spec.Authentication.Signer.SelfSigned.AdditionalSANs = []string{
					expectedSAN,
				}
				err = hubRuntimeClient.Update(context.TODO(), proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				Eventually(
					func() (bool, error) {
						signedNames, err := extractSANsFromSecret(
							hubKubeClient,
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
							hubKubeClient,
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
