package install

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
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

		It("ClusterProxy configuration - scale proxy agent to 1", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			targetReplicas := int32(1)
			proxyConfiguration.Spec.ProxyAgent.Replicas = targetReplicas
			err = c.Update(context.TODO(), proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			Eventually(
				func() bool {
					deploy := &appsv1.Deployment{}
					err = c.Get(context.TODO(), types.NamespacedName{
						Namespace: common.AddonInstallNamespace,
						Name:      proxyConfiguration.Name + "-" + common.ComponentNameProxyAgent,
					}, deploy)
					Expect(err).NotTo(HaveOccurred())
					if *deploy.Spec.Replicas != targetReplicas {
						return false
					}
					if deploy.Status.UpdatedReplicas != targetReplicas {
						return false
					}
					if deploy.Status.ReadyReplicas != targetReplicas {
						return false
					}
					return true
				}).
				WithTimeout(time.Minute).
				Should(BeTrue())
		})

		It("ClusterProxy configuration - scale proxy server to 0", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			targetReplicas := int32(0)
			proxyConfiguration.Spec.ProxyServer.Replicas = targetReplicas
			err = c.Update(context.TODO(), proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			Eventually(
				func() bool {
					deploy := &appsv1.Deployment{}
					err = c.Get(context.TODO(), types.NamespacedName{
						Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
						Name:      "cluster-proxy",
					}, deploy)
					Expect(err).NotTo(HaveOccurred())
					if deploy.Status.Replicas != targetReplicas {
						return false
					}
					if deploy.Status.UpdatedReplicas != targetReplicas {
						return false
					}
					if deploy.Status.ReadyReplicas != targetReplicas {
						return false
					}
					return true
				}).
				WithTimeout(time.Minute).
				Should(BeTrue())
		})

		It("ClusterProxy configuration - scale proxy server to 3", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			targetReplicas := int32(3)
			proxyConfiguration.Spec.ProxyServer.Replicas = targetReplicas
			err = c.Update(context.TODO(), proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			Eventually(
				func() bool {
					deploy := &appsv1.Deployment{}
					err = c.Get(context.TODO(), types.NamespacedName{
						Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
						Name:      "cluster-proxy",
					}, deploy)
					Expect(err).NotTo(HaveOccurred())
					if deploy.Status.Replicas != targetReplicas {
						return false
					}
					if deploy.Status.UpdatedReplicas != targetReplicas {
						return false
					}
					if deploy.Status.ReadyReplicas != targetReplicas {
						return false
					}
					return true
				}).
				WithTimeout(time.Minute).
				Should(BeTrue())
		})

		It("ClusterProxy configuration - check configuration generation", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())
			expectedGeneration := proxyConfiguration.Generation
			proxyServerDeploy := &appsv1.Deployment{}
			err = c.Get(context.TODO(), types.NamespacedName{
				Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
				Name:      "cluster-proxy",
			}, proxyServerDeploy)
			Expect(err).NotTo(HaveOccurred())
			Expect(proxyServerDeploy.Annotations[common.AnnotationKeyConfigurationGeneration]).
				To(Equal(strconv.Itoa(int(expectedGeneration))))
			proxyAgentDeploy := &appsv1.Deployment{}
			err = c.Get(context.TODO(), types.NamespacedName{
				Namespace: common.AddonInstallNamespace,
				Name:      proxyConfiguration.Name + "-" + common.ComponentNameProxyAgent,
			}, proxyAgentDeploy)
			Expect(err).NotTo(HaveOccurred())
			Expect(proxyAgentDeploy.Annotations[common.AnnotationKeyConfigurationGeneration]).
				To(Equal(strconv.Itoa(int(expectedGeneration))))
		})
	})
