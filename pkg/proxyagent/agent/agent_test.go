package agent

import (
	"context"
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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/cert"
	"k8s.io/utils/ptr"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeruntime "sigs.k8s.io/controller-runtime/pkg/client/fake"

	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	fakeaddon "open-cluster-management.io/api/client/addon/clientset/versioned/fake"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
)

var (
	testscheme   = scheme.Scheme
	nodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	tolerations  = []corev1.Toleration{{Key: "foo", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
)

func init() {
	_ = proxyv1alpha1.AddToScheme(testscheme)
	_ = addonv1beta1.Install(testscheme)
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
		cluster            *clusterv1.ManagedCluster
		addon              *addonv1beta1.ManagedClusterAddOn
		expextedCSRConfigs int
		expectedCSRApprove bool
		expectedSignedCSR  bool
	}{
		{
			name:               "install all",
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
		},
		{
			name:               "sing csr",
			signerName:         ProxyAgentSignerName,
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
			expectedSignedCSR:  true,
		},
		{
			name:               "approve csr",
			cluster:            newCluster("cluster", true),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
			expectedCSRApprove: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"",
				nil,
				fakeKubeClient,
				true,
				false,
				false,
				nil,
			)
			assert.NoError(t, err)

			options := agentAddOn.GetAgentAddonOptions()

			csrConfigs, err := options.Registration.Configurations(context.TODO(), c.cluster, nil)
			assert.NoError(t, err)
			assert.Len(t, csrConfigs, c.expextedCSRConfigs)

			csrApprove := options.Registration.CSRApproveCheck(context.TODO(), c.cluster, nil, nil)
			assert.Equal(t, c.expectedCSRApprove, csrApprove)
			if csrApprove != c.expectedCSRApprove {
				t.Errorf("expect csr approve is %v, but %v", c.expectedCSRApprove, csrApprove)
			}

			err = options.Registration.PermissionConfig(context.TODO(), c.cluster, c.addon)
			assert.NoError(t, err)
			actions := fakeKubeClient.Actions()
			assert.Len(t, actions, 8)

			// Extract RBAC resources from actions
			var role *rbacv1.Role
			var roleBinding *rbacv1.RoleBinding
			var clusterRole *rbacv1.ClusterRole
			var clusterRoleBinding *rbacv1.ClusterRoleBinding

			for _, action := range actions {
				if action.GetVerb() == "create" {
					switch obj := action.(clienttesting.CreateAction).GetObject().(type) {
					case *rbacv1.Role:
						role = obj
					case *rbacv1.RoleBinding:
						roleBinding = obj
					case *rbacv1.ClusterRole:
						clusterRole = obj
					case *rbacv1.ClusterRoleBinding:
						clusterRoleBinding = obj
					}
				}
			}

			// Verify Role was created with correct name and permissions
			assert.NotNil(t, role)
			assert.Equal(t, "cluster-proxy-addon-agent", role.Name)
			assert.Equal(t, []rbacv1.PolicyRule{
				{
					APIGroups: []string{"coordination.k8s.io"},
					Verbs:     []string{"*"},
					Resources: []string{"leases"},
				},
			}, role.Rules)

			// Verify RoleBinding was created and references the correct subjects
			assert.NotNil(t, roleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent", roleBinding.Name)
			assert.Equal(t, rbacv1.RoleRef{
				Kind:     "Role",
				Name:     "cluster-proxy-addon-agent",
				APIGroup: rbacv1.GroupName,
			}, roleBinding.RoleRef)
			// For token-based registration, subjects come from addon.Status.Registrations
			assert.NotEmpty(t, roleBinding.Subjects)

			// Verify ClusterRole was created with correct permissions
			assert.NotNil(t, clusterRole)
			assert.Equal(t, "cluster-proxy-addon-agent-tokenreview", clusterRole.Name)
			assert.Equal(t, []rbacv1.PolicyRule{
				{
					APIGroups: []string{"authentication.k8s.io"},
					Verbs:     []string{"create"},
					Resources: []string{"tokenreviews"},
				},
			}, clusterRole.Rules)

			// Verify ClusterRoleBinding was created
			assert.NotNil(t, clusterRoleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent-tokenreview", clusterRoleBinding.Name)
			assert.Equal(t, rbacv1.RoleRef{
				Kind:     "ClusterRole",
				Name:     "cluster-proxy-addon-agent-tokenreview",
				APIGroup: rbacv1.GroupName,
			}, clusterRoleBinding.RoleRef)
			assert.NotEmpty(t, clusterRoleBinding.Subjects)

			cert, err := options.Registration.CSRSign(context.TODO(), nil, nil, newCSR(c.signerName))
			assert.NoError(t, err)
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
		"cluster-proxy-proxy-agent",              // deployment
		"cluster-proxy-addon-agent",              // role
		"cluster-proxy-addon-agent",              // rolebinding
		"cluster-proxy-ca",                       // ca
		clusterName,                              // cluster service
		addOnName,                                // namespace
		"cluster-proxy",                          // service account
		"cluster-proxy-addon-agent-impersonator", // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	expectedManifestNamesWithoutClusterService := []string{
		"cluster-proxy-proxy-agent",              // deployment
		"cluster-proxy-addon-agent",              // role
		"cluster-proxy-addon-agent",              // rolebinding
		"cluster-proxy-ca",                       // ca
		addOnName,                                // namespace
		"cluster-proxy",                          // service account
		"cluster-proxy-addon-agent-impersonator", // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	expectedManifestNamesWithServiceProxy := append([]string{}, expectedManifestNames...)
	expectedManifestNamesWithServiceProxy = append(expectedManifestNamesWithServiceProxy, "cluster-proxy-service-proxy-server-certificates")

	newAddonWithDeploymentConfig := func() *addonv1beta1.ManagedClusterAddOn {
		addOn := newAddOn(addOnName, clusterName)
		addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
			newManagedProxyConfigReference(managedProxyConfigName),
			newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
		}
		return addOn
	}

	cases := []struct {
		name                    string
		cluster                 *clusterv1.ManagedCluster
		addon                   *addonv1beta1.ManagedClusterAddOn
		managedProxyConfig      runtimeclient.Object
		addOndDeploymentConfigs []runtime.Object
		kubeObjs                []runtime.Object
		enableKubeApiProxy      bool
		enableServiceProxy      bool
		enableNetworkPolicies   bool
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference("none")}
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4")},
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
			name:    "hostname proxy server ",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      setProxyAgentReplicas(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname), 2),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
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
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
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
			name:    "port forward proxy server with service proxy",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			enableServiceProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNamesWithServiceProxy))
				assert.ElementsMatch(t, expectedManifestNamesWithServiceProxy, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				serviceProxy := getDeploymentContainer(agentDeploy, "service-proxy")
				if assert.NotNil(t, serviceProxy) {
					if assert.NotNil(t, serviceProxy.ReadinessProbe) &&
						assert.NotNil(t, serviceProxy.ReadinessProbe.TCPSocket) {
						assert.Equal(t, int32(constant.ServiceProxyPort), serviceProxy.ReadinessProbe.TCPSocket.Port.IntVal)
					}
					// oidc flags must not be rendered when the oidc variables are unset
					for _, arg := range serviceProxy.Args {
						assert.NotContains(t, arg, "--oidc-")
					}
				}
			},
		},
		{
			name:    "port forward proxy server with network policies",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			enableNetworkPolicies:   true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				expected := append([]string{}, expectedManifestNames...)
				expected = append(expected, "cluster-proxy-proxy-agent-network-policy")
				assert.Len(t, manifests, len(expected))
				assert.ElementsMatch(t, expected, manifestNames(manifests))
				np := getAgentNetworkPolicy(manifests)
				if assert.NotNil(t, np) {
					assert.False(t, networkPolicyHasAllowAllEgress(np),
						"without service-proxy, agent NP must not allow all egress")
				}
			},
		},
		{
			name:    "network policies with service proxy allows arbitrary backend egress",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			enableServiceProxy:      true,
			enableNetworkPolicies:   true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				expected := append([]string{}, expectedManifestNamesWithServiceProxy...)
				expected = append(expected, "cluster-proxy-proxy-agent-network-policy")
				assert.Len(t, manifests, len(expected))
				assert.ElementsMatch(t, expected, manifestNames(manifests))
				np := getAgentNetworkPolicy(manifests)
				if assert.NotNil(t, np) {
					// Fixed allowlist covers 443/6443/entrypoint but not e.g. :8080;
					// service-proxy backends use Cluster-Proxy-Port at request time.
					assert.False(t, networkPolicyAllowsEgressTCPPort(np, 8080),
						"fixed allowlist alone must not cover backend port 8080")
					assert.True(t, networkPolicyHasAllowAllEgress(np),
						"service-proxy requires allow-all egress exemption for arbitrary backends")
				}
			},
		},
		{
			name:                    "with addon deployment config",
			cluster:                 newCluster(clusterName, true),
			addon:                   newAddonWithDeploymentConfig(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)},
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
			name:               "with addon deployment config using a customized serviceDomain",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "serviceDomain", Value: "svc.test.com"})},
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				externalNameService := getKubeAPIServerExternalNameService(manifests, clusterName)
				assert.NotNil(t, externalNameService)
				assert.Equal(t, "kubernetes.default.svc.test.com", externalNameService.Spec.ExternalName)
			},
		},
		{
			name:               "enable-kube-api-proxy is false",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "serviceDomain", Value: "svc.test.com"})},
			enableKubeApiProxy: false,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				// expect cluster service not created.
				assert.Len(t, manifests, len(expectedManifestNames)-1)
				assert.ElementsMatch(t, expectedManifestNamesWithoutClusterService, manifestNames(manifests))
			},
		},
		{
			name:                    "with addon deployment config including https proxy config",
			cluster:                 newCluster(clusterName, true),
			addon:                   newAddonWithDeploymentConfig(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpsProxy(addOndDeployConfigName, clusterName)},
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
			name:                    "with addon deployment config including http proxy config",
			cluster:                 newCluster(clusterName, true),
			addon:                   newAddonWithDeploymentConfig(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpProxy(addOndDeployConfigName, clusterName)},
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
			name:               "with addon deployment config including install namespace",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				func() *addonv1beta1.AddOnDeploymentConfig {
					config := newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)
					config.Spec.AgentInstallNamespace = "addon-test"
					return config
				}()},
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				newexpectedManifestNames := []string{}
				newexpectedManifestNames = append(newexpectedManifestNames, expectedManifestNames...)
				newexpectedManifestNames[5] = "addon-test"
				newexpectedManifestNames[8] = "cluster-proxy-addon-agent-impersonator:addon-test" // clusterrolebinding
				assert.ElementsMatch(t, newexpectedManifestNames, manifestNames(manifests))
			},
		},
		{
			name:               "with addon deployment config using customized variables",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "replicas", Value: "10"})},
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
			name:               "with addon deployment config using customized oidc variables",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcUsernameClaim", Value: "email"},
				addonv1beta1.CustomizedVariable{Name: "oidcUsernamePrefix", Value: "oidc:"},
				addonv1beta1.CustomizedVariable{Name: "oidcGroupsClaim", Value: "groups"},
				addonv1beta1.CustomizedVariable{Name: "oidcGroupsPrefix", Value: "oidc:"},
				addonv1beta1.CustomizedVariable{Name: "oidcReservedNamePrefixes", Value: "system:,dev:"},
				addonv1beta1.CustomizedVariable{Name: "oidcCAConfigMap", Value: "dex-ca"},
				addonv1beta1.CustomizedVariable{Name: "oidcSigningAlgs", Value: "RS256,ES256"},
				addonv1beta1.CustomizedVariable{Name: "oidcRequiredClaimsJSON", Value: `{"hd":"example.com","tenant":"tenant-id"}`},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				serviceProxy := getDeploymentContainer(agentDeploy, "service-proxy")
				if assert.NotNil(t, serviceProxy) {
					assert.Subset(t, serviceProxy.Args, []string{
						"--oidc-issuer-url=https://dex.example.com/dex",
						"--oidc-client-id=cluster-proxy",
						"--oidc-username-claim=email",
						"--oidc-username-prefix=oidc:",
						"--oidc-groups-claim=groups",
						"--oidc-groups-prefix=oidc:",
						"--oidc-reserved-name-prefixes=system:,dev:",
						"--oidc-ca-file=/oidc-ca/ca.crt",
						"--oidc-signing-algs=RS256,ES256",
						"--oidc-required-claim=hd=example.com",
						"--oidc-required-claim=tenant=tenant-id",
					})

					assert.Contains(t, serviceProxy.VolumeMounts, corev1.VolumeMount{
						Name:      "oidc-ca",
						MountPath: "/oidc-ca",
						ReadOnly:  true,
					})
				}
				assert.Contains(t, agentDeploy.Spec.Template.Spec.Volumes, corev1.Volume{
					Name: "oidc-ca",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "dex-ca"},
							Optional:             ptr.To(true),
						},
					},
				})
			},
		},
		{
			name:               "with addon deployment config using default oidc reserved name prefixes",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				serviceProxy := getDeploymentContainer(agentDeploy, "service-proxy")
				if assert.NotNil(t, serviceProxy) {
					assert.Contains(t, serviceProxy.Args, "--oidc-reserved-name-prefixes=system:")
				}
			},
		},
		{
			// the flag is passed unconditionally, so an empty value disables the
			// check instead of falling back to the agent's own default
			name:               "with addon deployment config disabling oidc reserved name prefixes",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcReservedNamePrefixes", Value: ""},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				serviceProxy := getDeploymentContainer(agentDeploy, "service-proxy")
				if assert.NotNil(t, serviceProxy) {
					assert.Contains(t, serviceProxy.Args, "--oidc-reserved-name-prefixes=")
				}
			},
		},
		{
			name:               "rejects oidc issuer without client id",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcIssuerURL and oidcClientID must be specified together",
		},
		{
			name:               "rejects oidc client id without issuer",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcIssuerURL and oidcClientID must be specified together",
		},
		{
			name:               "rejects oidc when impersonation is disabled",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "enableImpersonation", Value: "false"},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcIssuerURL requires enableImpersonation=true",
		},
		{
			// customizedVariables are string-typed, so the chart's schema is what
			// rejects an empty prefix before it reaches the agent
			name:               "rejects an empty oidc reserved name prefix",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcReservedNamePrefixes", Value: "system:,,dev:"},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcReservedNamePrefixes",
		},
		{
			name:               "rejects malformed oidc required claims JSON",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcRequiredClaimsJSON", Value: `{"hd":`},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcRequiredClaimsJSON",
		},
		{
			name:               "rejects non-object oidc required claims JSON",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcRequiredClaimsJSON", Value: `[]`},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcRequiredClaimsJSON must be a JSON object",
		},
		{
			name:               "rejects non-string oidc required claim values",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithVariables(addOndDeployConfigName, clusterName,
				addonv1beta1.CustomizedVariable{Name: "oidcIssuerURL", Value: "https://dex.example.com/dex"},
				addonv1beta1.CustomizedVariable{Name: "oidcClientID", Value: "cluster-proxy"},
				addonv1beta1.CustomizedVariable{Name: "oidcRequiredClaimsJSON", Value: `{"tenant":42}`},
			)},
			enableKubeApiProxy: true,
			enableServiceProxy: true,
			expectedErrorMsg:   "oidcRequiredClaimsJSON values must be strings",
		},
		{
			name:               "with addon deployment config using resources requirement",
			cluster:            newCluster(clusterName, true),
			addon:              newAddonWithDeploymentConfig(),
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
				fakeRuntimeClient,
				fakeKubeClient,
				c.enableKubeApiProxy,
				c.enableServiceProxy,
				c.enableNetworkPolicies,
				fakeAddonClient,
			)
			assert.NoError(t, err)

			manifests, err := agentAddOn.Manifests(context.TODO(), c.cluster, c.addon.DeepCopy())
			if c.expectedErrorMsg != "" {
				assert.ErrorContains(t, err, c.expectedErrorMsg)
				return
			}
			assert.NoError(t, err)
			assertPodSecurityContext(t, getAgentDeployment(manifests))
			c.verifyManifests(t, manifests)
		})
	}
}

func assertPodSecurityContext(t *testing.T, deploy *appsv1.Deployment) {
	t.Helper()

	if !assert.NotNil(t, deploy) {
		return
	}
	expected := &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	assert.Equal(t, expected, deploy.Spec.Template.Spec.SecurityContext)
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

func newAddOn(name, namespace string) *addonv1beta1.ManagedClusterAddOn {
	return &addonv1beta1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.ManagedClusterAddOnSpec{},
		Status: addonv1beta1.ManagedClusterAddOnStatus{
			// Simulates what the registration controller sets in production.
			// In v1beta1, Status.Namespace is set by the registration controller
			// before the agentdeploy controller calls Manifests().
			Namespace: name,
			Registrations: []addonv1beta1.RegistrationConfig{
				{
					Type: addonv1beta1.KubeClient,
					KubeClient: &addonv1beta1.KubeClientConfig{
						Subject: addonv1beta1.KubeClientSubject{
							BaseSubject: addonv1beta1.BaseSubject{
								User:   "system:serviceaccount:" + name + ":cluster-proxy",
								Groups: []string{"system:serviceaccounts:" + name},
							},
						},
					},
				},
			},
		},
	}
}

func newManagedProxyConfigReference(name string) addonv1beta1.ConfigReference {
	return addonv1beta1.ConfigReference{
		ConfigGroupResource: addonv1beta1.ConfigGroupResource{
			Group:    "proxy.open-cluster-management.io",
			Resource: "managedproxyconfigurations",
		},
		DesiredConfig: &addonv1beta1.ConfigSpecHash{
			ConfigReferent: addonv1beta1.ConfigReferent{
				Name: name,
			},
			SpecHash: "dummy",
		},
	}
}

func newAddOndDeploymentConfigReference(name, namespace string) addonv1beta1.ConfigReference {
	return addonv1beta1.ConfigReference{
		ConfigGroupResource: addonv1beta1.ConfigGroupResource{
			Group:    "addon.open-cluster-management.io",
			Resource: "addondeploymentconfigs",
		},
		DesiredConfig: &addonv1beta1.ConfigSpecHash{
			ConfigReferent: addonv1beta1.ConfigReferent{
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

func newAddOnDeploymentConfig(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
		},
	}
}

func newAddOnDeploymentConfigWithVariables(name, namespace string,
	variables ...addonv1beta1.CustomizedVariable) *addonv1beta1.AddOnDeploymentConfig {
	config := newAddOnDeploymentConfig(name, namespace)
	config.Spec.CustomizedVariables = variables
	return config
}

var fakeCA = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUM2VENDQWRFQ0ZHSG5lTUpBQ1NjR2lRSnA2K1RYa0NKRVBTVitNQTBHQ1NxR1NJYjNEUUVCQ3dVQU1ERXgKRmpBVUJnTlZCQW9NRFU5d1pXNVRhR2xtZENCQlEwMHhGekFWQmdOVkJBTU1EbmQzZHk1eVpXUm9ZWFF1WTI5dApNQjRYRFRJek1URXhNakV5TURZME4xb1hEVEkwTVRFeE1URXlNRFkwTjFvd01URVdNQlFHQTFVRUNnd05UM0JsCmJsTm9hV1owSUVGRFRURVhNQlVHQTFVRUF3d09kM2QzTG5KbFpHaGhkQzVqYjIwd2dnRWlNQTBHQ1NxR1NJYjMKRFFFQkFRVUFBNElCRHdBd2dnRUtBb0lCQVFEUXZMbHFjYXpYZmxXNXgzcVFDSE52ZjNqTFNCY0QrY3pCczFoMApUV0p2TWEvWVd2T2MrK3VNWXg2OW1RaXRCWEFaMEsyUVpQa1BYK2lEc244Mk9mNklYTUpUSVpmZk1Wb3g4UmtqCkNlQ00vdlNaMzExVGlwa0NkaGVTbnp0WElhek1hN0ZZS3BVT2htYTF3L2RReFcvcnIwandwRG9TMFUvN0xhWGwKNHF2bUF4Wk1iSHVWaFk2S0RZSGJ2MEdKYWdqekJtVkpieTZlMFg3MkozL05ZME1KT2plYklrOTEydjBXZ1pUKwo3UWU0a29scVY1MkQvaUhYV0xFUzhXMWQrMFZUbnlRaFAzY3RvNWp3TFZyWnQ2NDFZL0lRc2ZNQ0w1bGdhVTF0Cm9UMlcvQ3F1amw5aCt0UCt2SG1rNk5JZXk2RUNIdm1MV0xLbU5nblp2M0d0bVdnZEFnTUJBQUV3RFFZSktvWkkKaHZjTkFRRUxCUUFEZ2dFQkFKSjBnd0UxSUR4SlNzaUd1TGxDMlVGV2J3U0RHMUVEK3VlQWYvRDRlV0VSWFZDUAo4aVdZZC9RckdsakYxNGxvZllHb280Vk5PL28xQWJQS2gveXB4UW16REdrVE1NaGg2WFg1bExob3RZWHZERlM2CmlkQXk5TFpiWDFUQnV5UEcwNmorbkI4eEtEY3F4aFNLYTlNb0trck9XcmtGbnFZS2syQzIyZGRvZVlZdlRjR2cKK2JmZ3RSWFJRUFdQRmt2NDR5MGlMZVh0S0VMbHBQMkMyQW5JQkU4b2hzY0JiYnloVmptem5YS1dFSTg3T0xmUgoxNDJBOWoydlVVQW80T0o5d1JCei8raDFXUXkyL3prclVUMW90MFdienY1cy91YmlUQkRpSjlQQ0k4YkZmZXplCnpDbCthbEE5aUFJdGt4OVdZS2pzaDFuVHEzTnJwVWM0MXBJWlFBQT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="

func newAddOnDeploymentConfigWithHttpsProxy(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
			ProxyConfig: addonv1beta1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "https://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}
func newAddOnDeploymentConfigWithHttpProxy(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
			ProxyConfig: addonv1beta1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "http://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}

func newAddOnDeploymentConfigWithResourcesRequirement(name, namespace, containerID string,
	resources corev1.ResourceRequirements) *addonv1beta1.AddOnDeploymentConfig {

	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			ResourceRequirements: []addonv1beta1.ContainerResourceRequirements{
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

func getAgentNetworkPolicy(manifests []runtime.Object) *networkingv1.NetworkPolicy {
	for _, manifest := range manifests {
		if np, ok := manifest.(*networkingv1.NetworkPolicy); ok {
			if np.Name == "cluster-proxy-proxy-agent-network-policy" {
				return np
			}
		}
	}
	return nil
}

// networkPolicyHasAllowAllEgress reports whether any egress rule matches all
// destinations and ports (empty to and empty ports), used as the service-proxy
// backend exemption for arbitrary Cluster-Proxy-Port targets.
func networkPolicyHasAllowAllEgress(np *networkingv1.NetworkPolicy) bool {
	if np == nil {
		return false
	}
	for _, rule := range np.Spec.Egress {
		if len(rule.To) == 0 && len(rule.Ports) == 0 {
			return true
		}
	}
	return false
}

// networkPolicyAllowsEgressTCPPort is true only when a port-restricted egress
// rule (non-empty ports) explicitly lists the TCP port — not via allow-all.
func networkPolicyAllowsEgressTCPPort(np *networkingv1.NetworkPolicy, port int32) bool {
	if np == nil {
		return false
	}
	for _, rule := range np.Spec.Egress {
		if len(rule.Ports) == 0 {
			continue
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port {
				if p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP {
					return true
				}
			}
		}
	}
	return false
}

func getDeploymentContainer(deploy *appsv1.Deployment, name string) *corev1.Container {
	if deploy == nil {
		return nil
	}
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == name {
			return &deploy.Spec.Template.Spec.Containers[i]
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
