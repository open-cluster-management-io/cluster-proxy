package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
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
	managedclusterset   = "default"
	managedclusterName  = "loopback"
)

const (
	timeout  = time.Second * 30
	interval = time.Second * 10
)

var _ = Describe("Connectivity Test", func() {
	f := framework.NewE2EFramework(connectTestBasename)

	It("should eventually connect to a kube-apiserver and hello-world service on managed cluster", func() {
		var err error

		err = deployHelleWorldApplication(context.Background(), serviceName, serviceNamespace, f)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		err = f.DeployClusterSetAndBinding(context.Background(), managedclusterset, "default")
		Expect(err).NotTo(HaveOccurred())

		err = deployMPSR(context.Background(), serviceName, serviceName, serviceNamespace, managedclusterset, f)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		Eventually(func() error {
			if err := probeHealth(f); err != nil {
				return err
			}
			if err := checkWithSelfPordforward(f); err != nil {
				return err
			}
			return nil
		}, 20*timeout, 3*interval).Should(Succeed()) // This has to be more than 6 mins because agent render every 5 mins.
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

	mungledRestConfig, err := buildTunnelRestConfig(ctx, f, proxyConfiguration)
	if err != nil {
		return err
	}
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

func buildTunnelRestConfig(ctx context.Context, f framework.Framework, proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration) (*rest.Config, error) {
	hubRestConfig := f.HubRESTConfig()
	tunnelTlsCfg, err := util.GetKonnectivityTLSConfig(hubRestConfig, proxyConfiguration)
	if err != nil {
		return nil, err
	}

	tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		ctx,
		net.JoinHostPort("127.0.0.1", "8090"),
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tunnelTlsCfg)),
	)
	if err != nil {
		return nil, err
	}

	mungledRestConfig := rest.CopyConfig(hubRestConfig)
	mungledRestConfig.TLSClientConfig = rest.TLSClientConfig{
		Insecure: true,
	}
	mungledRestConfig.Host = f.TestClusterName()
	mungledRestConfig.Dial = tunnel.DialContext
	return mungledRestConfig, nil
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
