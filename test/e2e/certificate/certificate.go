package certificate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterapiv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/proxyagent/agent"
	"open-cluster-management.io/cluster-proxy/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const certificateTestBasename = "certificate"

var _ = Describe("Certificate rotation Test",
	func() {
		f := framework.NewE2EFramework(certificateTestBasename)
		It("Agent certificate's signer should be custom signer",
			func() {
				Eventually(
					func() error {
						By("ManagedClusterAddon should be present firstly")
						addon := &addonapiv1alpha1.ManagedClusterAddOn{}
						if err := f.HubRuntimeClient().Get(context.TODO(), types.NamespacedName{
							Namespace: f.TestClusterName(),
							Name:      common.AddonName,
						}, addon); err != nil {
							return err
						}
						By("A csr with custom signer should be issued")
						csrList := &certificatesv1.CertificateSigningRequestList{}
						err := f.HubRuntimeClient().List(context.TODO(), csrList, client.MatchingLabels{
							addonapiv1alpha1.AddonLabelKey:   common.AddonName,
							clusterapiv1.ClusterNameLabelKey: f.TestClusterName(),
						})
						if err != nil {
							return err
						}
						if len(csrList.Items) == 0 {
							return fmt.Errorf("no csr created")
						}
						exists := false
						for _, csr := range csrList.Items {
							if csr.Spec.SignerName == agent.ProxyAgentSignerName {
								exists = true
							}
						}
						Expect(exists).Should(BeTrue())

						By("Agent secret should be created (after CSR approval)")
						agentSecret := &corev1.Secret{}
						err = f.HubRuntimeClient().Get(context.TODO(), types.NamespacedName{
							Namespace: addon.Status.Namespace,
							Name:      agent.AgentSecretName,
						}, agentSecret)
						if err != nil {
							return err
						}
						return nil
					}).
					WithTimeout(time.Minute).
					WithPolling(time.Second * 10).
					Should(Succeed())
			})

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
