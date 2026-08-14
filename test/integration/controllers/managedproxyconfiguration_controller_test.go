package controllers

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
)

var _ = Describe("ManagedProxyConfigurationReconciler Test", func() {
	var config *proxyv1alpha1.ManagedProxyConfiguration

	const (
		proxyServerNamespace = "open-cluster-management-proxy"
		configName           = "cluster-proxy-config"
		timeout              = time.Second * 30
		interval             = time.Second * 1
	)

	BeforeEach(func() {
		config = &proxyv1alpha1.ManagedProxyConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: configName,
			},
			Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
				ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
					Image:     "cluster-proxy",
					Namespace: proxyServerNamespace,
					Replicas:  3,
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
	})

	AfterEach(func() {
		// Add any teardown steps that needs to be executed after each test
		err := ctrlClient.Delete(ctx, config)
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).ToNot(HaveOccurred())
	})

	Context("Deploy proxy server", func() {
		It("Should have a proxy server deployed correctly with default config", func() {
			// Wait for reconcile done
			Eventually(func() error {
				var err error
				currentConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
				err = ctrlClient.Get(ctx, client.ObjectKeyFromObject(config), currentConfig)
				if err != nil {
					return err
				}
				for _, c := range currentConfig.Status.Conditions {
					if c.Type == proxyv1alpha1.ConditionTypeProxyServerDeployed && corev1.ConditionStatus(c.Status) == corev1.ConditionTrue {
						return nil
					}
				}
				return fmt.Errorf("managedproxy not ready")
			}, 3*timeout, 3*interval).Should(Succeed())

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

		It("Should have a proxy server deployed correctly with node selector, toleration and replicas", func() {
			nodeSelector := map[string]string{"dev": "prod"}
			tolerations := []corev1.Toleration{
				{
					Key:      "test.io/noschedule",
					Operator: corev1.TolerationOpEqual,
					Value:    "noschedule",
				},
			}

			Eventually(func() error {
				newConfig := &proxyv1alpha1.ManagedProxyConfiguration{}

				err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(config), newConfig)
				if err != nil {
					return err
				}

				newConfig.Spec.ProxyServer.NodePlacement = proxyv1alpha1.NodePlacement{
					NodeSelector: nodeSelector,
					Tolerations:  tolerations,
				}
				newConfig.Spec.ProxyServer.Replicas = 1

				// Move update in to Eventually to avoid "the object has been modified; please apply your changes to the latest version and try again"
				err = ctrlClient.Update(ctx, newConfig)
				if err != nil {
					return err
				}

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
				if *deployment.Spec.Replicas != 1 {
					return fmt.Errorf("replicas is not correct, got %d", *deployment.Spec.Replicas)
				}
				return err
			}, timeout, interval).Should(Succeed())
		})

		It("Should reconcile rendered service changes without replacing allocated network fields", func() {
			var originalService *corev1.Service
			Eventually(func() error {
				service, err := kubeClient.CoreV1().Services(proxyServerNamespace).
					Get(ctx, "proxy-entrypoint", metav1.GetOptions{})
				if err != nil {
					return err
				}
				if service.Spec.ClusterIP == "" {
					return fmt.Errorf("service cluster IP is not allocated")
				}
				originalService = service.DeepCopy()
				return nil
			}, timeout, interval).Should(Succeed())

			staleService := originalService.DeepCopy()
			staleService.Spec.Ports[0].Port = 18090
			staleService.Annotations[common.AnnotationKeyRenderedHash] = "stale-render"
			_, err := kubeClient.CoreV1().Services(proxyServerNamespace).
				Update(ctx, staleService, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			var generation int64
			Eventually(func() error {
				currentConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
				if err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(config), currentConfig); err != nil {
					return err
				}
				generation = currentConfig.Generation
				if currentConfig.Annotations == nil {
					currentConfig.Annotations = map[string]string{}
				}
				currentConfig.Annotations["proxy.open-cluster-management.io/test-trigger"] = "service-reconcile"
				return ctrlClient.Update(ctx, currentConfig)
			}, timeout, interval).Should(Succeed())

			Eventually(func() error {
				service, err := kubeClient.CoreV1().Services(proxyServerNamespace).
					Get(ctx, "proxy-entrypoint", metav1.GetOptions{})
				if err != nil {
					return err
				}
				if service.Spec.Ports[0].Port != 8090 {
					return fmt.Errorf("service port is not reconciled, got %d", service.Spec.Ports[0].Port)
				}
				if service.Annotations[common.AnnotationKeyRenderedHash] == "stale-render" {
					return fmt.Errorf("service rendered hash is not reconciled")
				}
				if service.Spec.ClusterIP != originalService.Spec.ClusterIP ||
					!equality.Semantic.DeepEqual(service.Spec.ClusterIPs, originalService.Spec.ClusterIPs) ||
					!equality.Semantic.DeepEqual(service.Spec.IPFamilies, originalService.Spec.IPFamilies) ||
					!equality.Semantic.DeepEqual(service.Spec.IPFamilyPolicy, originalService.Spec.IPFamilyPolicy) {
					return fmt.Errorf("service allocated network fields changed")
				}

				currentConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
				if err := ctrlClient.Get(ctx, client.ObjectKeyFromObject(config), currentConfig); err != nil {
					return err
				}
				if currentConfig.Generation != generation {
					return fmt.Errorf("configuration generation changed from %d to %d", generation, currentConfig.Generation)
				}
				return nil
			}, timeout, interval).Should(Succeed())
		})
	})
})
