package install

import (
	"context"
	"net"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
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
						return isDeployed && isAgentServerSigned && isProxyServerSigned, nil
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

		It("Probe cluster health",
			func() {
				cfg := f.HubRESTConfig()
				c := f.HubRuntimeClient()
				proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}

				err := c.Get(context.TODO(), types.NamespacedName{
					Name: "cluster-proxy",
				}, proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				By("Running local port-forward stream to proxy service")
				localProxy := util.NewRoundRobinLocalProxy(
					cfg,
					proxyConfiguration.Spec.ProxyServer.Namespace,
					common.LabelKeyComponentName+"="+common.ComponentNameProxyServer, // TODO: configurable label selector?
					8090,
				)

				ctx, cancel := context.WithCancel(context.TODO())
				defer cancel()

				closeFn, err := localProxy.Listen(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer closeFn()

				tunnelTlsCfg, err := util.GetKonnectivityTLSConfig(cfg, proxyConfiguration)
				Expect(err).NotTo(HaveOccurred())

				tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
					ctx,
					net.JoinHostPort("127.0.0.1", "8090"),
					grpc.WithTransportCredentials(grpccredentials.NewTLS(tunnelTlsCfg)),
				)
				mungledRestConfig := rest.CopyConfig(cfg)
				mungledRestConfig.TLSClientConfig = rest.TLSClientConfig{
					Insecure: true,
				}
				mungledRestConfig.Host = f.TestClusterName()
				mungledRestConfig.Dial = tunnel.DialContext
				nativeClient, err := kubernetes.NewForConfig(mungledRestConfig)
				Expect(err).NotTo(HaveOccurred())
				data, err := nativeClient.RESTClient().Get().AbsPath("/healthz").DoRaw(context.TODO())
				Expect(err).NotTo(HaveOccurred())
				Expect(string(data)).To(Equal("ok"))
			})

		It("ClusterProxy configuration - scale proxy server", func() {
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
