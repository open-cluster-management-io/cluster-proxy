package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterclient "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

func TestE2E(t *testing.T) {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	RegisterFailHandler(Fail)
	RunSpecs(t, "ClusterProxy E2E suite")
}

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(proxyv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.Install(scheme))
	utilruntime.Must(clusterv1beta2.Install(scheme))
	utilruntime.Must(clusterv1beta1.Install(scheme))
	utilruntime.Must(addonv1alpha1.Install(scheme))
	utilruntime.Must(k8sscheme.AddToScheme(scheme))
}

var (
	managedClusterName         string
	hubRESTConfig              *rest.Config
	hostingRESTConfig          *rest.Config
	managedRESTConfig          *rest.Config
	hubKubeClient              kubernetes.Interface
	hostingKubeClient          kubernetes.Interface
	managedKubeClient          kubernetes.Interface
	hubRuntimeClient           client.Client
	hostingRuntimeClient       client.Client
	managedRuntimeClient       client.Client
	clusterProxyKubeClient     kubernetes.Interface
	clusterProxyManagedClient  kubernetes.Interface
	clusterProxyWrongClient    kubernetes.Interface
	clusterProxyUnAuthClient   kubernetes.Interface
	clusterProxyHttpClient     *http.Client
	hubAddOnClient             addonclient.Interface
	hubClusterClient           clusterclient.Interface
	clusterProxyCfg            *rest.Config
	serviceAccountToken        string
	managedServiceAccountToken string
	podName                    string
	podContainerName           string
	podPort                    int
	hostedMode                 bool
	targetNamespace            string
	targetKubeClient           kubernetes.Interface
	targetRuntimeClient        client.Client
)

const (
	eventuallyTimeout              = 600 // seconds
	eventuallyInterval             = 30  // seconds
	hubInstallNamespace            = "open-cluster-management-addon"
	managedClusterInstallNamespace = "open-cluster-management-cluster-proxy"
	serviceAccountName             = "cluster-proxy-test"
	managedServiceAccountName      = "cluster-proxy-managed-test"
	hostedTestPodName              = "hello-world"
	hostedTestPodContainerName     = "hello-world"
	hostedTestPodPort              = 8000
)

var _ = BeforeSuite(func() {
	managedClusterName = os.Getenv("MANAGED_CLUSTER_NAME")
	if managedClusterName == "" {
		managedClusterName = "loopback"
	}

	var err error
	By("Init clients")
	err = func() error {
		var err error
		hostedMode = os.Getenv("E2E_HOSTING_KUBECONFIG") != "" || os.Getenv("E2E_MANAGED_KUBECONFIG") != ""

		hubRESTConfig, err = configFromEnvOrInCluster("E2E_HUB_KUBECONFIG")
		if err != nil {
			return err
		}

		hubKubeClient, err = kubernetes.NewForConfig(hubRESTConfig)
		if err != nil {
			return err
		}

		hubRuntimeClient, err = client.New(hubRESTConfig, client.Options{
			Scheme: scheme,
		})
		if err != nil {
			return err
		}

		hubAddOnClient, err = addonclient.NewForConfig(hubRESTConfig)
		if err != nil {
			return err
		}

		hubClusterClient, err = clusterclient.NewForConfig(hubRESTConfig)
		if err != nil {
			return err
		}

		targetNamespace = hubInstallNamespace
		targetKubeClient = hubKubeClient
		targetRuntimeClient = hubRuntimeClient
		podContainerName = "manager"

		if hostedMode {
			hostingRESTConfig, err = configFromEnv("E2E_HOSTING_KUBECONFIG")
			if err != nil {
				return err
			}
			managedRESTConfig, err = configFromEnv("E2E_MANAGED_KUBECONFIG")
			if err != nil {
				return err
			}
			hostingKubeClient, err = kubernetes.NewForConfig(hostingRESTConfig)
			if err != nil {
				return err
			}
			managedKubeClient, err = kubernetes.NewForConfig(managedRESTConfig)
			if err != nil {
				return err
			}
			hostingRuntimeClient, err = client.New(hostingRESTConfig, client.Options{Scheme: scheme})
			if err != nil {
				return err
			}
			managedRuntimeClient, err = client.New(managedRESTConfig, client.Options{Scheme: scheme})
			if err != nil {
				return err
			}
			targetNamespace = "default"
			targetKubeClient = managedKubeClient
			targetRuntimeClient = managedRuntimeClient
			podName = hostedTestPodName
			podContainerName = hostedTestPodContainerName
			podPort = hostedTestPodPort
		}

		return err
	}()
	Expect(err).To(BeNil())

	checkAddonStatus()

	prepareTestServiceAccount()

	preparePodFortest()

	prepareClusterProxyClient()
})

func configFromEnv(envName string) (*rest.Config, error) {
	kubeconfig := os.Getenv(envName)
	if kubeconfig == "" {
		return nil, fmt.Errorf("%s is required", envName)
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func configFromEnvOrInCluster(envName string) (*rest.Config, error) {
	kubeconfig := os.Getenv(envName)
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func checkAddonStatus() {
	var err error

	By("Check resources are running")
	Eventually(func() error {
		// deployments on hub is running
		deployments := []string{
			"cluster-proxy-addon-manager",
			"cluster-proxy-addon-user",
			"cluster-proxy",
		}
		for _, deployment := range deployments {
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Checking deployment: %s in namespace: %s\n", deployment, hubInstallNamespace)
			d, err := hubKubeClient.AppsV1().Deployments(hubInstallNamespace).Get(context.Background(), deployment, metav1.GetOptions{})
			if err != nil {
				fmt.Fprintf(GinkgoWriter, "[ERROR] Failed to get deployment %s: %v\n", deployment, err)
				return err
			}
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Deployment %s status - Replicas: %d, Available: %d, Ready: %d, Updated: %d\n",
				deployment, d.Status.Replicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas)
			if d.Status.AvailableReplicas < 1 {
				errMsg := fmt.Errorf("available replicas for %s should >= 1, but get %d", deployment, d.Status.AvailableReplicas)
				fmt.Fprintf(GinkgoWriter, "[ERROR] %v\n", errMsg)
				return errMsg
			}
			fmt.Fprintf(GinkgoWriter, "[SUCCESS] Deployment %s is ready\n", deployment)
		}

		// service on hub exist
		fmt.Fprintf(GinkgoWriter, "[DEBUG] Checking service: cluster-proxy-addon-user in namespace: %s\n", hubInstallNamespace)
		_, err = hubKubeClient.CoreV1().Services(hubInstallNamespace).Get(context.Background(), "cluster-proxy-addon-user", metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "[ERROR] Failed to get service cluster-proxy-addon-user: %v\n", err)
			return err
		}
		fmt.Fprintf(GinkgoWriter, "[SUCCESS] Service cluster-proxy-addon-user exists\n")

		if hostedMode {
			if err := deploymentAvailable(hostingKubeClient, managedClusterInstallNamespace, "cluster-proxy-proxy-agent"); err != nil {
				return err
			}
			if err := deploymentAvailable(hostingKubeClient, managedClusterInstallNamespace, "cluster-proxy-managed-kubeconfig-provisioner"); err != nil {
				return err
			}
			if err := deploymentAvailable(managedKubeClient, managedClusterInstallNamespace, "cluster-proxy-service-relay"); err != nil {
				return err
			}
		} else {
			// deployment on managedcluster is running
			fmt.Fprintf(GinkgoWriter, "[DEBUG] Checking deployment: cluster-proxy-proxy-agent in namespace: %s\n", managedClusterInstallNamespace)
			if err := deploymentAvailable(hubKubeClient, managedClusterInstallNamespace, "cluster-proxy-proxy-agent"); err != nil {
				return err
			}
			fmt.Fprintf(GinkgoWriter, "[SUCCESS] Deployment cluster-proxy-proxy-agent is ready\n")
		}

		fmt.Fprintf(GinkgoWriter, "[SUCCESS] All resources are running\n")
		return nil
	}, eventuallyTimeout, eventuallyInterval).ShouldNot(HaveOccurred())
}

func deploymentAvailable(kubeClient kubernetes.Interface, namespace, name string) error {
	fmt.Fprintf(GinkgoWriter, "[DEBUG] Checking deployment: %s in namespace: %s\n", name, namespace)
	deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "[ERROR] Failed to get deployment %s: %v\n", name, err)
		return err
	}
	fmt.Fprintf(GinkgoWriter, "[DEBUG] Deployment %s status - Replicas: %d, Available: %d, Ready: %d, Updated: %d\n",
		name, deploy.Status.Replicas, deploy.Status.AvailableReplicas, deploy.Status.ReadyReplicas, deploy.Status.UpdatedReplicas)
	if deploy.Status.AvailableReplicas < 1 {
		return fmt.Errorf("available replicas for %s should >= 1, but get %d", name, deploy.Status.AvailableReplicas)
	}
	return nil
}

func prepareTestServiceAccount() {
	By("Create a hub serviceaccount for cluster-proxy requests")
	_, err := hubKubeClient.CoreV1().ServiceAccounts(hubInstallNamespace).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: hubInstallNamespace,
		},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}

	By("Create a role")
	_, err = hubKubeClient.RbacV1().Roles(hubInstallNamespace).Create(context.Background(), &v1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "podrole",
			Namespace: hubInstallNamespace,
		},
		Rules: []v1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/log"},
				Verbs:     []string{"get", "list"},
			}, {
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/portforward"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"watch"},
			},
		},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}

	By("Create a rolebinding")
	_, err = hubKubeClient.RbacV1().RoleBindings(hubInstallNamespace).Create(context.Background(), &v1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "podrolebinding",
			Namespace: hubInstallNamespace,
		},
		RoleRef: v1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "podrole",
		},
		Subjects: []v1.Subject{
			{
				Kind: v1.ServiceAccountKind,
				Name: serviceAccountName,
			},
		},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}

	if hostedMode {
		prepareHostedTargetRBAC()
	}
}

func prepareHostedTargetRBAC() {
	hubUser := fmt.Sprintf("cluster:hub:system:serviceaccount:%s:%s", hubInstallNamespace, serviceAccountName)
	createTargetRoleBinding("cluster-proxy-hub-user", v1.Subject{
		Kind: v1.UserKind,
		Name: hubUser,
	})

	By("Create a managed serviceaccount for managed-token authentication")
	_, err := managedKubeClient.CoreV1().ServiceAccounts(targetNamespace).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedServiceAccountName,
			Namespace: targetNamespace,
		},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}

	createTargetRoleBinding("cluster-proxy-managed-user", v1.Subject{
		Kind:      v1.ServiceAccountKind,
		Name:      managedServiceAccountName,
		Namespace: targetNamespace,
	})
}

func createTargetRoleBinding(name string, subject v1.Subject) {
	By("Create target role for cluster-proxy access")
	_, err := targetKubeClient.RbacV1().Roles(targetNamespace).Create(context.Background(), &v1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: targetNamespace,
		},
		Rules: []v1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "pods/log"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/exec", "pods/portforward"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"watch"},
			},
		},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}

	By("Create target rolebinding for cluster-proxy access")
	_, err = targetKubeClient.RbacV1().RoleBindings(targetNamespace).Create(context.Background(), &v1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: targetNamespace,
		},
		RoleRef: v1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []v1.Subject{subject},
	}, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).To(BeNil())
	}
}

func preparePodFortest() {
	if hostedMode {
		By("Use the hosted hello-world pod for kube-apiserver proxy tests")
		Eventually(func() error {
			pod, err := managedKubeClient.CoreV1().Pods(targetNamespace).Get(context.Background(), podName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if pod.Status.Phase != corev1.PodRunning {
				return fmt.Errorf("pod %s/%s is not running: %s", targetNamespace, podName, pod.Status.Phase)
			}
			return nil
		}, eventuallyTimeout, eventuallyInterval).ShouldNot(HaveOccurred())
		return
	}

	pods, err := hubKubeClient.CoreV1().Pods(hubInstallNamespace).List(context.Background(), metav1.ListOptions{})
	Expect(err).To(BeNil())
	for _, pod := range pods.Items {
		if !strings.Contains(pod.Name, "cluster-proxy-addon-manager") {
			continue
		}
		podName = pod.Name
	}
}

var (
	userServerServiceAddress string
)

func prepareClusterProxyClient() {
	var err error
	kubeconfig, err := configFromEnvOrInCluster("E2E_HUB_KUBECONFIG")
	if err != nil {
		Expect(err).To(BeNil())
	}
	userServerServiceAddress = os.Getenv("CLUSTER_PROXY_USER_SERVER_ADDRESS")
	if userServerServiceAddress == "" {
		userServerServiceAddress = "cluster-proxy-addon-user." + hubInstallNamespace + ".svc:9092"
	}

	By("Get RootCA of the cluster-proxy")
	// Get the CA certificate from the proxy-server-ca secret that is used to sign all certificates
	// including the user-server certificate (when userServer.enabled=true)
	caSecret, err := hubKubeClient.CoreV1().Secrets(hubInstallNamespace).Get(context.Background(), "proxy-server-ca", metav1.GetOptions{})
	Expect(err).To(BeNil())
	rootCA := string(caSecret.Data["ca.crt"])

	By("Create token for serviceAccount using TokenRequest API")
	tokenRequest, err := hubKubeClient.CoreV1().ServiceAccounts(hubInstallNamespace).CreateToken(
		context.Background(),
		serviceAccountName,
		&authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{
				ExpirationSeconds: func(i int64) *int64 { return &i }(3600), // 1 hour
			},
		},
		metav1.CreateOptions{},
	)
	Expect(err).To(BeNil())
	serviceAccountToken = tokenRequest.Status.Token

	By("Create kubeclient using cluster-proxy kubeconfig and http client to access specified services")
	err = func() error {
		var err error
		// create good client
		clusterProxyCfg = rest.CopyConfig(kubeconfig)

		// Add rootCA to the clusterProxyCfg
		clusterProxyCfg.TLSClientConfig.CAData = []byte(rootCA)
		clusterProxyCfg.TLSClientConfig.CertData = nil
		clusterProxyCfg.TLSClientConfig.KeyData = nil
		clusterProxyCfg.TLSClientConfig.CertFile = ""
		clusterProxyCfg.TLSClientConfig.KeyFile = ""
		clusterProxyCfg.BearerToken = serviceAccountToken
		clusterProxyCfg.BearerTokenFile = "" // Clear the default token file path from InClusterConfig

		clusterProxyCfg.Host = fmt.Sprintf("https://%s/%s", userServerServiceAddress, managedClusterName)
		fmt.Println("host:", clusterProxyCfg.Host)

		clusterProxyKubeClient, err = kubernetes.NewForConfig(clusterProxyCfg)
		if err != nil {
			return err
		}

		// change Host to the wrong host
		clusterWrongProxyCfg := rest.CopyConfig(clusterProxyCfg)
		clusterWrongProxyCfg.Host = fmt.Sprintf("https://%s/%s:", userServerServiceAddress, "wrongcluster")
		clusterWrongProxyCfg.TLSClientConfig.CAData = []byte(rootCA)
		clusterWrongProxyCfg.TLSClientConfig.CertData = nil
		clusterWrongProxyCfg.TLSClientConfig.KeyData = nil
		clusterWrongProxyCfg.TLSClientConfig.CertFile = ""
		clusterWrongProxyCfg.TLSClientConfig.KeyFile = ""
		clusterWrongProxyCfg.BearerToken = serviceAccountToken
		clusterWrongProxyCfg.BearerTokenFile = "" // Clear the default token file path from InClusterConfig

		clusterProxyWrongClient, err = kubernetes.NewForConfig(clusterWrongProxyCfg)
		if err != nil {
			return err
		}

		// create unauth proxy client
		clusterUnAuthProxyCfg := rest.CopyConfig(clusterProxyCfg)

		clusterUnAuthProxyCfg.Host = fmt.Sprintf("https://%s/%s", userServerServiceAddress, managedClusterName)
		clusterUnAuthProxyCfg.TLSClientConfig.CAData = []byte(rootCA)
		clusterUnAuthProxyCfg.TLSClientConfig.CertData = nil
		clusterUnAuthProxyCfg.TLSClientConfig.KeyData = nil
		clusterUnAuthProxyCfg.TLSClientConfig.CertFile = ""
		clusterUnAuthProxyCfg.TLSClientConfig.KeyFile = ""
		clusterUnAuthProxyCfg.BearerToken = serviceAccountToken + "wrong token"
		clusterUnAuthProxyCfg.BearerTokenFile = "" // Clear the default token file path from InClusterConfig

		clusterProxyUnAuthClient, err = kubernetes.NewForConfig(clusterUnAuthProxyCfg)
		if err != nil {
			return err
		}

		if hostedMode {
			By("Create managed serviceAccount token using TokenRequest API")
			tokenRequest, err := managedKubeClient.CoreV1().ServiceAccounts(targetNamespace).CreateToken(
				context.Background(),
				managedServiceAccountName,
				&authenticationv1.TokenRequest{
					Spec: authenticationv1.TokenRequestSpec{
						ExpirationSeconds: func(i int64) *int64 { return &i }(3600),
					},
				},
				metav1.CreateOptions{},
			)
			if err != nil {
				return err
			}
			managedServiceAccountToken = tokenRequest.Status.Token

			managedTokenProxyCfg := rest.CopyConfig(clusterProxyCfg)
			managedTokenProxyCfg.TLSClientConfig.CAData = []byte(rootCA)
			managedTokenProxyCfg.TLSClientConfig.CertData = nil
			managedTokenProxyCfg.TLSClientConfig.KeyData = nil
			managedTokenProxyCfg.TLSClientConfig.CertFile = ""
			managedTokenProxyCfg.TLSClientConfig.KeyFile = ""
			managedTokenProxyCfg.BearerToken = managedServiceAccountToken
			managedTokenProxyCfg.BearerTokenFile = ""
			clusterProxyManagedClient, err = kubernetes.NewForConfig(managedTokenProxyCfg)
			if err != nil {
				return err
			}
		}

		// clusterProxyHttpClient
		rootCAPool := x509.NewCertPool()
		rootCAPool.AppendCertsFromPEM([]byte(rootCA))
		clusterProxyHttpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs: rootCAPool,
				},
			},
		}

		return nil
	}()
	Expect(err).To(BeNil())
}
