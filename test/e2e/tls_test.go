package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TLS Profile Test runs serially (after all parallel tests complete) because it
// creates/modifies the ocm-tls-profile ConfigMap, which triggers the cluster-proxy
// deployment to restart with new TLS settings. Running this concurrently with other
// tests (especially connectivity tests) would cause those tests to fail when the
// proxy restarts. The test properly cleans up by restoring the original ConfigMap
// state in AfterEach.
var _ = Describe("TLS Profile Test", Serial, Label("tls", "profile", "configuration"),
	func() {
		const tlsConfigMapName = "ocm-tls-profile"
		const namespace = "open-cluster-management-addon"

		var (
			originalConfigMapData map[string]string
			configMapExisted      bool
		)

		BeforeEach(func() {
			// Reset variables for clean state
			originalConfigMapData = nil
			configMapExisted = false

			By("Saving original TLS ConfigMap state")
			existingConfigMap, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), tlsConfigMapName, metav1.GetOptions{})
			if err == nil {
				// ConfigMap exists, save its data
				configMapExisted = true
				originalConfigMapData = make(map[string]string)
				for k, v := range existingConfigMap.Data {
					originalConfigMapData[k] = v
				}
				fmt.Fprintf(GinkgoWriter, "[INFO] Saved original TLS ConfigMap: %v\n", originalConfigMapData)
			} else {
				// ConfigMap doesn't exist
				configMapExisted = false
				originalConfigMapData = nil
				fmt.Fprintf(GinkgoWriter, "[INFO] TLS ConfigMap does not exist before test\n")
			}
		})

		AfterEach(func() {
			By("Restoring original TLS ConfigMap state")
			if configMapExisted {
				// Restore original ConfigMap data
				configMap, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), tlsConfigMapName, metav1.GetOptions{})
				if err == nil {
					configMap.Data = originalConfigMapData
					_, err = hubKubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), configMap, metav1.UpdateOptions{})
					Expect(err).ToNot(HaveOccurred())
					fmt.Fprintf(GinkgoWriter, "[INFO] Restored original TLS ConfigMap: %v\n", originalConfigMapData)
				}
			} else {
				// ConfigMap didn't exist before, delete it
				err := hubKubeClient.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), tlsConfigMapName, metav1.DeleteOptions{})
				if err == nil {
					fmt.Fprintf(GinkgoWriter, "[INFO] Deleted TLS ConfigMap (it didn't exist before test)\n")
				}
			}

			By("Waiting for deployment to stabilize after ConfigMap cleanup")
			Eventually(func() bool {
				deploy := &appsv1.Deployment{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: namespace,
					Name:      "cluster-proxy",
				}, deploy)
				if err != nil {
					return false
				}
				return deploy.Status.AvailableReplicas >= 1 && deploy.Status.ReadyReplicas >= 1
			}).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"Deployment should stabilize after ConfigMap cleanup")
		})

		It("should apply TLS 1.2 with cipher suites from ConfigMap", Label("tls", "flags", "tls12"), func() {
			By("Creating TLS ConfigMap with TLS 1.2 and cipher suites")
			tlsConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tlsConfigMapName,
					Namespace: namespace,
				},
				Data: map[string]string{
					"minTLSVersion": "VersionTLS12",
					"cipherSuites":  "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
				},
			}

			if configMapExisted {
				existingConfigMap, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), tlsConfigMapName, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				existingConfigMap.Data = tlsConfigMap.Data
				_, err = hubKubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), existingConfigMap, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Updated ConfigMap with TLS 1.2 config\n")
			} else {
				_, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Create(context.TODO(), tlsConfigMap, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Created ConfigMap with TLS 1.2 config\n")
			}

			By("Waiting for deployment to have cipher suites from ConfigMap")
			expectedCipherSuites := "--cipher-suites=TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"
			var proxyServerDeploy *appsv1.Deployment
			Eventually(func() bool {
				deploy := &appsv1.Deployment{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: namespace,
					Name:      "cluster-proxy",
				}, deploy)
				if err != nil {
					return false
				}

				// Wait for cipher suites flag specifically (TLS 1.2 is the default, so can't rely on that)
				for _, container := range deploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-server" {
						for _, arg := range container.Args {
							if arg == expectedCipherSuites {
								proxyServerDeploy = deploy
								fmt.Fprintf(GinkgoWriter, "[DEBUG] Found cipher suites in deployment\n")
								return true
							}
						}
					}
				}
				return false
			}).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"Deployment should have cipher suites from ConfigMap")

			By("Verifying both TLS version and cipher suites are present")
			Expect(proxyServerDeploy).NotTo(BeNil())

			var proxyServerContainer *corev1.Container
			for i := range proxyServerDeploy.Spec.Template.Spec.Containers {
				if proxyServerDeploy.Spec.Template.Spec.Containers[i].Name == "proxy-server" {
					proxyServerContainer = &proxyServerDeploy.Spec.Template.Spec.Containers[i]
					break
				}
			}
			Expect(proxyServerContainer).NotTo(BeNil())

			args := proxyServerContainer.Args
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Proxy-server args: %v\n", args)

			hasMinVersion := false
			hasCipherSuites := false
			for _, arg := range args {
				if arg == "--tls-min-version=VersionTLS12" {
					hasMinVersion = true
				}
				if arg == expectedCipherSuites {
					hasCipherSuites = true
					fmt.Fprintf(GinkgoWriter, "[DEBUG] Found cipher suites: %s\n", arg)
				}
			}

			Expect(hasMinVersion).To(BeTrue(), "Expected --tls-min-version=VersionTLS12")
			Expect(hasCipherSuites).To(BeTrue(), "Expected %s", expectedCipherSuites)
		})

		It("should apply TLS 1.3 with cipher suites from ConfigMap", Label("tls", "flags", "tls13"), func() {
			By("Creating TLS ConfigMap with TLS 1.3 and cipher suites")
			tlsConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tlsConfigMapName,
					Namespace: namespace,
				},
				Data: map[string]string{
					"minTLSVersion": "VersionTLS13",
					"cipherSuites":  "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384",
				},
			}

			if configMapExisted {
				existingConfigMap, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), tlsConfigMapName, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				existingConfigMap.Data = tlsConfigMap.Data
				_, err = hubKubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), existingConfigMap, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Updated ConfigMap with TLS 1.3 config\n")
			} else {
				_, err := hubKubeClient.CoreV1().ConfigMaps(namespace).Create(context.TODO(), tlsConfigMap, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Created ConfigMap with TLS 1.3 config\n")
			}

			By("Waiting for deployment to have TLS 1.3 config")
			var proxyServerDeploy *appsv1.Deployment
			Eventually(func() bool {
				deploy := &appsv1.Deployment{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: namespace,
					Name:      "cluster-proxy",
				}, deploy)
				if err != nil {
					return false
				}

				for _, container := range deploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-server" {
						for _, arg := range container.Args {
							if arg == "--tls-min-version=VersionTLS13" {
								proxyServerDeploy = deploy
								fmt.Fprintf(GinkgoWriter, "[DEBUG] Found TLS 1.3 in deployment\n")
								return true
							}
						}
					}
				}
				return false
			}).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"Deployment should have --tls-min-version=VersionTLS13")

			By("Verifying both TLS version and cipher suites are present")
			Expect(proxyServerDeploy).NotTo(BeNil())

			var proxyServerContainer *corev1.Container
			for i := range proxyServerDeploy.Spec.Template.Spec.Containers {
				if proxyServerDeploy.Spec.Template.Spec.Containers[i].Name == "proxy-server" {
					proxyServerContainer = &proxyServerDeploy.Spec.Template.Spec.Containers[i]
					break
				}
			}
			Expect(proxyServerContainer).NotTo(BeNil())

			args := proxyServerContainer.Args
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Proxy-server args: %v\n", args)

			hasMinVersion := false
			expectedCipherSuites := "--cipher-suites=TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384"
			hasCipherSuites := false
			for _, arg := range args {
				if arg == "--tls-min-version=VersionTLS13" {
					hasMinVersion = true
				}
				if arg == expectedCipherSuites {
					hasCipherSuites = true
					fmt.Fprintf(GinkgoWriter, "[DEBUG] Found cipher suites: %s\n", arg)
				}
			}

			Expect(hasMinVersion).To(BeTrue(), "Expected --tls-min-version=VersionTLS13")
			Expect(hasCipherSuites).To(BeTrue(), "Expected %s", expectedCipherSuites)
		})
	})
