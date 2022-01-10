package configuration

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

const configurationTestBasename = "configuration"

var _ = Describe("Basic configuration Test",
	func() {
		f := framework.NewE2EFramework(configurationTestBasename)

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
						return isDeployed && isAgentServerSigned && isProxyServerSigned, nil
					}).
					WithTimeout(time.Minute).
					Should(BeTrue())
			})

		It("ClusterProxy configuration - scale proxy server", func() {
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			targetReplicas := int32(1)
			proxyConfiguration.Spec.ProxyServer.Replicas = targetReplicas
			err = c.Update(context.TODO(), proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() int32 {
				deploy := &appsv1.Deployment{}
				err = c.Get(context.TODO(), types.NamespacedName{
					Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
					Name:      "cluster-proxy",
				}, deploy)
				Expect(err).NotTo(HaveOccurred())
				return *deploy.Spec.Replicas
			}).
				WithTimeout(time.Minute).
				Should(Equal(targetReplicas))
		})
	})
