package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/util"

	grpccredentials "google.golang.org/grpc/credentials"
	clusterproxyclient "open-cluster-management.io/cluster-proxy/client"
	"open-cluster-management.io/cluster-proxy/test/e2e/framework"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

const (
	connectTestBasename = "connect"
	serviceName         = "hello-world"
	serviceNamespace    = "default"
	managedclusterset   = "test"
	managedclusterName  = "loopback"
)

const (
	timeout  = time.Second * 30
	interval = time.Second * 10
)

var _ = Describe("Connectivity Test", func() {
	f := framework.NewE2EFramework(connectTestBasename)

	c := f.HubRuntimeClient()
	proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
	err := c.Get(context.TODO(), types.NamespacedName{
		Name: "cluster-proxy",
	}, proxyConfiguration)
	Expect(err).NotTo(HaveOccurred())

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	cfg := f.HubRESTConfig()
	By("Running local port-forward stream to proxy service")

	localProxy := util.NewRoundRobinLocalProxyWithReqId(
		cfg,
		&atomic.Value{},
		proxyConfiguration.Spec.ProxyServer.Namespace,
		common.LabelKeyComponentName+"="+common.ComponentNameProxyServer, // TODO: configurable label selector?
		8090,
		200,
	)

	closeFn, err := localProxy.Listen(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer closeFn()

	It("should return health when probe managed cluster kube-apiserver", func() {
		Eventually(func() error {
			return probeHealth(f)
		}, 6*timeout, 3*interval).Should(Succeed())
	})

	It("should eventually connect to a hello-world/default service", func() {
		var err error

		err = deployHelleWorldApplication(context.Background(), serviceName, serviceNamespace, f)
		Expect(err).NotTo(HaveOccurred())

		err = deployMCS(context.Background(), managedclusterset, f)
		Expect(err).NotTo(HaveOccurred())

		err = attachMCS(context.Background(), managedclusterName, managedclusterset, f)
		Expect(err).NotTo(HaveOccurred())

		err = deployMPSR(context.Background(), serviceName, serviceName, serviceNamespace, managedclusterset, f)
		Expect(err).NotTo(HaveOccurred())

		// scalability test
		for i := 0; i < 100; i++ {
			name := fmt.Sprintf("%s-%d", serviceName, i)
			err = deployMPSR(context.Background(), name, name, serviceNamespace, managedclusterset, f)
			Expect(err).NotTo(HaveOccurred())
		}

		Eventually(func() error {
			return checkWithSelfPordforward(f)
		}, 6*timeout, 3*interval).Should(Succeed())
	})
})

func probeHealth(f framework.Framework) error {
	var err error
	c := f.HubRuntimeClient()
	proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
	err = c.Get(context.TODO(), types.NamespacedName{
		Name: "cluster-proxy",
	}, proxyConfiguration)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	mungledRestConfig := buildTunnelRestConfig(ctx, f, proxyConfiguration)
	mungledRestConfig.Timeout = time.Second * 10
	nativeClient, err := kubernetes.NewForConfig(mungledRestConfig)
	if err != nil {
		return err
	}

	data, err := nativeClient.RESTClient().Get().AbsPath("/healthz").DoRaw(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get healthz: %w", err)
	}
	if string(data) != "ok" {
		return fmt.Errorf("unexpected healthz response: %s", string(data))
	}
	return nil
}

func checkWithSelfPordforward(f framework.Framework) error {
	var err error
	c := f.HubRuntimeClient()
	proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
	err = c.Get(context.TODO(), types.NamespacedName{
		Name: "cluster-proxy",
	}, proxyConfiguration)
	if err != nil {
		return err
	}

	hubRestConfig := f.HubRESTConfig()
	tunnelTlsCfg, err := util.GetKonnectivityTLSConfig(hubRestConfig, proxyConfiguration)
	if err != nil {
		return err
	}

	proxyDialer, err := konnectivity.CreateSingleUseGrpcTunnel(
		context.Background(),
		net.JoinHostPort("127.0.0.1", "8090"),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tunnelTlsCfg)),
	)
	if err != nil {
		return err
	}

	tr := &http.Transport{
		DialContext:         proxyDialer.DialContext,
		TLSHandshakeTimeout: 2 * time.Second,
	}
	client := http.Client{Transport: tr}

	proxyHost, err := clusterproxyclient.GetProxyHost(context.Background(), hubRestConfig, managedclusterName, serviceNamespace, serviceName)
	if err != nil {
		return err
	}

	resp, err := client.Get(fmt.Sprintf("http://%s:8000", proxyHost))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if string(content) != "Hello from hello-world\n" {
		return fmt.Errorf("response:%s", string(content))
	}
	return nil
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

func deployHelleWorldApplication(ctx context.Context, name, namespace string, e2eframe framework.Framework) error {
	// Because in e2e test, hub is self-managed, so we can user hubClient to deploy the application.
	hubClient := e2eframe.HubNativeClient()

	var err error

	// Create pod
	_, err = hubClient.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"run": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  name,
					Image: "quay.io/prometheus/busybox",
					Args: []string{
						"sh",
						"-c",
						"echo 'Hello from " + name + "' > /var/www/index.html && httpd -f -p 8000 -h /var/www/",
					},
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 8000,
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Create service
	_, err = hubClient.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"run": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"run": name,
			},
			Ports: []corev1.ServicePort{
				{
					Port: 8000,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func deployMCS(ctx context.Context, clusterset string, e2eframe framework.Framework) error {
	return e2eframe.HubRuntimeClient().Create(ctx, &clusterv1beta1.ManagedClusterSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterset,
		},
	})
}

func attachMCS(ctx context.Context, clustername string, managedclusterset string, e2eframe framework.Framework) error {
	managedcluster := &clusterv1.ManagedCluster{}

	var err error
	err = e2eframe.HubRuntimeClient().Get(ctx, client.ObjectKey{Name: clustername}, managedcluster)
	if err != nil {
		return err
	}

	// Add managedclusterset label
	managedcluster.Labels[clusterv1beta1.ClusterSetLabel] = managedclusterset

	return e2eframe.HubRuntimeClient().Update(ctx, managedcluster)
}

func deployMPSR(ctx context.Context, name string, serviceName string, serviceNamespace string, managedclusterSet string, e2eframe framework.Framework) error {
	return e2eframe.HubRuntimeClient().Create(ctx, &proxyv1alpha1.ManagedProxyServiceResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
			ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
				Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
				ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
					Name: managedclusterSet,
				},
			},
			ServiceSelector: proxyv1alpha1.ServiceSelector{
				Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
				ServiceRef: &proxyv1alpha1.ServiceRef{
					Name:      serviceName,
					Namespace: serviceNamespace,
				},
			}},
	})
}
