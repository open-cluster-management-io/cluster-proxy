package install

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

const installTestBasename = "install"

var _ = Describe("Basic install Test",
	func() {
		f := framework.NewE2EFramework(installTestBasename)

		It("Install cluster management addon",
			func() {
				proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
				c := f.HubRuntimeClient()
				err := c.Get(context.TODO(), types.NamespacedName{
					Name: "cluster-proxy",
				}, proxyConfiguration)
				if apierrors.IsNotFound(err) {
					By("Missing ManagedProxyConfiguration, creating one")
					proxyConfiguration = &proxyv1alpha1.ManagedProxyConfiguration{
						ObjectMeta: metav1.ObjectMeta{
							Name: "cluster-proxy",
						},
						Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
							ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
								Image: "quay.io/open-cluster-management/cluster-proxy:latest",
							},
							ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
								Image: "quay.io/open-cluster-management/cluster-proxy:latest",
							},
						},
					}
					Expect(c.Create(context.TODO(), proxyConfiguration)).NotTo(HaveOccurred())
				}
				Expect(err).NotTo(HaveOccurred())
			})

		It("ClusterProxy configuration conditions should be okay",
			func() {
				c := f.HubRuntimeClient()
				By("ManagedProxyConfiguration's conditions should be working")
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

	})
