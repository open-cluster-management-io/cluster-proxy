package e2e

import (
	"context"
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
)

var _ = Describe("Install Test", Label("install", "deployment"),
	func() {
		It("ClusterProxy configuration conditions should be okay", Label("configuration", "conditions"),
			func() {
				By("Polling configuration conditions")
				Eventually(
					func() (bool, error) {
						proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
						err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
							Name: "cluster-proxy",
						}, proxyConfiguration)
						if err != nil {
							return false, err
						}
						isDeployed := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeProxyServerDeployed)
						isAgentServerSigned := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeProxyServerSecretSigned)
						isProxyServerSigned := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeAgentServerSecretSigned)
						ready := isDeployed && isAgentServerSigned && isProxyServerSigned
						return ready, nil
					}).
					WithTimeout(time.Minute).
					Should(BeTrue())
			})

		It("ManagedClusterAddon should be available", Label("addon", "health"), func() {
			By("Polling addon healthiness")
			Eventually(
				func() (bool, error) {
					addon := &addonapiv1alpha1.ManagedClusterAddOn{}
					if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
						Namespace: managedClusterName,
						Name:      "cluster-proxy",
					}, addon); err != nil {
						if apierrors.IsNotFound(err) {
							return false, nil
						}
						return false, err
					}
					return meta.IsStatusConditionTrue(
						addon.Status.Conditions,
						addonapiv1alpha1.ManagedClusterAddOnConditionAvailable), nil
				}).
				WithTimeout(time.Minute).
				Should(BeTrue())
		})

		It("ManagedClusterAddon should be configured with AddOnDeployMentConfig", Label("addon", "config", "deployment"), func() {
			deployConfigName := "deploy-config"
			nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
			tolerations := []corev1.Toleration{{Key: "node-role.kubernetes.io/infra", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}

			By("Cleanup existing AddOnDeploymentConfig if any")
			Expect(deleteAddOnDeploymentConfig(deployConfigName)).To(Succeed())

			waitProxyAgentDeploymentRolledOut()
			originalAddon, err := getManagedClusterAddon()
			Expect(err).ToNot(HaveOccurred())
			originalConfigs := originalAddon.Spec.Configs

			originalDeployment, err := getProxyAgentDeployment()
			Expect(err).ToNot(HaveOccurred())
			originalNodeSelector := originalDeployment.Spec.Template.Spec.NodeSelector
			originalTolerations := originalDeployment.Spec.Template.Spec.Tolerations
			originalReplicas := ptr.Deref(originalDeployment.Spec.Replicas, 1)

			DeferCleanup(func() {
				By("Restore cluster-proxy addon config after test")
				Eventually(func() error {
					return setManagedClusterAddonConfigs(originalConfigs)
				}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

				By("Cleanup AddOnDeploymentConfig after test")
				Expect(deleteAddOnDeploymentConfig(deployConfigName)).To(Succeed())

				By("Wait for cluster-proxy deployment to return to the previous placement")
				waitProxyAgentDeploymentConfigured(originalNodeSelector, originalTolerations, originalReplicas)
				waitManagedClusterAddonAvailable()
			})

			By("Prepare a AddOnDeployMentConfig for cluster-proxy")
			Eventually(func() error {
				return hubRuntimeClient.Create(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      deployConfigName,
						Namespace: managedClusterName,
					},
					Spec: addonapiv1alpha1.AddOnDeploymentConfigSpec{
						NodePlacement: &addonapiv1alpha1.NodePlacement{
							NodeSelector: nodeSelector,
							Tolerations:  tolerations,
						},
						AgentInstallNamespace: config.DefaultAddonInstallNamespace,
					},
				})
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Add the config to cluster-proxy")
			Eventually(func() error {
				return setManagedClusterAddonConfigs([]addonapiv1alpha1.AddOnConfig{
					addOnDeploymentConfigReference(deployConfigName),
				})
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the config is referenced")
			waitManagedClusterAddonConfigReferenced(deployConfigName)

			By("Ensure the cluster-proxy is configured")
			waitProxyAgentDeploymentConfigured(nodeSelector, tolerations, originalReplicas)

			By("Ensure the cluster-proxy is available")
			waitManagedClusterAddonAvailable()
		})

		It("ClusterProxy configuration - check configuration generation", Label("configuration", "generation"), func() {
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).ToNot(HaveOccurred())

			expectedGenerationAnnotation := strconv.Itoa(int(proxyConfiguration.Generation))
			Eventually(func() error {
				proxyServerDeploy := &appsv1.Deployment{}
				err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
					Name:      "cluster-proxy",
				}, proxyServerDeploy)
				if err != nil {
					return err
				}
				if proxyServerDeploy.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
					return fmt.Errorf("proxy server deployment is not updated")
				}

				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			waitProxyAgentReady(proxyConfiguration, hubKubeClient)
		})
	})

func waitProxyAgentReady(proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration, client kubernetes.Interface) {
	waitProxyAgentDeploymentGenerationRolledOut(proxyConfiguration.Generation, proxyConfiguration.Spec.ProxyAgent.Replicas)

	expectedGenerationAnnotation := strconv.Itoa(int(proxyConfiguration.Generation))
	expectedReplicas := int(proxyConfiguration.Spec.ProxyAgent.Replicas)
	Eventually(
		func() int {
			podList, err := client.CoreV1().
				Pods(config.DefaultAddonInstallNamespace).
				List(context.TODO(), metav1.ListOptions{
					LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent,
				})
			Expect(err).NotTo(HaveOccurred())
			matchedGeneration := 0
			for _, pod := range podList.Items {
				if pod.DeletionTimestamp != nil {
					continue
				}
				allReady := true
				for _, st := range pod.Status.ContainerStatuses {
					if !st.Ready {
						allReady = false
					}
				}
				if allReady &&
					pod.Annotations[common.AnnotationKeyConfigurationGeneration] == expectedGenerationAnnotation {
					matchedGeneration++
				}
			}
			return matchedGeneration
		}).
		WithTimeout(time.Second * 30).
		Should(Equal(expectedReplicas))
}

func waitManagedClusterAddonAvailable() {
	Eventually(func() error {
		addon, err := getManagedClusterAddon()
		if err != nil {
			return err
		}

		if !meta.IsStatusConditionTrue(
			addon.Status.Conditions,
			addonapiv1alpha1.ManagedClusterAddOnConditionAvailable) {
			return fmt.Errorf("addon is unavailable")
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func waitManagedClusterAddonConfigReferenced(name string) {
	expected := addOnDeploymentConfigReference(name)
	Eventually(func() error {
		addon, err := getManagedClusterAddon()
		if err != nil {
			return err
		}

		for _, cr := range addon.Status.ConfigReferences {
			if cr.ConfigGroupResource == expected.ConfigGroupResource &&
				cr.ConfigReferent == expected.ConfigReferent {
				return nil
			}
		}
		return fmt.Errorf("config reference %s/%s not found in addon status: %v",
			expected.Namespace, expected.Name, addon.Status.ConfigReferences)
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func setManagedClusterAddonConfigs(configs []addonapiv1alpha1.AddOnConfig) error {
	addon, err := getManagedClusterAddon()
	if err != nil {
		return err
	}

	addon.Spec.Configs = configs
	return hubRuntimeClient.Update(context.TODO(), addon)
}

func getManagedClusterAddon() (*addonapiv1alpha1.ManagedClusterAddOn, error) {
	addon := &addonapiv1alpha1.ManagedClusterAddOn{}
	err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: managedClusterName,
		Name:      "cluster-proxy",
	}, addon)
	return addon, err
}

func addOnDeploymentConfigReference(name string) addonapiv1alpha1.AddOnConfig {
	return addonapiv1alpha1.AddOnConfig{
		ConfigGroupResource: addonapiv1alpha1.ConfigGroupResource{
			Group:    "addon.open-cluster-management.io",
			Resource: "addondeploymentconfigs",
		},
		ConfigReferent: addonapiv1alpha1.ConfigReferent{
			Namespace: managedClusterName,
			Name:      name,
		},
	}
}

func deleteAddOnDeploymentConfig(name string) error {
	err := hubRuntimeClient.Delete(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: managedClusterName,
		},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func waitProxyAgentDeploymentConfigured(
	expectedNodeSelector map[string]string,
	expectedTolerations []corev1.Toleration,
	expectedReplicas int32,
) {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		if err := proxyAgentRolledOut(deploy, expectedReplicas); err != nil {
			return err
		}

		if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.NodeSelector, expectedNodeSelector) {
			return fmt.Errorf("unexpected nodeSelector %v", deploy.Spec.Template.Spec.NodeSelector)
		}

		if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.Tolerations, expectedTolerations) {
			return fmt.Errorf("unexpected tolerations %v", deploy.Spec.Template.Spec.Tolerations)
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func waitProxyAgentDeploymentGenerationRolledOut(expectedGeneration int64, expectedReplicas int32) {
	expectedGenerationAnnotation := strconv.Itoa(int(expectedGeneration))
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		if deploy.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
			return fmt.Errorf("proxy agent deployment generation annotation is not updated")
		}
		if deploy.Spec.Template.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
			return fmt.Errorf("proxy agent pod template generation annotation is not updated")
		}

		return proxyAgentRolledOut(deploy, expectedReplicas)
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func waitProxyAgentDeploymentRolledOut() {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		return proxyAgentRolledOut(deploy, ptr.Deref(deploy.Spec.Replicas, 1))
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

// proxyAgentRolledOut returns nil only once the proxy agent deployment has fully
// rolled out the latest generation to expectedReplicas pods with none unavailable.
func proxyAgentRolledOut(deploy *appsv1.Deployment, expectedReplicas int32) error {
	if deploy.Status.ObservedGeneration < deploy.Generation {
		return fmt.Errorf("proxy agent deployment generation %d has not been observed: %v", deploy.Generation, deploy.Status)
	}

	if specReplicas := ptr.Deref(deploy.Spec.Replicas, 1); specReplicas != expectedReplicas {
		return fmt.Errorf("unexpected proxy agent spec replicas %d", specReplicas)
	}

	if deploy.Status.Replicas != expectedReplicas ||
		deploy.Status.UpdatedReplicas != expectedReplicas ||
		deploy.Status.ReadyReplicas != expectedReplicas ||
		deploy.Status.AvailableReplicas != expectedReplicas ||
		deploy.Status.UnavailableReplicas != 0 {
		return fmt.Errorf("proxy agent deployment rollout is incomplete: %v", deploy.Status)
	}

	return nil
}

func getProxyAgentDeployment() (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{}
	err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: config.DefaultAddonInstallNamespace,
		Name:      "cluster-proxy-proxy-agent",
	}, deploy)
	return deploy, err
}
