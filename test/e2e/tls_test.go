package e2e

import (
	"context"
	"fmt"
	"strings"
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
			proxyServerDeploy     *appsv1.Deployment
		)

		BeforeEach(func() {
			// Reset variables for clean state
			originalConfigMapData = nil
			configMapExisted = false
			proxyServerDeploy = nil

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

			By("Creating or updating the ocm-tls-profile ConfigMap with test data")
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
				// Update existing ConfigMap
				existingConfigMap.Data = tlsConfigMap.Data
				_, err = hubKubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), existingConfigMap, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Updated ocm-tls-profile ConfigMap with test data\n")
			} else {
				// Create new ConfigMap
				_, err = hubKubeClient.CoreV1().ConfigMaps(namespace).Create(context.TODO(), tlsConfigMap, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "[INFO] Created ocm-tls-profile ConfigMap with test data\n")
			}

			By("Waiting for proxy-server deployment to update with TLS config")
			Eventually(func() bool {
				deploy := &appsv1.Deployment{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: namespace,
					Name:      "cluster-proxy",
				}, deploy)
				if err != nil {
					fmt.Fprintf(GinkgoWriter, "[DEBUG] Failed to get deployment: %v\n", err)
					return false
				}

				// Check if deployment has the TLS flags in its args
				for _, container := range deploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-server" {
						hasTLSFlag := false
						for _, arg := range container.Args {
							if strings.HasPrefix(arg, "--tls-min-version=") {
								hasTLSFlag = true
								fmt.Fprintf(GinkgoWriter, "[DEBUG] Found TLS flag in deployment: %s\n", arg)
								break
							}
						}
						if hasTLSFlag {
							proxyServerDeploy = deploy
							return true
						}
						fmt.Fprintf(GinkgoWriter, "[DEBUG] TLS flag not yet in deployment args, waiting...\n")
						return false
					}
				}
				fmt.Fprintf(GinkgoWriter, "[DEBUG] proxy-server container not found\n")
				return false
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"Deployment should be updated with --tls-min-version flag within 2 minutes")
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
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"Deployment should stabilize after ConfigMap cleanup")
		})

		It("Proxy server deployment should have TLS flags when TLS profile exists", Label("tls", "flags"), func() {
			By("Verifying proxy-server container args contain TLS flags")
			Expect(proxyServerDeploy).NotTo(BeNil(), "Deployment should be loaded in BeforeEach")

			By("Checking proxy-server container args contain TLS flags")
			containers := proxyServerDeploy.Spec.Template.Spec.Containers
			Expect(containers).NotTo(BeEmpty())

			var proxyServerContainer *corev1.Container
			for i := range containers {
				if containers[i].Name == "proxy-server" {
					proxyServerContainer = &containers[i]
					break
				}
			}
			Expect(proxyServerContainer).NotTo(BeNil(), "proxy-server container not found")

			args := proxyServerContainer.Args
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Proxy-server container args: %v\n", args)

			// Verify cipher suites flag is present (we set this in BeforeEach)
			hasCipherSuites := false
			for _, arg := range args {
				if strings.HasPrefix(arg, "--cipher-suites=") {
					hasCipherSuites = true
					fmt.Fprintf(GinkgoWriter, "[SUCCESS] Found cipher suites flag: %s\n", arg)
					break
				}
			}
			Expect(hasCipherSuites).To(BeTrue(), "Expected --cipher-suites flag in proxy-server args")

			// Verify tls-min-version flag is present (we set this in BeforeEach)
			hasMinVersion := false
			for _, arg := range args {
				if strings.HasPrefix(arg, "--tls-min-version=") {
					hasMinVersion = true
					fmt.Fprintf(GinkgoWriter, "[SUCCESS] Found tls-min-version flag: %s\n", arg)
					break
				}
			}
			Expect(hasMinVersion).To(BeTrue(), "Expected --tls-min-version flag in proxy-server args")

			By("Verifying proxy-server pod is running with TLS flags")
			Eventually(func() error {
				pods, err := hubKubeClient.CoreV1().Pods("open-cluster-management-addon").List(context.TODO(), metav1.ListOptions{
					LabelSelector: "component=proxy-server",
				})
				if err != nil {
					return err
				}
				if len(pods.Items) == 0 {
					return fmt.Errorf("no proxy-server pods found")
				}

				// Check at least one pod is running and ready
				for _, pod := range pods.Items {
					if pod.Status.Phase == corev1.PodRunning {
						allReady := true
						for _, status := range pod.Status.ContainerStatuses {
							if !status.Ready {
								allReady = false
								// Check if container crashed due to invalid TLS flags
								if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
									return fmt.Errorf("proxy-server container is crashing - possibly due to invalid TLS flags: %s", status.State.Waiting.Message)
								}
								break
							}
						}
						if allReady {
							fmt.Fprintf(GinkgoWriter, "[SUCCESS] Proxy-server pod %s is running and ready with TLS flags\n", pod.Name)
							return nil
						}
					}
				}
				return fmt.Errorf("no ready proxy-server pods found")
			}).WithTimeout(2 * time.Minute).ShouldNot(HaveOccurred())
		})
	})
