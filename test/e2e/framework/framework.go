package framework

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// unique identifier of the e2e run
var RunID = rand.String(6)

type Framework interface {
	HubRESTConfig() *rest.Config
	TestClusterName() string

	HubNativeClient() kubernetes.Interface
	HubRuntimeClient() client.Client
}

var _ Framework = &framework{}

type framework struct {
	basename string
	ctx      *E2EContext
}

func NewE2EFramework(basename string) Framework {
	f := &framework{
		basename: basename,
		ctx:      e2eContext,
	}
	BeforeEach(f.BeforeEach)
	AfterEach(f.AfterEach)
	return f
}

func (f *framework) HubRESTConfig() *rest.Config {
	restConfig, err := clientcmd.BuildConfigFromFlags("", f.ctx.HubKubeConfig)
	Expect(err).NotTo(HaveOccurred())
	return restConfig
}

func (f *framework) HubNativeClient() kubernetes.Interface {
	cfg := f.HubRESTConfig()
	nativeClient, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	return nativeClient
}

func (f *framework) HubRuntimeClient() client.Client {
	cfg := f.HubRESTConfig()
	runtimeClient, err := client.New(cfg, client.Options{
		Scheme: scheme,
	})
	Expect(err).NotTo(HaveOccurred())
	return runtimeClient
}

func (f *framework) TestClusterName() string {
	return f.ctx.TestCluster
}

func (f *framework) BeforeEach() {
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
}

func (f *framework) AfterEach() {

}
