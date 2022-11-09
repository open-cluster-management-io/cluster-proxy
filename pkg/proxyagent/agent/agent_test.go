package agent

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	mathrand "math/rand"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/cert"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	fakeaddon "open-cluster-management.io/api/client/addon/clientset/versioned/fake"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeruntime "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	testscheme   = scheme.Scheme
	nodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	tolerations  = []corev1.Toleration{{Key: "foo", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
)

func init() {
	testscheme.AddKnownTypes(proxyv1alpha1.SchemeGroupVersion, &proxyv1alpha1.ManagedProxyConfiguration{})
	testscheme.AddKnownTypes(clusterv1beta1.SchemeGroupVersion, &clusterv1beta1.ManagedClusterSetList{})
	testscheme.AddKnownTypes(proxyv1alpha1.SchemeGroupVersion, &proxyv1alpha1.ManagedProxyServiceResolverList{})
}

func TestFilterMPSR(t *testing.T) {
	testcases := []struct {
		name      string
		resolvers []proxyv1alpha1.ManagedProxyServiceResolver
		mcsMap    map[string]clusterv1beta1.ManagedClusterSet
		expected  []serviceToExpose
	}{
		{
			name: "filter out the resolver with deletion timestamp",
			resolvers: []proxyv1alpha1.ManagedProxyServiceResolver{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "resolver-1",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-1",
								Namespace: "ns-1",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-2", // this one expected to exist
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-2",
								Namespace: "ns-2",
							},
						},
					},
				},
			},
			mcsMap: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
			expected: []serviceToExpose{
				{
					Host:         util.GenerateServiceURL("cluster1", "ns-2", "service-2"),
					ExternalName: "service-2.ns-2",
				},
			},
		},
		{
			name: "filter out the resolver match other managed cluster set",
			resolvers: []proxyv1alpha1.ManagedProxyServiceResolver{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-1",
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-1",
								Namespace: "ns-1",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-2",
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-2",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-2",
								Namespace: "ns-2",
							},
						},
					},
				},
			},
			mcsMap: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
			expected: []serviceToExpose{
				{
					Host:         util.GenerateServiceURL("cluster1", "ns-1", "service-1"),
					ExternalName: "service-1.ns-1",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual := managedProxyServiceResolverToFilterServiceToExpose(testcase.resolvers, testcase.mcsMap, "cluster1")
			if len(actual) != len(testcase.expected) {
				t.Errorf("%s, expected %d resolvers, but got %d", testcase.name, len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("%s, expected %v, but got %v", testcase.name, testcase.expected, actual)
			}
		})
	}
}

func TestFilterMCS(t *testing.T) {
	testcases := []struct {
		name          string
		clusterlabels map[string]string
		clusters      []clusterv1beta1.ManagedClusterSet
		expected      map[string]clusterv1beta1.ManagedClusterSet
	}{
		{
			name: "filter out the cluster with deletion timestamp",
			clusterlabels: map[string]string{
				clusterv1beta1.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta1.ManagedClusterSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "set-1",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
				},
			},
			expected: map[string]clusterv1beta1.ManagedClusterSet{},
		},
		{
			name: "filter out the cluster without the current cluster label",
			clusterlabels: map[string]string{
				clusterv1beta1.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta1.ManagedClusterSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-2",
					},
				},
			},
			expected: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual, err := managedClusterSetsToFilteredMap(testcase.clusters, testcase.clusterlabels)
			if err != nil {
				t.Errorf("expected no error, but got %v", err)
			}
			if len(actual) != len(testcase.expected) {
				t.Errorf("expected %d clusters, but got %d", len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("expected %v, but got %v", testcase.expected, actual)
			}
		})
	}
}

func TestRemoveDupAndSortservicesToExpose(t *testing.T) {
	testcases := []struct {
		name     string
		services []serviceToExpose
		expected []serviceToExpose
	}{
		{
			name: "remove duplicate and sort other services",
			services: []serviceToExpose{
				{
					Host: "service-3",
				},
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-1",
				},
			},
			expected: []serviceToExpose{
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-3",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual := removeDupAndSortServices(testcase.services)
			if len(actual) != len(testcase.expected) {
				t.Errorf("expected %d services, but got %d", len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("expected %v, but got %v", testcase.expected, actual)
			}
		})
	}
}

func TestAgentAddonRegistrationOption(t *testing.T) {
	cases := []struct {
		name                     string
		signerName               string
		v1CSRSupported           bool
		agentInstallAll          bool
		cluster                  *clusterv1.ManagedCluster
		addon                    *addonv1alpha1.ManagedClusterAddOn
		expextedCSRConfigs       int
		expectedCSRApprove       bool
		expectedSignedCSR        bool
		expectedInstallNamespace string
	}{
		{
			name:                     "install all",
			agentInstallAll:          true,
			cluster:                  newCluster("cluster", false),
			addon:                    newAddOn("addon", "cluster"),
			expextedCSRConfigs:       1,
			expectedInstallNamespace: config.AddonInstallNamespace,
		},
		{
			name:               "csr v1 supported",
			v1CSRSupported:     true,
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
		},
		{
			name:               "sing csr",
			signerName:         ProxyAgentSignerName,
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 1,
			expectedSignedCSR:  true,
		},
		{
			name:               "approve csr",
			cluster:            newCluster("cluster", true),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 1,
			expectedCSRApprove: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"",
				c.v1CSRSupported,
				nil,
				fakeKubeClient,
				c.agentInstallAll,
				nil,
			)
			assert.NoError(t, err)

			options := agentAddOn.GetAgentAddonOptions()

			csrConfigs := options.Registration.CSRConfigurations(c.cluster)
			assert.Len(t, csrConfigs, c.expextedCSRConfigs)

			csrApprove := options.Registration.CSRApproveCheck(c.cluster, nil, nil)
			assert.Equal(t, c.expectedCSRApprove, csrApprove)
			if csrApprove != c.expectedCSRApprove {
				t.Errorf("expect csr approve is %v, but %v", c.expectedCSRApprove, csrApprove)
			}

			err = options.Registration.PermissionConfig(c.cluster, c.addon)
			assert.NoError(t, err)
			actions := fakeKubeClient.Actions()
			assert.Len(t, actions, 4)
			role := actions[1].(clienttesting.CreateAction).GetObject().(*rbacv1.Role)
			assert.Equal(t, "cluster-proxy-addon-agent", role.Name)
			rolebinding := actions[3].(clienttesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent", rolebinding.Name)

			cert := options.Registration.CSRSign(newCSR(c.signerName))
			assert.Equal(t, c.expectedSignedCSR, (len(cert) != 0))

			if c.expectedInstallNamespace != "" {
				assert.Equal(t, c.expectedInstallNamespace, options.InstallStrategy.InstallNamespace)
			}
		})
	}
}

func TestNewAgentAddon(t *testing.T) {
	addOnName := "addon"
	clusterName := "cluster"

	managedProxyConfigName := "cluster-proxy"
	addOndDeployConfigName := "deploy-config"

	expectedManifestNames := []string{
		"cluster-proxy-proxy-agent", // deployment
		"cluster-proxy-addon-agent", // role
		"cluster-proxy-addon-agent", // rolebinding
		"cluster-proxy-ca",          // ca
		clusterName,                 // cluster service
		addOnName,                   // namespace
		"cluster-proxy",             // service account
	}

	cases := []struct {
		name                    string
		cluster                 *clusterv1.ManagedCluster
		addon                   *addonv1alpha1.ManagedClusterAddOn
		managedProxyConfigs     []runtimeclient.Object
		addOndDeploymentConfigs []runtime.Object
		kubeObjs                []runtime.Object
		v1CSRSupported          bool
		expectedErrorMsg        string
		verifyManifests         func(t *testing.T, manifests []runtime.Object)
	}{
		{
			name:                    "without default config",
			addon:                   newAddOn(addOnName, clusterName),
			managedProxyConfigs:     []runtimeclient.Object{},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			expectedErrorMsg:        "unexpected managed proxy configurations: []",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no managed proxy configuration",
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference("none")}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			expectedErrorMsg:        "managedproxyconfigurations.proxy.open-cluster-management.io \"none\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no load balancer service",
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			expectedErrorMsg:        "services \"lbsvc\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "balancer service not ready",
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("")},
			expectedErrorMsg:        "the load-balancer service for proxy-server ingress is not yet provisioned",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name:    "balancer service proxy server",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4")},
			v1CSRSupported:          true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "1.2.3.4")
			},
		},
		{
			name:    "crs is not v1",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4"), newAgentClientSecret()},
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				crsExpectedManifestNames := append(expectedManifestNames, "cluster-proxy-open-cluster-management.io-proxy-agent-signer-client-cert")
				assert.Len(t, manifests, len(crsExpectedManifestNames))
				assert.ElementsMatch(t, crsExpectedManifestNames, manifestNames(manifests))
			},
		},
		{
			name:    "hostname proxy server ",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			v1CSRSupported:          true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "hostname")
			},
		},
		{
			name:    "port forward proxy server",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward)},
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			v1CSRSupported:          true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "127.0.0.1")
			},
		},
		{
			name:    "with addon deployment config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfigs:     []runtimeclient.Object{newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward)},
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)},
			v1CSRSupported:          true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, nodeSelector, agentDeploy.Spec.Template.Spec.NodeSelector)
				assert.Equal(t, tolerations, agentDeploy.Spec.Template.Spec.Tolerations)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset(c.kubeObjs...)
			fakeRuntimeClient := fakeruntime.NewClientBuilder().WithObjects(c.managedProxyConfigs...).Build()
			fakeAddonClient := fakeaddon.NewSimpleClientset(c.addOndDeploymentConfigs...)

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"test",
				c.v1CSRSupported,
				fakeRuntimeClient,
				fakeKubeClient,
				false,
				fakeAddonClient,
			)
			assert.NoError(t, err)

			manifests, err := agentAddOn.Manifests(c.cluster, c.addon)
			if c.expectedErrorMsg != "" {
				assert.ErrorContains(t, err, c.expectedErrorMsg)
				return
			}
			assert.NoError(t, err)

			c.verifyManifests(t, manifests)
		})
	}
}

type fakeSelfSigner struct {
	t *testing.T
}

func (fs *fakeSelfSigner) Sign(cfg cert.Config, expiry time.Duration) (selfsigned.CertPair, error) {
	return selfsigned.CertPair{}, nil
}

func (fs *fakeSelfSigner) CAData() []byte {
	return nil
}

func (fs *fakeSelfSigner) GetSigner() crypto.Signer {
	return nil
}

func (fs *fakeSelfSigner) CA() *openshiftcrypto.CA {
	_, key, err := newRSAKeyPair()
	if err != nil {
		fs.t.Fatal(err)
	}
	caCert, err := cert.NewSelfSignedCACert(cert.Config{CommonName: "open-cluster-management.io"}, key)
	if err != nil {
		fs.t.Fatal(err)
	}

	return &openshiftcrypto.CA{
		Config: &openshiftcrypto.TLSCertificateConfig{
			Certs: []*x509.Certificate{caCert},
			Key:   key,
		},
	}
}

func newRSAKeyPair() (*rsa.PublicKey, *rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	return &privateKey.PublicKey, privateKey, nil
}

func newCSR(signerName string) *csrv1.CertificateSigningRequest {
	insecureRand := mathrand.New(mathrand.NewSource(0))
	pk, err := ecdsa.GenerateKey(elliptic.P256(), insecureRand)
	if err != nil {
		panic(err)
	}
	csrb, err := x509.CreateCertificateRequest(insecureRand, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "cn",
			Organization: []string{"org"},
		},
		DNSNames:       []string{},
		EmailAddresses: []string{},
		IPAddresses:    []net.IP{},
	}, pk)
	if err != nil {
		panic(err)
	}
	return &csrv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:         "test",
			GenerateName: "csr-",
		},
		Spec: csrv1.CertificateSigningRequestSpec{
			Username:   "test",
			Usages:     []csrv1.KeyUsage{},
			SignerName: signerName,
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrb}),
		},
	}
}

func newCluster(name string, accepted bool) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: clusterv1.ManagedClusterSpec{
			HubAcceptsClient: accepted,
		},
	}
}

func newAddOn(name, namespace string) *addonv1alpha1.ManagedClusterAddOn {
	return &addonv1alpha1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1alpha1.ManagedClusterAddOnSpec{
			InstallNamespace: name,
		},
	}
}

func newManagedProxyConfigReference(name string) addonv1alpha1.ConfigReference {
	return addonv1alpha1.ConfigReference{
		ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
			Group:    "proxy.open-cluster-management.io",
			Resource: "managedproxyconfigurations",
		},
		ConfigReferent: addonv1alpha1.ConfigReferent{
			Name: name,
		},
	}
}

func newAddOndDeploymentConfigReference(name, namespace string) addonv1alpha1.ConfigReference {
	return addonv1alpha1.ConfigReference{
		ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
			Group:    "addon.open-cluster-management.io",
			Resource: "addondeploymentconfigs",
		},
		ConfigReferent: addonv1alpha1.ConfigReferent{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func newManagedProxyConfig(name string, entryPointType proxyv1alpha1.EntryPointType) *proxyv1alpha1.ManagedProxyConfiguration {
	return &proxyv1alpha1.ManagedProxyConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Entrypoint: &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
					Type: entryPointType,
					LoadBalancerService: &proxyv1alpha1.EntryPointLoadBalancerService{
						Name: "lbsvc",
					},
					Hostname: &proxyv1alpha1.EntryPointHostname{
						Value: "hostname",
					},
				},
				Namespace: "test",
			},
			ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
				Image: "quay.io/open-cluster-management.io/cluster-proxy-agent:test",
			},
		},
	}
}

func newAddOnDeploymentConfig(name, namespace string) *addonv1alpha1.AddOnDeploymentConfig {
	return &addonv1alpha1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1alpha1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
		},
	}
}

func newLoadBalancerService(ingress string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lbsvc",
			Namespace: "test",
		},
	}
	if len(ingress) != 0 {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ingress}}
	}
	return svc
}

func newAgentClientSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-client",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("testcrt"),
			"tls.key": []byte("testkey"),
		},
	}
}

func manifestNames(manifests []runtime.Object) []string {
	names := []string{}
	for _, manifest := range manifests {
		obj, ok := manifest.(metav1.ObjectMetaAccessor)
		if !ok {
			continue
		}
		names = append(names, obj.GetObjectMeta().GetName())
	}
	return names
}

func getAgentDeployment(manifests []runtime.Object) *appsv1.Deployment {
	for _, manifest := range manifests {
		switch obj := manifest.(type) {
		case *appsv1.Deployment:
			return obj
		}
	}

	return nil
}

func getProxyServerHost(deploy *appsv1.Deployment) string {
	args := deploy.Spec.Template.Spec.Containers[0].Args
	for _, arg := range args {
		if strings.HasPrefix(arg, "--proxy-server-host") {
			i := strings.Index(arg, "=") + 1
			return arg[i:]
		}
	}
	return ""
}
