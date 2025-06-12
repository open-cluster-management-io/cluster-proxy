package agent

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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
	"k8s.io/apimachinery/pkg/api/resource"
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
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
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
	testscheme.AddKnownTypes(clusterv1beta2.SchemeGroupVersion, &clusterv1beta2.ManagedClusterSetList{})
	testscheme.AddKnownTypes(proxyv1alpha1.SchemeGroupVersion, &proxyv1alpha1.ManagedProxyServiceResolverList{})
	testscheme.AddKnownTypes(addonv1alpha1.SchemeGroupVersion, &addonv1alpha1.AddOnDeploymentConfig{})
}

func TestFilterMPSR(t *testing.T) {
	testcases := []struct {
		name      string
		resolvers []proxyv1alpha1.ManagedProxyServiceResolver
		mcsMap    map[string]clusterv1beta2.ManagedClusterSet
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
			mcsMap: map[string]clusterv1beta2.ManagedClusterSet{
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
			mcsMap: map[string]clusterv1beta2.ManagedClusterSet{
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
							Type: "invalid",
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
			mcsMap: map[string]clusterv1beta2.ManagedClusterSet{
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
		clusters      []clusterv1beta2.ManagedClusterSet
		expected      map[string]clusterv1beta2.ManagedClusterSet
	}{
		{
			name: "filter out the cluster with deletion timestamp",
			clusterlabels: map[string]string{
				clusterv1beta2.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta2.ManagedClusterSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "set-1",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
				},
			},
			expected: map[string]clusterv1beta2.ManagedClusterSet{},
		},
		{
			name: "filter out the cluster without the current cluster label",
			clusterlabels: map[string]string{
				clusterv1beta2.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta2.ManagedClusterSet{
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
			expected: map[string]clusterv1beta2.ManagedClusterSet{
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
		name               string
		signerName         string
		v1CSRSupported     bool
		cluster            *clusterv1.ManagedCluster
		addon              *addonv1alpha1.ManagedClusterAddOn
		expextedCSRConfigs int
		expectedCSRApprove bool
		expectedSignedCSR  bool
	}{
		{
			name:               "install all",
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 1,
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
				true,
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
			assert.Len(t, actions, 8)
			role := actions[1].(clienttesting.CreateAction).GetObject().(*rbacv1.Role)
			assert.Equal(t, "cluster-proxy-addon-agent", role.Name)
			rolebinding := actions[3].(clienttesting.CreateAction).GetObject().(*rbacv1.RoleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent", rolebinding.Name)

			cert := options.Registration.CSRSign(newCSR(c.signerName))
			assert.Equal(t, c.expectedSignedCSR, (len(cert) != 0))
		})
	}
}

func TestNewAgentAddon(t *testing.T) {
	addOnName := "open-cluster-management-cluster-proxy"
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
		"cluster-proxy-service-proxy-server-certificates",
		"cluster-proxy-addon-agent-impersonator",                                       // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	expectedManifestNamesWithoutClusterService := []string{
		"cluster-proxy-proxy-agent", // deployment
		"cluster-proxy-addon-agent", // role
		"cluster-proxy-addon-agent", // rolebinding
		"cluster-proxy-ca",          // ca
		addOnName,                   // namespace
		"cluster-proxy",             // service account
		"cluster-proxy-service-proxy-server-certificates",
		"cluster-proxy-addon-agent-impersonator",                                       // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	cases := []struct {
		name                    string
		cluster                 *clusterv1.ManagedCluster
		addon                   *addonv1alpha1.ManagedClusterAddOn
		managedProxyConfig      runtimeclient.Object
		addOndDeploymentConfigs []runtime.Object
		kubeObjs                []runtime.Object
		v1CSRSupported          bool
		enableKubeApiProxy      bool
		expectedErrorMsg        string
		verifyManifests         func(t *testing.T, manifests []runtime.Object)
	}{
		{
			name:                    "without default config",
			addon:                   newAddOn(addOnName, clusterName),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "managedproxyconfigurations.proxy.open-cluster-management.io \"cluster-proxy\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no managed proxy configuration",
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference("none")}
				return addOn
			}(),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "managedproxyconfigurations.proxy.open-cluster-management.io \"cluster-proxy\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no load balancer service",
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("")},
			enableKubeApiProxy:      true,
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4")},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4"), newAgentClientSecret()},
			enableKubeApiProxy:      true,
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "hostname")
			},
		},
		{
			name:    "customized proxy-agent replicas",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      setProxyAgentReplicas(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname), 2),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, *agentDeploy.Spec.Replicas, int32(2))
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
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
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, nodeSelector, agentDeploy.Spec.Template.Spec.NodeSelector)
				assert.Equal(t, tolerations, agentDeploy.Spec.Template.Spec.Tolerations)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 1, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 1, count)

			},
		},
		{
			name:    "with addon deployment config using a customized serviceDomain",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				externalNameService := getKubeAPIServerExternalNameService(manifests, clusterName)
				assert.NotNil(t, externalNameService)
				assert.Equal(t, "kubernetes.default.svc.test.com", externalNameService.Spec.ExternalName)
			},
		},
		{
			name:    "enable-kube-api-proxy is false",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			v1CSRSupported:          true,
			enableKubeApiProxy:      false,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				// expect cluster service not created.
				assert.Len(t, manifests, len(expectedManifestNames)-1)
				assert.ElementsMatch(t, expectedManifestNamesWithoutClusterService, manifestNames(manifests))
			},
		},
		{
			name:    "with addon deployment config including https proxy config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpsProxy(addOndDeployConfigName, clusterName)},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 4, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 2, count)
			},
		},
		{
			name:    "with addon deployment config including http proxy config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpProxy(addOndDeployConfigName, clusterName)},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 4, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 1, count)
			},
		},
		{
			name:    "with addon deployment config including install namespace",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				func() *addonv1alpha1.AddOnDeploymentConfig {
					config := newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)
					config.Spec.AgentInstallNamespace = "addon-test"
					return config
				}()},
			v1CSRSupported:     true,
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				newexpectedManifestNames := []string{}
				newexpectedManifestNames = append(newexpectedManifestNames, expectedManifestNames...)
				newexpectedManifestNames[5] = "addon-test"
				newexpectedManifestNames[9] = "cluster-proxy-addon-agent-impersonator:addon-test" // clusterrolebinding
				assert.ElementsMatch(t, newexpectedManifestNames, manifestNames(manifests))
			},
		},
		{
			name:    "with addon deployment config using customized variables",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				func() *addonv1alpha1.AddOnDeploymentConfig {
					config := newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)
					config.Spec.CustomizedVariables = []addonv1alpha1.CustomizedVariable{
						{
							Name:  "replicas",
							Value: "10",
						},
					}
					return config
				}(),
			},
			v1CSRSupported:     true,
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, int32(10), *agentDeploy.Spec.Replicas)
			},
		},
		{
			name:    "with addon deployment config using a customized serviceDomain",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			v1CSRSupported:          true,
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				externalNameService := getKubeAPIServerExternalNameService(manifests, clusterName)
				assert.NotNil(t, externalNameService)
				assert.Equal(t, "kubernetes.default.svc.test.com", externalNameService.Spec.ExternalName)
			},
		},
		{
			name:    "with addon deployment config using resources requirement",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1alpha1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1alpha1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				newAddOnDeploymentConfigWithResourcesRequirement(
					addOndDeployConfigName,
					clusterName,
					"deployments:cluster-proxy-proxy-agent:proxy-agent",
					corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("250Mi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("250Mi"),
						},
					},
				),
			},
			v1CSRSupported:     true,
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))

				// Get the agent deployment and verify resource requirements
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)

				// Check if the container has the expected resource requirements
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						assert.Equal(t, resource.MustParse("150m"), container.Resources.Limits[corev1.ResourceCPU])
						assert.Equal(t, resource.MustParse("250Mi"), container.Resources.Limits[corev1.ResourceMemory])
						assert.Equal(t, resource.MustParse("150m"), container.Resources.Requests[corev1.ResourceCPU])
						assert.Equal(t, resource.MustParse("250Mi"), container.Resources.Requests[corev1.ResourceMemory])
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// add service-proxy secret into kubeObjects
			c.kubeObjs = append(c.kubeObjs, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-proxy-service-proxy-server-cert",
					Namespace: "test",
				},
				Data: map[string][]byte{
					"tls.crt": []byte("testcrt"),
					"tls.key": []byte("testkey"),
				},
			})

			fakeKubeClient := fakekube.NewSimpleClientset(c.kubeObjs...)
			var fakeRuntimeClient runtimeclient.Client
			if c.managedProxyConfig == nil {
				fakeRuntimeClient = fakeruntime.NewClientBuilder().Build()
			} else {
				fakeRuntimeClient = fakeruntime.NewClientBuilder().WithObjects(c.managedProxyConfig).Build()
			}
			fakeAddonClient := fakeaddon.NewSimpleClientset(c.addOndDeploymentConfigs...)

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"test",
				c.v1CSRSupported,
				fakeRuntimeClient,
				fakeKubeClient,
				c.enableKubeApiProxy,
				fakeAddonClient,
			)
			assert.NoError(t, err)

			manifests, err := agentAddOn.Manifests(c.cluster, c.addon.DeepCopy())
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
		DesiredConfig: &addonv1alpha1.ConfigSpecHash{
			ConfigReferent: addonv1alpha1.ConfigReferent{
				Name: name,
			},
			SpecHash: "dummy",
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
		DesiredConfig: &addonv1alpha1.ConfigSpecHash{
			ConfigReferent: addonv1alpha1.ConfigReferent{
				Name:      name,
				Namespace: namespace,
			},
			SpecHash: "dummy",
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

func setProxyAgentReplicas(mpc *proxyv1alpha1.ManagedProxyConfiguration, replicas int32) *proxyv1alpha1.ManagedProxyConfiguration {
	mpc.Spec.ProxyAgent.Replicas = replicas
	return mpc
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

func newAddOnDeploymentConfigWithCustomizedServiceDomain(name, namespace, serviceDomain string) *addonv1alpha1.AddOnDeploymentConfig {
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
			CustomizedVariables: []addonv1alpha1.CustomizedVariable{
				{
					Name:  "serviceDomain",
					Value: serviceDomain,
				},
			},
		},
	}
}

var fakeCA = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUM2VENDQWRFQ0ZHSG5lTUpBQ1NjR2lRSnA2K1RYa0NKRVBTVitNQTBHQ1NxR1NJYjNEUUVCQ3dVQU1ERXgKRmpBVUJnTlZCQW9NRFU5d1pXNVRhR2xtZENCQlEwMHhGekFWQmdOVkJBTU1EbmQzZHk1eVpXUm9ZWFF1WTI5dApNQjRYRFRJek1URXhNakV5TURZME4xb1hEVEkwTVRFeE1URXlNRFkwTjFvd01URVdNQlFHQTFVRUNnd05UM0JsCmJsTm9hV1owSUVGRFRURVhNQlVHQTFVRUF3d09kM2QzTG5KbFpHaGhkQzVqYjIwd2dnRWlNQTBHQ1NxR1NJYjMKRFFFQkFRVUFBNElCRHdBd2dnRUtBb0lCQVFEUXZMbHFjYXpYZmxXNXgzcVFDSE52ZjNqTFNCY0QrY3pCczFoMApUV0p2TWEvWVd2T2MrK3VNWXg2OW1RaXRCWEFaMEsyUVpQa1BYK2lEc244Mk9mNklYTUpUSVpmZk1Wb3g4UmtqCkNlQ00vdlNaMzExVGlwa0NkaGVTbnp0WElhek1hN0ZZS3BVT2htYTF3L2RReFcvcnIwandwRG9TMFUvN0xhWGwKNHF2bUF4Wk1iSHVWaFk2S0RZSGJ2MEdKYWdqekJtVkpieTZlMFg3MkozL05ZME1KT2plYklrOTEydjBXZ1pUKwo3UWU0a29scVY1MkQvaUhYV0xFUzhXMWQrMFZUbnlRaFAzY3RvNWp3TFZyWnQ2NDFZL0lRc2ZNQ0w1bGdhVTF0Cm9UMlcvQ3F1amw5aCt0UCt2SG1rNk5JZXk2RUNIdm1MV0xLbU5nblp2M0d0bVdnZEFnTUJBQUV3RFFZSktvWkkKaHZjTkFRRUxCUUFEZ2dFQkFKSjBnd0UxSUR4SlNzaUd1TGxDMlVGV2J3U0RHMUVEK3VlQWYvRDRlV0VSWFZDUAo4aVdZZC9RckdsakYxNGxvZllHb280Vk5PL28xQWJQS2gveXB4UW16REdrVE1NaGg2WFg1bExob3RZWHZERlM2CmlkQXk5TFpiWDFUQnV5UEcwNmorbkI4eEtEY3F4aFNLYTlNb0trck9XcmtGbnFZS2syQzIyZGRvZVlZdlRjR2cKK2JmZ3RSWFJRUFdQRmt2NDR5MGlMZVh0S0VMbHBQMkMyQW5JQkU4b2hzY0JiYnloVmptem5YS1dFSTg3T0xmUgoxNDJBOWoydlVVQW80T0o5d1JCei8raDFXUXkyL3prclVUMW90MFdienY1cy91YmlUQkRpSjlQQ0k4YkZmZXplCnpDbCthbEE5aUFJdGt4OVdZS2pzaDFuVHEzTnJwVWM0MXBJWlFBQT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="

func newAddOnDeploymentConfigWithHttpsProxy(name, namespace string) *addonv1alpha1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
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
			ProxyConfig: addonv1alpha1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "https://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}
func newAddOnDeploymentConfigWithHttpProxy(name, namespace string) *addonv1alpha1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
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
			ProxyConfig: addonv1alpha1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "http://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}

func newAddOnDeploymentConfigWithResourcesRequirement(name, namespace, containerID string,
	resources corev1.ResourceRequirements) *addonv1alpha1.AddOnDeploymentConfig {

	return &addonv1alpha1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
			ResourceRequirements: []addonv1alpha1.ContainerResourceRequirements{
				{
					ContainerID: containerID,
					Resources:   resources,
				},
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

func getKubeAPIServerExternalNameService(manifests []runtime.Object, clusterName string) *corev1.Service {
	for _, manifest := range manifests {
		switch obj := manifest.(type) {
		case *corev1.Service:
			// As the cluster-service.yaml shows, the service name is cluster name.
			if obj.Name == clusterName {
				return obj
			}
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

func getCASecret(manifests []runtime.Object) *corev1.Secret {
	for _, manifest := range manifests {
		switch obj := manifest.(type) {
		case *corev1.Secret:
			if obj.Name == "cluster-proxy-ca" {
				return obj
			}
		}
	}

	return nil
}
