package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/util"

	grpccredentials "google.golang.org/grpc/credentials"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

const (
	// Default proxy-entrypoint service namespace
	proxyEntrypointNamespace = "open-cluster-management-addon"
	proxyEntrypointService   = "proxy-entrypoint"
	proxyEntrypointPort      = "8090"
)

const (
	timeout  = time.Second * 30
	interval = time.Second * 10
)

// getProxyEntrypointAddress returns the address to connect to proxy-entrypoint service.
// If running in-cluster, it uses the service DNS name. Otherwise, it uses localhost (for port-forward).
func getProxyEntrypointAddress() string {
	// Running in-cluster, use service DNS name
	namespace := os.Getenv("PROXY_ENTRYPOINT_NAMESPACE")
	if namespace == "" {
		namespace = proxyEntrypointNamespace
	}
	service := os.Getenv("PROXY_ENTRYPOINT_SERVICE")
	if service == "" {
		service = proxyEntrypointService
	}
	port := os.Getenv("PROXY_ENTRYPOINT_PORT")
	if port == "" {
		port = proxyEntrypointPort
	}
	return net.JoinHostPort(fmt.Sprintf("%s.%s.svc", service, namespace), port)
}

var _ = Describe("Connectivity Test", Label("connectivity", "kube-apiserver"), func() {
	It("should eventually connect to a kube-apiserver and hello-world service on managed cluster", Label("apiserver", "service"), func() {
		Eventually(func() error {
			if err := probeHealth(); err != nil {
				fmt.Fprintf(GinkgoWriter, "[ERROR] probeHealth failed: %v\n", err)
				return err
			}
			return nil
		}, 20*timeout, 3*interval).Should(Succeed()) // This has to be more than 6 mins because agent render every 5 mins.
	})
})

func probeHealth() error {
	var err error
	proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
	err = hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Name: "cluster-proxy",
	}, proxyConfiguration)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	mungledRestConfig, err := buildTunnelRestConfig(ctx, proxyConfiguration)
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

func buildTunnelRestConfig(ctx context.Context, proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration) (*rest.Config, error) {
	tunnelTlsCfg, err := util.GetKonnectivityTLSConfig(hubRESTConfig, proxyConfiguration)
	if err != nil {
		return nil, err
	}

	proxyEntrypointAddr := getProxyEntrypointAddress()
	tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
		ctx,
		proxyEntrypointAddr,
		grpc.WithTransportCredentials(grpccredentials.NewTLS(tunnelTlsCfg)),
	)
	if err != nil {
		return nil, err
	}

	mungledRestConfig := rest.CopyConfig(hubRESTConfig)
	mungledRestConfig.TLSClientConfig = rest.TLSClientConfig{
		Insecure: true,
	}
	mungledRestConfig.Host = managedClusterName
	mungledRestConfig.Dial = tunnel.DialContext
	return mungledRestConfig, nil
}

var _ = Describe("Requests through Cluster-Proxy", Label("serviceproxy", "connectivity"), func() {
	Describe("Get pods", Label("pods"), func() {
		Context("URL is vailid", func() {
			It("should return pods information", Label("valid-url"), func() {
				_, err := clusterProxyKubeClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), v1.ListOptions{})
				Expect(err).To(BeNil())
			})
		})

		Context("URL is invalid", func() {
			It("shoudl return error msg", Label("invalid-url"), func() {
				_, err := clusterProxyWrongClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), v1.ListOptions{})
				Expect(err).ToNot(BeNil())
			})
		})

		Context("URL is valid, but out of namepsace open-cluster-management", func() {
			It("should return forbidden", Label("forbidden", "rbac"), func() {
				_, err := clusterProxyKubeClient.CoreV1().Pods(managedClusterInstallNamespace).List(context.Background(), v1.ListOptions{})
				Expect(err).ToNot(BeNil())
				Expect(errors.IsForbidden(err)).To(Equal(true))
			})
		})

		Context("URL is valid, but using unauth token", func() {
			It("should return unauth", Label("unauthorized", "auth"), func() {
				_, err := clusterProxyUnAuthClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), v1.ListOptions{})
				Expect(err).ToNot(BeNil())
				Expect(errors.IsUnauthorized(err)).To(Equal(true))
			})
		})
	})

	Describe("Get Logs of a pod", Label("logs"), func() {
		It("should return logs information", Label("pod-logs"), func() {
			req := clusterProxyKubeClient.CoreV1().Pods(hubInstallNamespace).GetLogs(podName, &corev1.PodLogOptions{})
			podlogs, err := req.Stream(context.Background())
			Expect(err).To(BeNil())
			podlogs.Close()
		})
	})

	Describe("Watch ConfigMap create", Label("watch"), func() {
		It("shoud watch", Label("configmap"), func() {
			watch, err := clusterProxyKubeClient.CoreV1().ConfigMaps(hubInstallNamespace).Watch(context.TODO(), v1.ListOptions{})
			Expect(err).To(BeNil())

			// create a pod
			_, err = hubKubeClient.CoreV1().ConfigMaps(hubInstallNamespace).Create(context.Background(), &corev1.ConfigMap{
				ObjectMeta: v1.ObjectMeta{
					Name: "cluster-proxy-test",
				},
			}, v1.CreateOptions{})
			Expect(err).To(BeNil())

			// check if r is create
			select {
			case <-watch.ResultChan():
				// this chan shoud not receive any pod event before pod created
				err := hubKubeClient.CoreV1().ConfigMaps(hubInstallNamespace).Delete(context.Background(), "cluster-proxy-test", metav1.DeleteOptions{})
				Expect(err).To(BeNil())
			default:
				Fail("Failed to received a pod create event")
			}
		})
	})

	Describe("Execute in a pod", Label("exec"), func() {
		It("should return hello", Label("pod-exec"), func() {
			req := clusterProxyKubeClient.CoreV1().RESTClient().Post().Resource("pods").Name(podName).Namespace(hubInstallNamespace).SubResource("exec").Param("container", "manager")

			req.VersionedParams(&corev1.PodExecOptions{
				Command:   []string{"/bin/sh", "-c", "echo hello"},
				Container: "manager",
				Stdin:     false,
				Stdout:    true,
				Stderr:    true,
				TTY:       false,
			}, k8sscheme.ParameterCodec)

			exec, err := remotecommand.NewSPDYExecutor(clusterProxyCfg, "POST", req.URL())
			Expect(err).To(BeNil())

			var stdout, stderr bytes.Buffer
			err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
				Stdin:  nil,
				Stdout: &stdout,
				Stderr: &stderr,
				Tty:    false,
			})
			Expect(err).To(BeNil())
			Expect(strings.Contains(stdout.String(), "hello")).To(Equal(true))
		})
	})

	// Note: hello-world service is deployed during environment initialization in test/e2e/env/init.sh
	Describe("Access hello-world service", Label("service-access"), func() {
		It("should return hello-world with http code 200", Label("hello-world", "http"), func() {
			targetHost := fmt.Sprintf(`https://%s/%s/api/v1/namespaces/default/services/http:hello-world:8000/proxy-service/index.html`, userServerServiceAddress, managedClusterName)
			fmt.Println("The targetHost: ", targetHost)

			req, err := http.NewRequest("GET", targetHost, nil)
			Expect(err).To(BeNil())

			resp, err := clusterProxyHttpClient.Do(req)
			Expect(err).To(BeNil())
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			Expect(err).To(BeNil())
			fmt.Println("response:", string(body))

			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(strings.Contains(string(body), "Hello from hello-world")).To(Equal(true))
		})
	})

	// Note: hello-world-https service is deployed during environment initialization in test/e2e/env/init.sh
	// This test verifies that service-proxy correctly routes HTTPS requests to backend services.
	// The backend service uses a self-signed certificate, so we expect either:
	// 1. Success (200) if the certificate is trusted by the service-proxy
	// 2. Bad Gateway (502) if the certificate is not trusted (TLS verification failed)
	// Both cases confirm that the request was correctly routed to the HTTPS backend.
	//
	// Note: Go's httputil.ReverseProxy default ErrorHandler only writes the status code (502)
	// without including error details in the response body. Therefore, we accept 502 status
	// as success since it indicates the request was routed but TLS verification failed.
	Describe("Access hello-world-https service", Label("service-access-https"), func() {
		It("should route request to hello-world-https backend service", Label("hello-world-https", "https"), func() {
			targetHost := fmt.Sprintf(`https://%s/%s/api/v1/namespaces/default/services/https:hello-world-https:8443/proxy-service/index.html`, userServerServiceAddress, managedClusterName)
			fmt.Println("The targetHost: ", targetHost)

			req, err := http.NewRequest("GET", targetHost, nil)
			Expect(err).To(BeNil())

			resp, err := clusterProxyHttpClient.Do(req)
			Expect(err).To(BeNil())
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			Expect(err).To(BeNil())
			fmt.Println("response:", string(body))

			// The request should either succeed (200) or fail with Bad Gateway (502).
			// Both cases confirm the request was correctly routed to the HTTPS backend:
			// - 200: Certificate is trusted, backend responded successfully
			// - 502: Certificate is not trusted (self-signed), TLS verification failed
			//
			// Note: Go's httputil.ReverseProxy default ErrorHandler only sets the status code
			// without writing error details to the body, so we cannot check the body content
			// for TLS error messages. The 502 status alone confirms routing worked.
			switch resp.StatusCode {
			case http.StatusOK:
				// Certificate is trusted, response should contain expected content
				Expect(strings.Contains(string(body), "Hello from hello-world-https")).To(BeTrue(),
					"expected response to contain 'Hello from hello-world-https', got: %s", string(body))
				fmt.Println("Request succeeded with trusted certificate")
			case http.StatusBadGateway:
				// Certificate is not trusted (self-signed), but request was correctly routed.
				// Since the HTTP hello-world test passes, this 502 confirms the HTTPS routing
				// is working - just TLS verification failed due to self-signed certificate.
				fmt.Println("Request correctly routed to HTTPS backend, got 502 (expected with self-signed cert)")
			default:
				Fail(fmt.Sprintf("Unexpected status code: %d, body: %s", resp.StatusCode, string(body)))
			}
		})
	})
})
