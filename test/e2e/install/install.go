package install

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
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	"open-cluster-management.io/cluster-proxy/test/e2e/framework"
)

const installTestBasename = "install"

var _ = Describe("Basic install Test",
	func() {
		f := framework.NewE2EFramework(installTestBasename)

		It("ClusterProxy configuration conditions should be okay",
			func() {
				c := f.HubRuntimeClient()
				By("Polling configuration conditions")
				Eventually(
					func() (bool, error) {
						proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
						err := c.Get(context.TODO(), types.NamespacedName{
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

		It("ManagedClusterAddon should be available", func() {
			c := f.HubRuntimeClient()
			By("Polling addon healthiness")
			Eventually(
				func() (bool, error) {
					addon := &addonapiv1alpha1.ManagedClusterAddOn{}
					if err := c.Get(context.TODO(), types.NamespacedName{
						Namespace: f.TestClusterName(),
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

		It("ManagedClusterAddon should be configured with AddOnDeployMentConfig", func() {
			deployConfigName := "deploy-config"
			nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
			tolerations := []corev1.Toleration{{Key: "node-role.kubernetes.io/infra", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}

			c := f.HubRuntimeClient()
			By("Prepare a AddOnDeployMentConfig for cluster-proxy")
			Eventually(func() error {
				return c.Create(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      deployConfigName,
						Namespace: f.TestClusterName(),
					},
					Spec: addonapiv1alpha1.AddOnDeploymentConfigSpec{
						NodePlacement: &addonapiv1alpha1.NodePlacement{
							NodeSelector: nodeSelector,
							Tolerations:  tolerations,
						},
					},
				})
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Add the config to cluster-proxy")
			Eventually(func() error {
				addon := &addonapiv1alpha1.ManagedClusterAddOn{}
				if err := c.Get(context.TODO(), types.NamespacedName{
					Namespace: f.TestClusterName(),
					Name:      "cluster-proxy",
				}, addon); err != nil {
					return err
				}

				addon.Spec.Configs = []addonapiv1alpha1.AddOnConfig{
					{
						ConfigGroupResource: addonapiv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "addondeploymentconfigs",
						},
						ConfigReferent: addonapiv1alpha1.ConfigReferent{
							Namespace: f.TestClusterName(),
							Name:      deployConfigName,
						},
					},
				}

				return c.Update(context.TODO(), addon)
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the config is referenced")
			Eventually(func() error {
				addon := &addonapiv1alpha1.ManagedClusterAddOn{}
				if err := c.Get(context.TODO(), types.NamespacedName{
					Namespace: f.TestClusterName(),
					Name:      "cluster-proxy",
				}, addon); err != nil {
					return err
				}

				if len(addon.Status.ConfigReferences) == 0 {
					return fmt.Errorf("no config references in addon status")
				}
				if addon.Status.ConfigReferences[0].Name != deployConfigName {
					return fmt.Errorf("unexpected config references %v", addon.Status.ConfigReferences)
				}
				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the cluster-proxy is configured")
			Eventually(func() error {
				deploy := &appsv1.Deployment{}
				if err := c.Get(context.TODO(), types.NamespacedName{
					Namespace: config.AddonInstallNamespace,
					Name:      "cluster-proxy-proxy-agent",
				}, deploy); err != nil {
					return err
				}

				if deploy.Status.AvailableReplicas != *deploy.Spec.Replicas {
					return fmt.Errorf("unexpected available replicas %v", deploy.Status)
				}

				if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.NodeSelector, nodeSelector) {
					return fmt.Errorf("unexpected nodeSeletcor %v", deploy.Spec.Template.Spec.NodeSelector)
				}

				if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.Tolerations, tolerations) {
					return fmt.Errorf("unexpected tolerations %v", deploy.Spec.Template.Spec.Tolerations)
				}
				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the cluster-proxy is available")
			Eventually(func() error {
				addon := &addonapiv1alpha1.ManagedClusterAddOn{}
				if err := c.Get(context.TODO(), types.NamespacedName{
					Namespace: f.TestClusterName(),
					Name:      "cluster-proxy",
				}, addon); err != nil {
					return err
				}

				if !meta.IsStatusConditionTrue(
					addon.Status.Conditions,
					addonapiv1alpha1.ManagedClusterAddOnConditionAvailable) {
					return fmt.Errorf("addon is unavailable")
				}

				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
		})

		It("ClusterProxy configuration - check configuration generation", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				expectedGeneration := proxyConfiguration.Generation
				proxyServerDeploy := &appsv1.Deployment{}
				err = c.Get(context.TODO(), types.NamespacedName{
					Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
					Name:      "cluster-proxy",
				}, proxyServerDeploy)
				if err != nil {
					return err
				}
				if proxyServerDeploy.Annotations[common.AnnotationKeyConfigurationGeneration] != strconv.Itoa(int(expectedGeneration)) {
					return fmt.Errorf("proxy server deployment is not updated")
				}

				proxyAgentDeploy := &appsv1.Deployment{}
				err = c.Get(context.TODO(), types.NamespacedName{
					Namespace: config.AddonInstallNamespace,
					Name:      proxyConfiguration.Name + "-" + common.ComponentNameProxyAgent,
				}, proxyAgentDeploy)
				if err != nil {
					return err
				}
				if proxyAgentDeploy.Annotations[common.AnnotationKeyConfigurationGeneration] != strconv.Itoa(int(expectedGeneration)) {
					return fmt.Errorf("proxy agent deployment is not updated")
				}

				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			waitAgentReady(proxyConfiguration, f.HubNativeClient())
		})
	})

func waitAgentReady(proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration, client kubernetes.Interface) {
	Eventually(
		func() int {
			podList, err := client.CoreV1().
				Pods(config.AddonInstallNamespace).
				List(context.TODO(), metav1.ListOptions{
					LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent,
				})
			Expect(err).NotTo(HaveOccurred())
			matchedGeneration := 0
			for _, pod := range podList.Items {
				allReady := true
				for _, st := range pod.Status.ContainerStatuses {
					if !st.Ready {
						allReady = false
					}
				}
				if allReady &&
					pod.Annotations[common.AnnotationKeyConfigurationGeneration] == strconv.Itoa(int(proxyConfiguration.Generation)) {
					matchedGeneration++
				}
			}
			return matchedGeneration
		}).
		WithTimeout(time.Second * 30).
		Should(Equal(int(proxyConfiguration.Spec.ProxyAgent.Replicas)))
}
