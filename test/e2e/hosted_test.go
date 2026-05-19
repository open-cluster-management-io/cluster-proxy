package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"

	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
)

const (
	sourceKubeconfigHashAnnotation = "proxy.open-cluster-management.io/source-kubeconfig-hash"
	managedKubeconfigSecretName    = "cluster-proxy-managed-kubeconfig"
	serviceRelayName               = "cluster-proxy-service-relay"
)

var _ = Describe("Hosted Mode", Label("hosted"), Ordered, func() {
	BeforeAll(func() {
		if !hostedMode {
			Skip("hosted mode kubeconfigs are not configured")
		}
	})

	It("should split hosted resources across hosting and managed clusters", Label("hosted-relay", "deployment"), func() {
		By("Checking hosting cluster resources")
		hostingDeploy := getDeployment(hostingKubeClient, managedClusterInstallNamespace, "cluster-proxy-proxy-agent")
		Expect(containerNames(hostingDeploy)).To(ContainElements(
			"proxy-agent",
			"addon-agent",
			"service-proxy",
			"managed-apiserver-proxy",
		))
		Expect(deploymentHasVolume(hostingDeploy, "managed-kubeconfig")).To(BeTrue())
		Expect(containerHasVolumeMount(hostingDeploy, "proxy-agent", "/etc/managed")).To(BeFalse())
		Expect(containerHasVolumeMount(hostingDeploy, "addon-agent", "/etc/managed")).To(BeTrue())
		Expect(containerHasVolumeMount(hostingDeploy, "service-proxy", "/etc/managed")).To(BeTrue())
		Expect(containerHasVolumeMount(hostingDeploy, "managed-apiserver-proxy", "/etc/managed")).To(BeTrue())

		getDeployment(hostingKubeClient, managedClusterInstallNamespace, "cluster-proxy-managed-kubeconfig-provisioner")
		_, err := hostingKubeClient.CoreV1().Secrets(managedClusterInstallNamespace).Get(
			context.Background(), managedKubeconfigSecretName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		_, err = hostingKubeClient.RbacV1().Roles(managedClusterInstallNamespace).Get(
			context.Background(), "cluster-proxy-addon-agent", metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		_, err = hostingKubeClient.RbacV1().RoleBindings(managedClusterInstallNamespace).Get(
			context.Background(), "cluster-proxy-addon-agent", metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		_, err = hostingKubeClient.CoreV1().Services(managedClusterInstallNamespace).Get(
			context.Background(), managedClusterName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		expectDeploymentNotFound(hostingKubeClient, managedClusterInstallNamespace, serviceRelayName)

		By("Checking managed cluster resources")
		managedRelay := getDeployment(managedKubeClient, managedClusterInstallNamespace, serviceRelayName)
		Expect(containerNames(managedRelay)).To(ContainElement("service-relay"))
		Expect(managedRelay.Spec.Template.Spec.AutomountServiceAccountToken).ToNot(BeNil())
		Expect(*managedRelay.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(deploymentHasVolume(managedRelay, "managed-kubeconfig")).To(BeFalse())
		_, err = managedKubeClient.CoreV1().ServiceAccounts(managedClusterInstallNamespace).Get(
			context.Background(), "cluster-proxy", metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		_, err = managedKubeClient.RbacV1().Roles(managedClusterInstallNamespace).Get(
			context.Background(), "cluster-proxy-service-relay-proxy", metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		expectRoleNotFound(managedKubeClient, managedClusterInstallNamespace, "cluster-proxy-addon-agent")
		_, err = managedKubeClient.CoreV1().Services(managedClusterInstallNamespace).Get(
			context.Background(), serviceRelayName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		expectDeploymentNotFound(managedKubeClient, managedClusterInstallNamespace, "cluster-proxy-proxy-agent")
		expectDeploymentNotFound(managedKubeClient, managedClusterInstallNamespace, "cluster-proxy-managed-kubeconfig-provisioner")
		_, err = managedKubeClient.CoreV1().Secrets(managedClusterInstallNamespace).Get(
			context.Background(), managedKubeconfigSecretName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("should provision and refresh the managed kubeconfig", Label("hosted-relay", "managed-kubeconfig"), func() {
		By("Checking generated managed kubeconfig Secret")
		generated := getGeneratedManagedKubeconfig()
		Expect(generated.Data).To(HaveKey("kubeconfig"))
		Expect(generated.Annotations).To(HaveKey(sourceKubeconfigHashAnnotation))
		originalHash := generated.Annotations[sourceKubeconfigHashAnnotation]
		originalResourceVersion := generated.ResourceVersion

		By("Checking ManagedKubeconfigReady condition")
		addon := &addonapiv1alpha1.ManagedClusterAddOn{}
		Expect(hubRuntimeClient.Get(context.Background(), types.NamespacedName{
			Namespace: managedClusterName,
			Name:      "cluster-proxy",
		}, addon)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(addon.Status.Conditions, "ManagedKubeconfigReady")).To(BeTrue())

		By("Changing source kubeconfig data and waiting for refresh")
		sourceSecretName := envOrDefault("E2E_HOSTED_EXTERNAL_KUBECONFIG_SECRET", "external-managed-kubeconfig")
		source, err := hostingKubeClient.CoreV1().Secrets(managedClusterName).Get(
			context.Background(), sourceSecretName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		source = source.DeepCopy()
		source.Data["kubeconfig"] = append(source.Data["kubeconfig"], []byte(fmt.Sprintf("\n# e2e-refresh=%d\n", time.Now().UnixNano()))...)
		_, err = hostingKubeClient.CoreV1().Secrets(managedClusterName).Update(context.Background(), source, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())

		Eventually(func() error {
			refreshed := getGeneratedManagedKubeconfig()
			if refreshed.ResourceVersion == originalResourceVersion {
				return fmt.Errorf("generated secret resourceVersion has not changed")
			}
			if refreshed.Annotations[sourceKubeconfigHashAnnotation] == originalHash {
				return fmt.Errorf("source kubeconfig hash has not changed")
			}
			return nil
		}, time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should proxy kube-apiserver requests with hub and managed tokens", Label("hosted-relay", "kube-apiserver"), func() {
		By("Checking raw konnectivity tunnel health")
		Expect(probeHealth()).To(Succeed())

		By("Checking hub token impersonation")
		_, err := clusterProxyKubeClient.CoreV1().Pods(targetNamespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		req := clusterProxyKubeClient.CoreV1().Pods(targetNamespace).GetLogs(podName, &corev1.PodLogOptions{})
		logs, err := req.Stream(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(logs.Close()).To(Succeed())

		stdout := execThroughClusterProxy(clusterProxyCfg)
		Expect(stdout).To(ContainSubstring("hello"))

		body := portForwardThroughClusterProxy()
		Expect(body).To(ContainSubstring("Hello from hello-world"))

		By("Checking managed token authentication")
		_, err = clusterProxyManagedClient.CoreV1().Pods(targetNamespace).List(context.Background(), metav1.ListOptions{})
		Expect(err).ToNot(HaveOccurred())

		By("Checking RBAC failures")
		_, err = clusterProxyKubeClient.CoreV1().Pods("kube-system").List(context.Background(), metav1.ListOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
		_, err = clusterProxyManagedClient.CoreV1().Pods("kube-system").List(context.Background(), metav1.ListOptions{})
		Expect(apierrors.IsForbidden(err)).To(BeTrue())

		By("Checking invalid tokens are rejected")
		_, err = clusterProxyUnAuthClient.CoreV1().Pods(targetNamespace).List(context.Background(), metav1.ListOptions{})
		Expect(apierrors.IsUnauthorized(err)).To(BeTrue())
	})

	It("should proxy HTTP and HTTPS services through Relay and expose metrics", Label("hosted-relay", "serviceproxy"), func() {
		statusCode, body := requestServiceThroughUserServer("http", "hello-world", 8000)
		Expect(statusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("Hello from hello-world"))

		statusCode, body = requestServiceThroughUserServer("https", "hello-world-https", 8443)
		Expect(statusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("Hello from hello-world-https"))

		Eventually(func() string {
			return metricsFromPod(hostingRESTConfig, hostingKubeClient, managedClusterInstallNamespace,
				"proxy.open-cluster-management.io/component-name=proxy-agent")
		}, time.Minute, 5*time.Second).Should(ContainSubstring("cluster_proxy_service_proxy_requests_total"))

		Eventually(func() string {
			return metricsFromPod(managedRESTConfig, managedKubeClient, managedClusterInstallNamespace,
				"proxy.open-cluster-management.io/component-name=service-relay")
		}, time.Minute, 5*time.Second).Should(ContainSubstring("cluster_proxy_service_relay_requests_total"))
	})

	It("should run BestEffort service proxy when explicitly requested", Label("hosted-besteffort"), func() {
		if os.Getenv("RUN_HOSTED_BESTEFFORT") != "true" {
			Skip("RUN_HOSTED_BESTEFFORT=true is required")
		}

		patchAddOnDeploymentConfigVariable("hostedServiceProxyMode", "BestEffort")
		waitServiceProxyMode("BestEffort")

		statusCode, body := requestServiceThroughUserServer("http", "hello-world", 8000)
		Expect(statusCode).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("Hello from hello-world"))
	})

	It("should clean generated managed kubeconfig resources when the addon is deleted", Label("hosted-relay", "cleanup"), func() {
		By("Removing the managed cluster from placement")
		labelKey := envOrDefault("E2E_HOSTED_PLACEMENT_LABEL_KEY", "cluster-proxy-e2e")
		Eventually(func() error {
			cluster, err := hubClusterClient.ClusterV1().ManagedClusters().Get(context.Background(), managedClusterName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			cluster = cluster.DeepCopy()
			delete(cluster.Labels, labelKey)
			_, err = hubClusterClient.ClusterV1().ManagedClusters().Update(context.Background(), cluster, metav1.UpdateOptions{})
			return err
		}, time.Minute, 5*time.Second).Should(Succeed())

		By("Deleting the ManagedClusterAddOn")
		err := hubRuntimeClient.Delete(context.Background(), &addonapiv1alpha1.ManagedClusterAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: managedClusterName,
				Name:      "cluster-proxy",
			},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		Eventually(func() bool {
			_, err := hostingKubeClient.CoreV1().Secrets(managedClusterInstallNamespace).Get(
				context.Background(), managedKubeconfigSecretName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 5*time.Minute, 5*time.Second).Should(BeTrue())

		Eventually(func() bool {
			_, err := hostingKubeClient.BatchV1().Jobs(managedClusterInstallNamespace).Get(
				context.Background(), "cluster-proxy-managed-kubeconfig-cleanup", metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 5*time.Minute, 5*time.Second).Should(BeTrue())

		Eventually(func() bool {
			addon := &addonapiv1alpha1.ManagedClusterAddOn{}
			err := hubRuntimeClient.Get(context.Background(), types.NamespacedName{
				Namespace: managedClusterName,
				Name:      "cluster-proxy",
			}, addon)
			return apierrors.IsNotFound(err)
		}, 5*time.Minute, 5*time.Second).Should(BeTrue())
	})
})

func getDeployment(kubeClient kubernetes.Interface, namespace, name string) *appsv1.Deployment {
	deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	return deploy
}

func expectDeploymentNotFound(kubeClient kubernetes.Interface, namespace, name string) {
	_, err := kubeClient.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected deployment %s/%s to be absent, got %v", namespace, name, err)
}

func expectRoleNotFound(kubeClient kubernetes.Interface, namespace, name string) {
	_, err := kubeClient.RbacV1().Roles(namespace).Get(context.Background(), name, metav1.GetOptions{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected role %s/%s to be absent, got %v", namespace, name, err)
}

func containerNames(deploy *appsv1.Deployment) []string {
	names := make([]string, 0, len(deploy.Spec.Template.Spec.Containers))
	for _, container := range deploy.Spec.Template.Spec.Containers {
		names = append(names, container.Name)
	}
	return names
}

func containerHasVolumeMount(deploy *appsv1.Deployment, containerName, mountPath string) bool {
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == mountPath {
				return true
			}
		}
	}
	return false
}

func deploymentHasVolume(deploy *appsv1.Deployment, name string) bool {
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func getGeneratedManagedKubeconfig() *corev1.Secret {
	secret, err := hostingKubeClient.CoreV1().Secrets(managedClusterInstallNamespace).Get(
		context.Background(), managedKubeconfigSecretName, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred())
	return secret
}

func execThroughClusterProxy(config *rest.Config) string {
	req := clusterProxyKubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(targetNamespace).
		SubResource("exec").
		Param("container", podContainerName)

	req.VersionedParams(&corev1.PodExecOptions{
		Command:   []string{"/bin/sh", "-c", "echo hello"},
		Container: podContainerName,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, k8sscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	Expect(err).ToNot(HaveOccurred())

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	Expect(err).ToNot(HaveOccurred(), stderr.String())
	return stdout.String()
}

func portForwardThroughClusterProxy() string {
	return getViaPortForward(clusterProxyCfg, clusterProxyKubeClient, targetNamespace, podName, podPort, "/index.html")
}

func requestServiceThroughUserServer(proto, service string, port int) (int, string) {
	targetHost := fmt.Sprintf(
		"https://%s/%s/api/v1/namespaces/default/services/%s:%s:%d/proxy-service/index.html",
		userServerServiceAddress,
		managedClusterName,
		proto,
		service,
		port,
	)

	req, err := http.NewRequest("GET", targetHost, nil)
	Expect(err).ToNot(HaveOccurred())
	resp, err := clusterProxyHttpClient.Do(req)
	Expect(err).ToNot(HaveOccurred())
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())
	return resp.StatusCode, string(body)
}

func metricsFromPod(restConfig *rest.Config, kubeClient kubernetes.Interface, namespace, selector string) string {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	Expect(err).ToNot(HaveOccurred())
	Expect(pods.Items).ToNot(BeEmpty())

	pod := pods.Items[0]
	return getViaPortForward(restConfig, kubeClient, namespace, pod.Name, 8000, "/metrics")
}

func getViaPortForward(restConfig *rest.Config, kubeClient kubernetes.Interface, namespace, pod string, remotePort int, path string) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).ToNot(HaveOccurred())
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	var requestID int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				errCh <- err
				return
			}
			id := atomic.AddInt32(&requestID, 1)
			go func() {
				if err := forwardPortForwardConnection(restConfig, kubeClient, namespace, pod, remotePort, int(id), conn); err != nil {
					errCh <- err
				}
			}()
		}
	}()

	localPort := listener.Addr().(*net.TCPAddr).Port
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", localPort, path))
	Expect(err).ToNot(HaveOccurred())
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	Expect(err).ToNot(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	select {
	case err := <-errCh:
		Expect(err).ToNot(HaveOccurred())
	default:
	}
	return string(body)
}

func forwardPortForwardConnection(
	restConfig *rest.Config,
	kubeClient kubernetes.Interface,
	namespace, pod string,
	remotePort int,
	requestID int,
	conn net.Conn,
) error {
	defer conn.Close()

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return err
	}
	req := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())
	streamConn, _, err := dialer.Dial("portforward.k8s.io")
	if err != nil {
		return err
	}
	defer streamConn.Close()

	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(remotePort))
	headers.Set(corev1.PortForwardRequestIDHeader, strconv.Itoa(requestID))
	errorStream, err := streamConn.CreateStream(headers)
	if err != nil {
		return err
	}
	errorStream.Close()

	errorCh := make(chan error, 1)
	go func() {
		message, err := io.ReadAll(errorStream)
		switch {
		case err != nil:
			errorCh <- err
		case len(message) > 0:
			errorCh <- fmt.Errorf("port-forward error: %s", string(message))
		default:
			errorCh <- nil
		}
	}()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := streamConn.CreateStream(headers)
	if err != nil {
		return err
	}
	defer dataStream.Close()

	remoteDone := make(chan error, 1)
	localDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, dataStream)
		remoteDone <- err
	}()
	go func() {
		_, err := io.Copy(dataStream, conn)
		localDone <- err
	}()

	select {
	case err := <-errorCh:
		return err
	case <-remoteDone:
		return nil
	case <-localDone:
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timed out waiting for port-forward data")
	}
}

func patchAddOnDeploymentConfigVariable(name, value string) {
	configName := envOrDefault("E2E_HOSTED_DEPLOY_CONFIG_NAME", "hosted-relay")
	Eventually(func() error {
		config := &addonapiv1alpha1.AddOnDeploymentConfig{}
		if err := hubRuntimeClient.Get(context.Background(), types.NamespacedName{
			Namespace: managedClusterName,
			Name:      configName,
		}, config); err != nil {
			return err
		}
		config = config.DeepCopy()
		found := false
		for i := range config.Spec.CustomizedVariables {
			if config.Spec.CustomizedVariables[i].Name == name {
				config.Spec.CustomizedVariables[i].Value = value
				found = true
				break
			}
		}
		if !found {
			config.Spec.CustomizedVariables = append(config.Spec.CustomizedVariables, addonapiv1alpha1.CustomizedVariable{
				Name:  name,
				Value: value,
			})
		}
		return hubRuntimeClient.Update(context.Background(), config)
	}, time.Minute, 5*time.Second).Should(Succeed())
}

func waitServiceProxyMode(mode string) {
	expectedArg := "--hosted-service-proxy-mode=" + mode
	Eventually(func() error {
		deploy := getDeployment(hostingKubeClient, managedClusterInstallNamespace, "cluster-proxy-proxy-agent")
		for _, container := range deploy.Spec.Template.Spec.Containers {
			if container.Name == "service-proxy" && stringSliceContains(container.Args, expectedArg) {
				return nil
			}
		}
		return fmt.Errorf("service-proxy does not contain arg %q", expectedArg)
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func envOrDefault(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}
