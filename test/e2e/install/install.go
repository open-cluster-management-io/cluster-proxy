package install

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"open-cluster-management.io/cluster-proxy/e2e/framework"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
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

		It("Probe cluster health",
			func() {
				probeOnce(f)
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
						Namespace: config.AddonInstallNamespace,
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
				func() error {
					deploy := &appsv1.Deployment{}
					err = c.Get(context.TODO(), types.NamespacedName{
						Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
						Name:      "cluster-proxy",
					}, deploy)
					if err != nil {
						return err
					}
					if deploy.Status.Replicas != targetReplicas {
						return fmt.Errorf("replicas in status is not correct, get %d", deploy.Status.Replicas)
					}
					if deploy.Status.UpdatedReplicas != targetReplicas {
						return fmt.Errorf("updatedReplicas in status is not correct, get %d", deploy.Status.UpdatedReplicas)
					}
					if deploy.Status.ReadyReplicas != targetReplicas {
						return fmt.Errorf("readyReplicas in status is not correct, get %d", deploy.Status.ReadyReplicas)
					}
					return nil
				}).
				WithTimeout(time.Minute).
				Should(Succeed())
		})

		It("ClusterProxy configuration - scale proxy server to 1", func() {
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

			Eventually(
				func() error {
					deploy := &appsv1.Deployment{}
					err = c.Get(context.TODO(), types.NamespacedName{
						Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
						Name:      "cluster-proxy",
					}, deploy)
					if err != nil {
						return err
					}
					if deploy.Status.Replicas != targetReplicas {
						return fmt.Errorf("replicas in status is not correct, get %d", deploy.Status.Replicas)
					}
					if deploy.Status.UpdatedReplicas != targetReplicas {
						return fmt.Errorf("updatedReplicas in status is not correct, get %d", deploy.Status.UpdatedReplicas)
					}
					if deploy.Status.ReadyReplicas != targetReplicas {
						return fmt.Errorf("readyReplica in status is not correct, get %d", deploy.Status.ReadyReplicas)
					}
					return nil
				}).
				WithTimeout(time.Minute).
				Should(Succeed())
			waitAgentReady(proxyConfiguration, f.HubNativeClient())
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
				Namespace: config.AddonInstallNamespace,
				Name:      proxyConfiguration.Name + "-" + common.ComponentNameProxyAgent,
			}, proxyAgentDeploy)
			Expect(err).NotTo(HaveOccurred())
			Expect(proxyAgentDeploy.Annotations[common.AnnotationKeyConfigurationGeneration]).
				To(Equal(strconv.Itoa(int(expectedGeneration))))
			waitAgentReady(proxyConfiguration, f.HubNativeClient())
		})

		It("Probe cluster health should work after proxy servers restart",
			func() {
				probeOnce(f)
			})
	})

func probeOnce(f framework.Framework) {
	Eventually(
		func() (bool, error) {
			cfg := f.HubRESTConfig()
			c := f.HubRuntimeClient()
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := c.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).NotTo(HaveOccurred())

			By("Running local port-forward stream to proxy service")
			localProxy := util.NewRoundRobinLocalProxyWithReqId(
				cfg,
				&atomic.Value{},
				proxyConfiguration.Spec.ProxyServer.Namespace,
				common.LabelKeyComponentName+"="+common.ComponentNameProxyServer, // TODO: configurable label selector?
				8090,
				200,
			)

			ctx, cancel := context.WithCancel(context.TODO())
			defer cancel()

			closeFn, err := localProxy.Listen(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer closeFn()

			mungledRestConfig := buildTunnelRestConfig(ctx, f, proxyConfiguration)
			mungledRestConfig.Timeout = time.Second * 10
			nativeClient, err := kubernetes.NewForConfig(mungledRestConfig)
			Expect(err).NotTo(HaveOccurred())
			data, err := nativeClient.RESTClient().Get().AbsPath("/healthz").DoRaw(context.TODO())
			return string(data) == "ok", err
		}).WithTimeout(time.Minute).WithPolling(time.Second * 10)
}
func buildTunnelRestConfig(ctx context.Context, f framework.Framework, proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration) *rest.Config {
	hubRestConfig := f.HubRESTConfig()
	tunnelTlsCfg, err := util.GetKonnectivityTLSConfig(hubRestConfig, proxyConfiguration)
	Expect(err).NotTo(HaveOccurred())

	tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		ctx,
		net.JoinHostPort("127.0.0.1", "8090"),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tunnelTlsCfg)),
	)
	Expect(err).NotTo(HaveOccurred())

	mungledRestConfig := rest.CopyConfig(hubRestConfig)
	mungledRestConfig.TLSClientConfig = rest.TLSClientConfig{
		Insecure: true,
	}
	mungledRestConfig.Host = f.TestClusterName()
	mungledRestConfig.Dial = tunnel.DialContext
	return mungledRestConfig
}

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
