package controllers

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/proxyagent/agent"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/controllers"
	cpv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("ClusterProfileReconciler Test", func() {
	const (
		proxyServerNamespace = "open-cluster-management-proxy"
		clusterName          = "test-cluster"
		timeout              = time.Second * 30
		interval             = time.Second * 1
	)

	var (
		clusterProfile *cpv1alpha1.ClusterProfile
		config         *proxyv1alpha1.ManagedProxyConfiguration
		caSecret       *corev1.Secret
		namespace      *corev1.Namespace
	)

	BeforeEach(func() {
		// Create namespace (ignore if already exists)
		namespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: proxyServerNamespace,
			},
		}
		err := ctrlClient.Create(ctx, namespace)
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		// Create ManagedProxyConfiguration
		config = &proxyv1alpha1.ManagedProxyConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: agent.ManagedClusterConfigurationName,
			},
			Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
				ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
					Namespace:            proxyServerNamespace,
					InClusterServiceName: constant.UserServerServiceName,
					Image:                "quay.io/open-cluster-management/cluster-proxy:latest",
					Entrypoint: &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
						Type: proxyv1alpha1.EntryPointTypePortForward,
					},
				},
				ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
					Image: "quay.io/open-cluster-management/cluster-proxy:latest",
				},
				Authentication: proxyv1alpha1.ManagedProxyConfigurationAuthentication{
					Signer: proxyv1alpha1.ManagedProxyConfigurationCertificateSigner{
						Type: proxyv1alpha1.SelfSigned,
					},
				},
			},
		}
		err = ctrlClient.Create(ctx, config)
		Expect(err).ToNot(HaveOccurred())

		// Create CA secret
		caSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      constant.UserServerSecretName,
				Namespace: proxyServerNamespace,
			},
			Data: map[string][]byte{
				"tls.crt": []byte("test-certificate-data"),
			},
		}
		err = ctrlClient.Create(ctx, caSecret)
		Expect(err).ToNot(HaveOccurred())

		// Create ClusterProfile with proper labels
		clusterProfile = &cpv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: "default",
				Labels: map[string]string{
					cpv1alpha1.LabelClusterManagerKey: controllers.ClusterProfileManagerName,
					clusterv1.ClusterNameLabelKey:     clusterName,
				},
			},
			Spec: cpv1alpha1.ClusterProfileSpec{
				DisplayName: clusterName,
				ClusterManager: cpv1alpha1.ClusterManager{
					Name: controllers.ClusterProfileManagerName,
				},
			},
		}
		err = ctrlClient.Create(ctx, clusterProfile)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		// Cleanup in reverse order (ignore not found errors)
		err := ctrlClient.Delete(ctx, clusterProfile)
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		err = ctrlClient.Delete(ctx, caSecret)
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		err = ctrlClient.Delete(ctx, config)
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		// Don't delete namespace as it may be shared with other tests
	})

	Context("Reconcile ClusterProfile", func() {
		It("Should add AccessProvider to ClusterProfile status", func() {
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(clusterProfile), currentProfile)
				if err != nil {
					return err
				}

				if len(currentProfile.Status.AccessProviders) == 0 {
					return fmt.Errorf("no access providers found")
				}

				// Check if open-cluster-management provider exists
				var ocmProvider *cpv1alpha1.AccessProvider
				for i := range currentProfile.Status.AccessProviders {
					if currentProfile.Status.AccessProviders[i].Name == controllers.OCMAccessProviderName {
						ocmProvider = &currentProfile.Status.AccessProviders[i]
						break
					}
				}

				if ocmProvider == nil {
					return fmt.Errorf("open-cluster-management provider not found")
				}

				// Verify the server URL format
				expectedServer := fmt.Sprintf("https://%s.%s:9092/%s",
					constant.UserServerServiceName,
					proxyServerNamespace,
					clusterName)
				if ocmProvider.Cluster.Server != expectedServer {
					return fmt.Errorf("unexpected server URL: got %s, want %s", ocmProvider.Cluster.Server, expectedServer)
				}

				// Verify CA data
				if string(ocmProvider.Cluster.CertificateAuthorityData) != "test-certificate-data" {
					return fmt.Errorf("unexpected CA data")
				}

				// Verify exec extension
				if len(ocmProvider.Cluster.Extensions) != 1 {
					return fmt.Errorf("expected 1 extension, got %d", len(ocmProvider.Cluster.Extensions))
				}

				ext := ocmProvider.Cluster.Extensions[0]
				if ext.Name != "client.authentication.k8s.io/exec" {
					return fmt.Errorf("unexpected extension name: %s", ext.Name)
				}

				// Verify extension contains cluster name
				var execConfig map[string]any
				err = json.Unmarshal(ext.Extension.Raw, &execConfig)
				if err != nil {
					return fmt.Errorf("failed to unmarshal exec extension: %v", err)
				}

				if execConfig["clusterName"] != clusterName {
					return fmt.Errorf("unexpected cluster name in exec config: %v", execConfig["clusterName"])
				}

				return nil
			}, timeout, interval).Should(Succeed())
		})

		It("Should update existing AccessProvider when ClusterProfile changes", func() {
			// Wait for initial AccessProvider to be created
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(clusterProfile), currentProfile)
				if err != nil {
					return err
				}

				if len(currentProfile.Status.AccessProviders) == 0 {
					return fmt.Errorf("no access providers found")
				}

				return nil
			}, timeout, interval).Should(Succeed())

			// Manually add another provider to simulate existing providers
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(clusterProfile), currentProfile)
				if err != nil {
					return err
				}

				// Add another provider
				currentProfile.Status.AccessProviders = append(currentProfile.Status.AccessProviders, cpv1alpha1.AccessProvider{
					Name: "another-provider",
					Cluster: clientcmdapiv1.Cluster{
						Server: "https://another-server:8443",
					},
				})

				return ctrlClient.Status().Update(ctx, currentProfile)
			}, timeout, interval).Should(Succeed())

			// Trigger reconciliation by updating the ClusterProfile
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(clusterProfile), currentProfile)
				if err != nil {
					return err
				}

				currentProfile.Spec.DisplayName = "Updated Test Cluster"
				return ctrlClient.Update(ctx, currentProfile)
			}, timeout, interval).Should(Succeed())

			// Verify both providers exist and open-cluster-management is still correct
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(clusterProfile), currentProfile)
				if err != nil {
					return err
				}

				if len(currentProfile.Status.AccessProviders) != 2 {
					return fmt.Errorf("expected 2 access providers, got %d", len(currentProfile.Status.AccessProviders))
				}

				// Verify both providers exist
				hasOCM := false
				hasAnother := false
				for _, provider := range currentProfile.Status.AccessProviders {
					if provider.Name == controllers.OCMAccessProviderName {
						hasOCM = true
					}
					if provider.Name == "another-provider" {
						hasAnother = true
					}
				}

				if !hasOCM || !hasAnother {
					return fmt.Errorf("missing expected providers: ocm=%v, another=%v", hasOCM, hasAnother)
				}

				return nil
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("Handle missing resources", func() {
		It("Should requeue when ManagedProxyConfiguration is missing", func() {
			// Delete the config
			err := ctrlClient.Delete(ctx, config)
			Expect(err).ToNot(HaveOccurred())

			// Create a new ClusterProfile
			newCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-without-config",
					Namespace: "default",
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster Without Config",
					ClusterManager: cpv1alpha1.ClusterManager{
						Name: controllers.ClusterProfileManagerName,
					},
				},
			}
			err = ctrlClient.Create(ctx, newCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait for the ClusterProfile to be readable
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				return ctrlClient.Get(ctx, client.ObjectKeyFromObject(newCluster), currentProfile)
			}, timeout, interval).Should(Succeed())

			// Verify no AccessProvider is added (since config is missing)
			Consistently(func() int {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(newCluster), currentProfile)
				if err != nil {
					return -1
				}
				return len(currentProfile.Status.AccessProviders)
			}, 5*time.Second, interval).Should(Equal(0))

			// Cleanup
			err = ctrlClient.Delete(ctx, newCluster)
			Expect(err).ToNot(HaveOccurred())

			// Recreate config for other tests
			config = &proxyv1alpha1.ManagedProxyConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: agent.ManagedClusterConfigurationName,
				},
				Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
					ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
						Namespace:            proxyServerNamespace,
						InClusterServiceName: constant.UserServerServiceName,
						Image:                "quay.io/open-cluster-management/cluster-proxy:latest",
						Entrypoint: &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
							Type: proxyv1alpha1.EntryPointTypePortForward,
						},
					},
					ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
						Image: "quay.io/open-cluster-management/cluster-proxy:latest",
					},
					Authentication: proxyv1alpha1.ManagedProxyConfigurationAuthentication{
						Signer: proxyv1alpha1.ManagedProxyConfigurationCertificateSigner{
							Type: proxyv1alpha1.SelfSigned,
						},
					},
				},
			}
			err = ctrlClient.Create(ctx, config)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should requeue when CA secret is missing", func() {
			// Delete the CA secret
			err := ctrlClient.Delete(ctx, caSecret)
			Expect(err).ToNot(HaveOccurred())

			// Create a new ClusterProfile
			newCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-without-secret",
					Namespace: "default",
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster Without Secret",
					ClusterManager: cpv1alpha1.ClusterManager{
						Name: controllers.ClusterProfileManagerName,
					},
				},
			}
			err = ctrlClient.Create(ctx, newCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait for the ClusterProfile to be readable
			Eventually(func() error {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				return ctrlClient.Get(ctx, client.ObjectKeyFromObject(newCluster), currentProfile)
			}, timeout, interval).Should(Succeed())

			// Verify no AccessProvider is added (since secret is missing)
			Consistently(func() int {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(newCluster), currentProfile)
				if err != nil {
					return -1
				}
				return len(currentProfile.Status.AccessProviders)
			}, 5*time.Second, interval).Should(Equal(0))

			// Cleanup
			err = ctrlClient.Delete(ctx, newCluster)
			Expect(err).ToNot(HaveOccurred())

			// Recreate secret for other tests
			caSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constant.UserServerSecretName,
					Namespace: proxyServerNamespace,
				},
				Data: map[string][]byte{
					"tls.crt": []byte("test-certificate-data"),
				},
			}
			err = ctrlClient.Create(ctx, caSecret)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("Filter ClusterProfile with labels", func() {
		It("Should NOT reconcile ClusterProfile without cluster manager label", func() {
			// Create ClusterProfile without cluster manager label
			noManagerCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-no-manager-label",
					Namespace: "default",
					Labels: map[string]string{
						clusterv1.ClusterNameLabelKey: "cluster-no-manager-label",
					},
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster Without Manager Label",
				},
			}
			err := ctrlClient.Create(ctx, noManagerCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait a bit to ensure controller has time to process
			time.Sleep(3 * time.Second)

			// Verify no AccessProvider is added (controller should skip it)
			currentProfile := &cpv1alpha1.ClusterProfile{}
			err = ctrlClient.Get(ctx, client.ObjectKeyFromObject(noManagerCluster), currentProfile)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(currentProfile.Status.AccessProviders)).To(Equal(0))

			// Cleanup
			err = ctrlClient.Delete(ctx, noManagerCluster)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should NOT reconcile ClusterProfile with wrong cluster manager label", func() {
			// Create ClusterProfile with wrong cluster manager
			wrongManagerCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-wrong-manager",
					Namespace: "default",
					Labels: map[string]string{
						cpv1alpha1.LabelClusterManagerKey: "other-cluster-manager",
						clusterv1.ClusterNameLabelKey:     "cluster-wrong-manager",
					},
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster With Wrong Manager",
				},
			}
			err := ctrlClient.Create(ctx, wrongManagerCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait a bit to ensure controller has time to process
			time.Sleep(3 * time.Second)

			// Verify no AccessProvider is added
			currentProfile := &cpv1alpha1.ClusterProfile{}
			err = ctrlClient.Get(ctx, client.ObjectKeyFromObject(wrongManagerCluster), currentProfile)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(currentProfile.Status.AccessProviders)).To(Equal(0))

			// Cleanup
			err = ctrlClient.Delete(ctx, wrongManagerCluster)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should NOT reconcile ClusterProfile without cluster name label", func() {
			// Create ClusterProfile without cluster name label
			noNameCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-no-name-label",
					Namespace: "default",
					Labels: map[string]string{
						cpv1alpha1.LabelClusterManagerKey: controllers.ClusterProfileManagerName,
					},
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster Without Name Label",
				},
			}
			err := ctrlClient.Create(ctx, noNameCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait a bit to ensure controller has time to process
			time.Sleep(3 * time.Second)

			// Verify no AccessProvider is added
			currentProfile := &cpv1alpha1.ClusterProfile{}
			err = ctrlClient.Get(ctx, client.ObjectKeyFromObject(noNameCluster), currentProfile)
			Expect(err).ToNot(HaveOccurred())
			Expect(len(currentProfile.Status.AccessProviders)).To(Equal(0))

			// Cleanup
			err = ctrlClient.Delete(ctx, noNameCluster)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should reconcile ClusterProfile with both required labels", func() {
			// Create ClusterProfile with both required labels
			validCluster := &cpv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-valid-labels",
					Namespace: "default",
					Labels: map[string]string{
						cpv1alpha1.LabelClusterManagerKey: controllers.ClusterProfileManagerName,
						clusterv1.ClusterNameLabelKey:     "cluster-valid-labels",
					},
				},
				Spec: cpv1alpha1.ClusterProfileSpec{
					DisplayName: "Cluster With Valid Labels",
				},
			}
			err := ctrlClient.Create(ctx, validCluster)
			Expect(err).ToNot(HaveOccurred())

			// Wait for AccessProvider to be added
			Eventually(func() int {
				currentProfile := &cpv1alpha1.ClusterProfile{}
				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(validCluster), currentProfile)
				if err != nil {
					return -1
				}
				return len(currentProfile.Status.AccessProviders)
			}, timeout, interval).Should(BeNumerically(">", 0))

			// Verify the AccessProvider was added
			currentProfile := &cpv1alpha1.ClusterProfile{}
			err = ctrlClient.Get(ctx, client.ObjectKeyFromObject(validCluster), currentProfile)
			Expect(err).ToNot(HaveOccurred())

			// Find OCM provider
			var ocmProvider *cpv1alpha1.AccessProvider
			for i := range currentProfile.Status.AccessProviders {
				if currentProfile.Status.AccessProviders[i].Name == controllers.OCMAccessProviderName {
					ocmProvider = &currentProfile.Status.AccessProviders[i]
					break
				}
			}
			Expect(ocmProvider).ToNot(BeNil())

			// Cleanup
			err = ctrlClient.Delete(ctx, validCluster)
			Expect(err).ToNot(HaveOccurred())
		})
	})
})
