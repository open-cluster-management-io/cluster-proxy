package agent

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"github.com/pkg/errors"
	certificatesv1 "k8s.io/api/certificates/v1"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed manifests
var FS embed.FS

const (
	ProxyAgentSignerName = "open-cluster-management.io/proxy-agent-signer"
)

func NewAgentAddon(scheme *runtime.Scheme, caCertData, caKeyData []byte, runtimeClient client.Client, nativeClient kubernetes.Interface) (agent.AgentAddon, error) {
	return addonfactory.NewAgentAddonFactory(common.AddonName, FS, "manifests/charts/addon-agent").
		WithAgentRegistrationOption(&agent.RegistrationOption{
			CSRConfigurations: func(cluster *clusterv1.ManagedCluster) []addonv1alpha1.RegistrationConfig {
				return []addonv1alpha1.RegistrationConfig{
					{
						SignerName: ProxyAgentSignerName,
						Subject: addonv1alpha1.Subject{
							User: common.SubjectUserClusterProxyAgent,
							Groups: []string{
								common.SubjectGroupClusterProxy,
							},
						},
					},
					{
						SignerName: csrv1.KubeAPIServerClientSignerName,
						Subject: addonv1alpha1.Subject{
							User: common.SubjectUserClusterAddonAgent,
							Groups: []string{
								common.SubjectGroupClusterProxy,
							},
						},
					},
				}
			},
			CSRApproveCheck: func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn, csr *csrv1.CertificateSigningRequest) bool {
				return cluster.Spec.HubAcceptsClient
			},
			PermissionConfig: utils.NewRBACPermissionConfigBuilder(nativeClient).
				WithStaticRole(&rbacv1.Role{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-proxy-addon-agent",
					},
					Rules: []rbacv1.PolicyRule{
						{
							APIGroups: []string{"coordination.k8s.io"},
							Verbs:     []string{"*"},
							Resources: []string{"leases"},
						},
					},
				}).
				WithStaticRoleBinding(&rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-proxy-addon-agent",
					},
					RoleRef: rbacv1.RoleRef{
						Kind: "Role",
						Name: "cluster-proxy-addon-agent",
					},
					Subjects: []rbacv1.Subject{
						{
							Kind: rbacv1.GroupKind,
							Name: common.SubjectGroupClusterProxy,
						},
					},
				}).
				Build(),
			CSRSign: CustomSignerWithExpiry(ProxyAgentSignerName, caKeyData, caCertData, time.Hour*24*180),
		}).
		WithInstallStrategy(agent.InstallAllStrategy(common.AddonInstallNamespace)).
		WithGetValuesFuncs(GetClusterProxyValueFunc(runtimeClient, nativeClient, caCertData)).
		BuildHelmAgentAddon()

}

func GetClusterProxyValueFunc(runtimeClient client.Client, nativeClient kubernetes.Interface, caCertData []byte) addonfactory.GetValuesFunc {
	return func(cluster *clusterv1.ManagedCluster,
		addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
		// prepping
		clusterAddon := &addonv1alpha1.ClusterManagementAddOn{}
		if err := runtimeClient.Get(context.TODO(), types.NamespacedName{
			Name: common.AddonName,
		}, clusterAddon); err != nil {
			return nil, err
		}
		proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
		if err := runtimeClient.Get(context.TODO(), types.NamespacedName{
			Name: clusterAddon.Spec.AddOnConfiguration.CRName,
		}, proxyConfig); err != nil {
			return nil, err
		}
		// this is how we set the right ingress endpoint for proxy servers to
		// receive handshakes from proxy agents:
		// 1. upon "Hostname" type, use the prescribed hostname directly
		// 2. upon "LoadBalancerService" type, use the first entry in the ip lists
		// 3. otherwise defaulted to the in-cluster service endpoint
		serviceEntryPoint := proxyConfig.Spec.ProxyServer.InClusterServiceName + "." + proxyConfig.Spec.ProxyServer.Namespace
		// find the referenced proxy load-balancer prescribed in the proxy config if there's any
		var proxyServerLoadBalancer *corev1.Service
		if proxyConfig.Spec.ProxyServer.Entrypoint.Type == proxyv1alpha1.EntryPointTypeLoadBalancerService {
			entrySvc, err := nativeClient.CoreV1().
				Services(proxyConfig.Spec.ProxyServer.Namespace).
				Get(context.TODO(),
					proxyConfig.Spec.ProxyServer.Entrypoint.LoadBalancerService.Name,
					metav1.GetOptions{})
			if err != nil {
				return nil, errors.Wrapf(err, "failed getting proxy loadbalancer")
			}
			if len(entrySvc.Status.LoadBalancer.Ingress) == 0 {
				return nil, fmt.Errorf("the load-balancer service for proxy-server ingress is not yet provisioned")
			}
			proxyServerLoadBalancer = entrySvc
		}
		addonAgentArgs := []string{
			"--hub-kubeconfig=/etc/kubeconfig/kubeconfig",
			"--cluster-name=" + cluster.Name,
			"--proxy-server-namespace=" + proxyConfig.Spec.ProxyServer.Namespace,
		}
		annotations := make(map[string]string)
		switch proxyConfig.Spec.ProxyServer.Entrypoint.Type {
		case proxyv1alpha1.EntryPointTypeHostname:
			serviceEntryPoint = proxyConfig.Spec.ProxyServer.Entrypoint.Hostname.Value
		case proxyv1alpha1.EntryPointTypeLoadBalancerService:
			serviceEntryPoint = proxyServerLoadBalancer.Status.LoadBalancer.Ingress[0].IP
		case proxyv1alpha1.EntryPointTypePortForward:
			serviceEntryPoint = "127.0.0.1"
			addonAgentArgs = append(addonAgentArgs,
				"--enable-port-forward-proxy=true")
			annotations[common.AnnotationKeyConfigurationGeneration] = strconv.Itoa(int(proxyConfig.Generation))
		}

		serviceEntryPointPort := proxyConfig.Spec.ProxyServer.Entrypoint.Port
		if serviceEntryPointPort == 0 {
			serviceEntryPointPort = 8091
		}

		registry, image, tag := config.GetParsedAgentImage()
		return map[string]interface{}{
			"agentDeploymentName":      "cluster-proxy-proxy-agent",
			"serviceDomain":            "svc.cluster.local",
			"includeNamespaceCreation": true,
			"spokeAddonNamespace":      "open-cluster-management-cluster-proxy",
			"additionalProxyAgentArgs": proxyConfig.Spec.ProxyAgent.AdditionalArgs,

			"clusterName":                cluster.Name,
			"registry":                   registry,
			"image":                      image,
			"tag":                        tag,
			"proxyAgentImage":            proxyConfig.Spec.ProxyAgent.Image,
			"replicas":                   proxyConfig.Spec.ProxyAgent.Replicas,
			"base64EncodedCAData":        base64.StdEncoding.EncodeToString(caCertData),
			"serviceEntryPoint":          serviceEntryPoint,
			"serviceEntryPointPort":      serviceEntryPointPort,
			"agentDeploymentAnnotations": annotations,
			"addonAgentArgs":             addonAgentArgs,
		}, nil
	}
}

func CustomSignerWithExpiry(customSignerName string, caKey, caData []byte, duration time.Duration) agent.CSRSignerFunc {
	return func(csr *certificatesv1.CertificateSigningRequest) []byte {
		if csr.Spec.SignerName != customSignerName {
			return nil
		}
		return utils.DefaultSignerWithExpiry(caKey, caData, duration)(csr)
	}
}

const (
	ApiserverNetworkProxyLabelAddon = "open-cluster-management.io/addon"

	AgentSecretName   = "cluster-proxy-open-cluster-management.io-proxy-agent-signer-client-cert"
	AgentCASecretName = "cluster-proxy-ca"
)
