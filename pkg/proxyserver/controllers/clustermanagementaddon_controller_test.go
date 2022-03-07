package controllers

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

var _ = Describe("ClusterManagementAddon Controller", func() {
	var addon *addonv1alpha1.ClusterManagementAddOn
	var config *proxyv1alpha1.ManagedProxyConfiguration

	const (
		proxyServerNamespace = "open-cluster-management-proxy"
		configName           = "cluster-proxy-config"
		timeout              = time.Second * 30
		interval             = time.Second * 1
	)

	BeforeEach(func() {
		// Create namespace
		addon = &addonv1alpha1.ClusterManagementAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Name: addonName,
			},
			Spec: addonv1alpha1.ClusterManagementAddOnSpec{
				AddOnConfiguration: addonv1alpha1.ConfigCoordinates{
					CRName: configName,
				},
			},
		}

		config = &proxyv1alpha1.ManagedProxyConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: configName,
			},
			Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
				ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
					Image:     "cluster-proxy",
					Namespace: proxyServerNamespace,
					Entrypoint: &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
						Type: proxyv1alpha1.EntryPointTypePortForward,
					},
				},
				Authentication: proxyv1alpha1.ManagedProxyConfigurationAuthentication{
					Signer: proxyv1alpha1.ManagedProxyConfigurationCertificateSigner{
						Type: proxyv1alpha1.SelfSigned,
					},
					Dump: proxyv1alpha1.ManagedProxyConfigurationCertificateDump{
						Secrets: proxyv1alpha1.CertificateSigningSecrets{},
					},
				},
				ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
					Image: "cluster-proxy-agent",
				},
			},
		}

		err := ctrlClient.Create(ctx, config)
		Expect(err).ToNot(HaveOccurred())

		err = ctrlClient.Create(ctx, addon)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		// Add any teardown steps that needs to be executed after each test
		err := ctrlClient.Delete(ctx, addon)
		Expect(err).ToNot(HaveOccurred())

		err = ctrlClient.Delete(ctx, config)
		Expect(err).ToNot(HaveOccurred())
	})

	Context("Deploy proxy server", func() {
		It("Should have a proxy server deployed correctly with default config", func() {
			Eventually(func() error {
				_, err := kubeClient.CoreV1().Namespaces().Get(ctx, proxyServerNamespace, metav1.GetOptions{})
				return err
			}, timeout, interval).Should(Succeed())

			Eventually(func() error {
				_, err := kubeClient.CoreV1().Secrets(proxyServerNamespace).Get(ctx, "proxy-client", metav1.GetOptions{})
				return err
			}, timeout, interval).Should(Succeed())

			Eventually(func() error {
				deployment, err := kubeClient.AppsV1().Deployments(proxyServerNamespace).Get(ctx, configName, metav1.GetOptions{})
				if err != nil {
					return err
				}

				image := deployment.Spec.Template.Spec.Containers[0].Image
				if image != "cluster-proxy" {
					return fmt.Errorf("image is not correct, get %s", image)
				}

				replicas := *deployment.Spec.Replicas
				if replicas != 3 {
					return fmt.Errorf("replicas is not correct, get %d", replicas)
				}
				return err
			}, timeout, interval).Should(Succeed())
		})

		It("Should have a proxy server deployed correctly with node selector and toleration", func() {
			nodeSelector := map[string]string{"dev": "prod"}
			tolerations := []corev1.Toleration{
				{
					Key:      "test.io/noschedule",
					Operator: corev1.TolerationOpEqual,
					Value:    "noschedule",
				},
			}

			newConfig := &proxyv1alpha1.ManagedProxyConfiguration{}

			err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(config), newConfig)
			Expect(err).ToNot(HaveOccurred())

			newConfig.Spec.ProxyServer.NodePlacement = proxyv1alpha1.NodePlacement{
				NodeSelector: nodeSelector,
				Tolerations:  tolerations,
			}

			err = ctrlClient.Update(ctx, newConfig)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				deployment, err := kubeClient.AppsV1().Deployments(proxyServerNamespace).Get(ctx, configName, metav1.GetOptions{})
				if err != nil {
					return err
				}

				if !equality.Semantic.DeepEqual(deployment.Spec.Template.Spec.NodeSelector, nodeSelector) {
					return fmt.Errorf("nodeSelect is not correct, got %v", deployment.Spec.Template.Spec.NodeSelector)
				}
				if !equality.Semantic.DeepEqual(deployment.Spec.Template.Spec.Tolerations, tolerations) {
					return fmt.Errorf("tolerations is not correct, got %v", deployment.Spec.Template.Spec.Tolerations)
				}
				return err
			}, timeout, interval).Should(Succeed())
		})
	})
})
