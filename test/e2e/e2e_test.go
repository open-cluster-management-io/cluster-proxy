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
	managedClusterName       string
	hubRESTConfig            *rest.Config
	hubKubeClient            kubernetes.Interface
	hubRuntimeClient         client.Client
	clusterProxyKubeClient   kubernetes.Interface
	clusterProxyWrongClient  kubernetes.Interface
	clusterProxyUnAuthClient kubernetes.Interface
	clusterProxyHttpClient   *http.Client
	hubAddOnClient           addonclient.Interface
	hubClusterClient         clusterclient.Interface
	clusterProxyCfg          *rest.Config
	serviceAccountToken      string
	podName                  string
)

const (
	eventuallyTimeout              = 600 // seconds
	eventuallyInterval             = 30  // seconds
	hubInstallNamespace            = "open-cluster-management-addon"
	managedClusterInstallNamespace = "open-cluster-management-cluster-proxy"
	serviceAccountName             = "cluster-proxy-test"
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
		hubRESTConfig, err = rest.InClusterConfig()
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

		return err
	}()
	Expect(err).To(BeNil())

	checkAddonStatus()

	prepareTestServiceAccount()

	preparePodFortest()

	prepareClusterProxyClient()
})

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

		// deployment on managedcluster is running
		fmt.Fprintf(GinkgoWriter, "[DEBUG] Checking deployment: cluster-proxy-proxy-agent in namespace: %s\n", managedClusterInstallNamespace)
		anpAgent, err := hubKubeClient.AppsV1().Deployments(managedClusterInstallNamespace).Get(context.Background(), "cluster-proxy-proxy-agent", metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "[ERROR] Failed to get deployment cluster-proxy-proxy-agent: %v\n", err)
			return err
		}
		fmt.Fprintf(GinkgoWriter, "[DEBUG] Deployment cluster-proxy-proxy-agent status - Replicas: %d, Available: %d, Ready: %d, Updated: %d\n",
			anpAgent.Status.Replicas, anpAgent.Status.AvailableReplicas, anpAgent.Status.ReadyReplicas, anpAgent.Status.UpdatedReplicas)
		if anpAgent.Status.AvailableReplicas < 1 {
			errMsg := fmt.Errorf("available replicas for %s should be more than 1, but get %d", "anp-agent", anpAgent.Status.AvailableReplicas)
			fmt.Fprintf(GinkgoWriter, "[ERROR] %v\n", errMsg)
			return errMsg
		}
		fmt.Fprintf(GinkgoWriter, "[SUCCESS] Deployment cluster-proxy-proxy-agent is ready\n")

		fmt.Fprintf(GinkgoWriter, "[SUCCESS] All resources are running\n")
		return nil
	}, eventuallyTimeout, eventuallyInterval).ShouldNot(HaveOccurred())
}

func prepareTestServiceAccount() {
	By("Create a serviceaccount on managedcluster")
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
}

func preparePodFortest() {
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
	kubeconfig, err := rest.InClusterConfig()
	if err != nil {
		Expect(err).To(BeNil())
	}
	userServerServiceAddress = "cluster-proxy-addon-user." + hubInstallNamespace + ".svc:9092"

	By("Get RootCA of the cluster-proxy")
	// Get the CA certificate from the cluster-proxy-ca-secret that was used to sign the user-server certificate
	caSecret, err := hubKubeClient.CoreV1().Secrets(hubInstallNamespace).Get(context.Background(), "cluster-proxy-ca-secret", metav1.GetOptions{})
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
		clusterUnAuthProxyCfg.BearerToken = serviceAccountToken + "wrong token"
		clusterUnAuthProxyCfg.BearerTokenFile = "" // Clear the default token file path from InClusterConfig

		clusterProxyUnAuthClient, err = kubernetes.NewForConfig(clusterUnAuthProxyCfg)
		if err != nil {
			return err
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
