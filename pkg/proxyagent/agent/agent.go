package agent

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed manifests
var FS embed.FS

const (
	ManagedClusterConfigurationName = "cluster-proxy"

	ProxyAgentSignerName = "open-cluster-management.io/proxy-agent-signer"

	// serviceDomain must added because go dns client won't recursivly search CNAME.
	// See more details: https://coredns.io/manual/setups/#recursive-resolver; https://github.com/golang/go/blob/6f445a9db55f65e55c5be29d3c506ecf3be37915/src/net/dnsclient_unix.go#L666
	// The default value is "svc.cluster.local". We can also set a CustomizedVariables with key "serviceDomain" to overwrite it.
	serviceDomain = "svc.cluster.local"
)

func NewAgentAddon(
	signer selfsigned.SelfSigner,
	signerNamespace string,
	runtimeClient client.Client,
	nativeClient kubernetes.Interface,
	enableKubeApiProxy bool,
	enableServiceProxy bool,
	addonClient addonclient.Interface) (agent.AgentAddon, error) {
	caCertData, caKeyData, err := signer.CA().Config.GetPEMBytes()
	if err != nil {
		return nil, err
	}

	regConfigs := []addonv1alpha1.RegistrationConfig{
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
	// Register the custom signer CSR option if V1 csr is supported
	// caculate a hash value of signer ca data and add it to the organizationUnits of the subject
	signerHash := sha256.Sum256(signer.CAData())
	regConfigs = append(regConfigs, addonv1alpha1.RegistrationConfig{
		SignerName: ProxyAgentSignerName,
		Subject: addonv1alpha1.Subject{
			User: common.SubjectUserClusterProxyAgent,
			Groups: []string{
				common.SubjectGroupClusterProxy,
			},
			OrganizationUnits: []string{
				fmt.Sprintf("signer-%x", base64.StdEncoding.EncodeToString(signerHash[:])),
			},
		},
	})

	agentFactory := addonfactory.NewAgentAddonFactory(common.AddonName, FS, "manifests/charts/addon-agent").
		WithAgentRegistrationOption(&agent.RegistrationOption{
			CSRConfigurations: func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn) ([]addonv1alpha1.RegistrationConfig, error) {
				return regConfigs, nil
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
							Kind:     rbacv1.GroupKind,
							Name:     common.SubjectGroupClusterProxy,
							APIGroup: rbacv1.GroupName,
						},
					},
				}).
				WithStaticClusterRole(&rbacv1.ClusterRole{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-proxy-addon-agent-tokenreview",
					},
					Rules: []rbacv1.PolicyRule{
						{
							APIGroups: []string{"authentication.k8s.io"},
							Verbs:     []string{"create"},
							Resources: []string{"tokenreviews"},
						},
					},
				}).
				WithStaticClusterRoleBinding(&rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-proxy-addon-agent-tokenreview",
					},
					RoleRef: rbacv1.RoleRef{
						Kind: "ClusterRole",
						Name: "cluster-proxy-addon-agent-tokenreview",
					},
					Subjects: []rbacv1.Subject{
						{
							Kind:     rbacv1.GroupKind,
							Name:     common.SubjectGroupClusterProxy,
							APIGroup: rbacv1.GroupName,
						},
					},
				}).
				Build(),
			CSRSign: CustomSignerWithExpiry(ProxyAgentSignerName, caKeyData, caCertData, time.Hour*24*180),
		}).
		WithConfigGVRs(
			schema.GroupVersionResource{
				Group:    proxyv1alpha1.GroupVersion.Group,
				Version:  proxyv1alpha1.GroupVersion.Version,
				Resource: "managedproxyconfigurations",
			},
			utils.AddOnDeploymentConfigGVR,
		).
		WithAgentDeployTriggerClusterFilter(utils.ClusterImageRegistriesAnnotationChanged).
		WithGetValuesFuncs(
			GetClusterProxyValueFunc(runtimeClient, nativeClient, signerNamespace, caCertData, enableKubeApiProxy),
			GetClusterProxyAdditionalValueFunc(runtimeClient, nativeClient, signerNamespace, enableServiceProxy),
			addonfactory.GetAddOnDeploymentConfigValues(
				utils.NewAddOnDeploymentConfigGetter(addonClient),
				toAgentAddOnChartValues(caCertData),
				addonfactory.ToAddOnResourceRequirementsValues,
			),
		).
		WithConfigCheckEnabledOption().
		WithAgentInstallNamespace(agentInstallNamespaceFunc(utils.NewAddOnDeploymentConfigGetter(addonClient)))

	return agentFactory.BuildHelmAgentAddon()
}

// agentInstallNamespaceFunc returns namespace from AddonDeploymentConfig, and config.DefaultAddonInstallNamespace if
// AddonDeploymentConfig is not set.
func agentInstallNamespaceFunc(getter utils.AddOnDeploymentConfigGetter) func(*addonv1alpha1.ManagedClusterAddOn) (string, error) {
	return func(addon *addonv1alpha1.ManagedClusterAddOn) (string, error) {
		ns, err := utils.AgentInstallNamespaceFromDeploymentConfigFunc(getter)(addon)
		if err != nil {
			return config.DefaultAddonInstallNamespace, err
		}
		if len(ns) == 0 {
			return config.DefaultAddonInstallNamespace, nil
		}
		return ns, nil
	}
}

func GetClusterProxyValueFunc(
	runtimeClient client.Client,
	nativeClient kubernetes.Interface,
	signerNamespace string,
	caCertData []byte,
	enableKubeApiProxy bool,
) addonfactory.GetValuesFunc {
	return func(cluster *clusterv1.ManagedCluster,
		addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
		proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
		if err := runtimeClient.Get(context.TODO(), types.NamespacedName{
			Name: ManagedClusterConfigurationName,
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
		}
		annotations[common.AnnotationKeyConfigurationGeneration] = strconv.Itoa(int(proxyConfig.Generation))

		serviceEntryPointPort := proxyConfig.Spec.ProxyServer.Entrypoint.Port
		if serviceEntryPointPort == 0 {
			serviceEntryPointPort = 8091
		}

		registry, image, tag, err := config.GetParsedAgentImage(proxyConfig.Spec.ProxyAgent.Image)
		if err != nil {
			return nil, err
		}

		// get agent namespace from addon status
		namespace := config.DefaultAddonInstallNamespace
		if len(addon.Status.Namespace) > 0 {
			namespace = addon.Status.Namespace
		}

		// servicesToExpose defines the services we want to expose to the hub.
		servicesToExpose := []serviceToExpose{}

		var aids []string

		// Add service-proxy host as the default agentIdentifier.
		// Using SHA256 to hash cluster.name to:
		// 1. Generate consistent and unique host names
		// 2. Keep host name length under DNS limit (max 64 chars)
		serviceProxyHost := util.GenerateServiceProxyHost(cluster.Name)
		aids = append(aids, fmt.Sprintf("host=%s", serviceProxyHost))

		// add default kube-apiserver agentIdentifiers
		if enableKubeApiProxy {
			aids = append(aids, fmt.Sprintf("host=%s", cluster.Name))
			aids = append(aids, fmt.Sprintf("host=%s.%s", cluster.Name, namespace))
		}
		// add servicesToExpose into aids
		for _, s := range servicesToExpose {
			aids = append(aids, fmt.Sprintf("host=%s", s.Host))
		}
		agentIdentifiers := strings.Join(aids, "&")

		values := map[string]interface{}{
			"agentDeploymentName":          "cluster-proxy-proxy-agent",
			"serviceDomain":                serviceDomain,
			"includeNamespaceCreation":     true,
			"spokeAddonNamespace":          addon.Spec.InstallNamespace,
			"additionalProxyAgentArgs":     proxyConfig.Spec.ProxyAgent.AdditionalArgs,
			"clusterName":                  cluster.Name,
			"registry":                     registry,
			"image":                        image,
			"tag":                          tag,
			"proxyAgentImage":              proxyConfig.Spec.ProxyAgent.Image,
			"proxyAgentImagePullSecrets":   proxyConfig.Spec.ProxyAgent.ImagePullSecrets,
			"replicas":                     proxyConfig.Spec.ProxyAgent.Replicas,
			"base64EncodedCAData":          base64.StdEncoding.EncodeToString(caCertData),
			"serviceEntryPoint":            serviceEntryPoint,
			"serviceEntryPointPort":        serviceEntryPointPort,
			"agentDeploymentAnnotations":   annotations,
			"addonAgentArgs":               addonAgentArgs,
			"additionalServiceCAConfigMap": proxyConfig.Spec.ProxyAgent.AdditionalServiceCAConfigMap,
			// support to access not only but also other services on managed cluster
			"agentIdentifiers":   agentIdentifiers,
			"serviceProxyHost":   serviceProxyHost,
			"servicesToExpose":   servicesToExpose,
			"enableKubeApiProxy": enableKubeApiProxy,
		}

		return values, nil
	}
}

type serviceToExpose struct {
	Host         string `json:"host"`
	ExternalName string `json:"externalName"`
}

func CustomSignerWithExpiry(customSignerName string, caKey, caData []byte, duration time.Duration) agent.CSRSignerFunc {
	return func(cluster *clusterv1.ManagedCluster, addon *addonv1alpha1.ManagedClusterAddOn, csr *csrv1.CertificateSigningRequest) ([]byte, error) {
		if csr.Spec.SignerName != customSignerName {
			return nil, nil
		}
		return utils.DefaultSignerWithExpiry(caKey, caData, duration)(cluster, addon, csr)
	}
}

const (
	ApiserverNetworkProxyLabelAddon = "open-cluster-management.io/addon"
	AgentSecretName                 = "cluster-proxy-open-cluster-management.io-proxy-agent-signer-client-cert"
	AgentCASecretName               = "cluster-proxy-ca"
)

func removeDupAndSortServices(services []serviceToExpose) []serviceToExpose {
	newServices := []serviceToExpose{}

	// remove dup
	encountered := map[string]bool{}
	for i := range services {
		s := services[i]
		if !encountered[s.Host] {
			encountered[s.Host] = true
			newServices = append(newServices, s)
		}
	}

	// sort
	sort.Slice(newServices, func(i, j int) bool {
		return newServices[i].Host < newServices[j].Host
	})

	return newServices
}

func toAgentAddOnChartValues(caCertData []byte) func(config addonv1alpha1.AddOnDeploymentConfig) (addonfactory.Values, error) {
	return func(config addonv1alpha1.AddOnDeploymentConfig) (addonfactory.Values, error) {
		values := addonfactory.Values{}
		for _, variable := range config.Spec.CustomizedVariables {
			values[variable.Name] = variable.Value
		}

		if config.Spec.NodePlacement != nil {
			values["nodeSelector"] = config.Spec.NodePlacement.NodeSelector
			values["tolerations"] = config.Spec.NodePlacement.Tolerations
		}

		proxyConfig := config.Spec.ProxyConfig
		values["proxyConfig"] = map[string]string{
			"HTTP_PROXY":  proxyConfig.HTTPProxy,
			"HTTPS_PROXY": proxyConfig.HTTPSProxy,
			"NO_PROXY":    proxyConfig.NoProxy,
		}

		if strings.HasPrefix(proxyConfig.HTTPSProxy, "https") && len(proxyConfig.CABundle) != 0 {
			caCert, err := common.MergeCertificateData(proxyConfig.CABundle, caCertData)
			if err != nil {
				return nil, fmt.Errorf("faield to merge proxy env ca. %v", err)
			}

			values["base64EncodedCAData"] = base64.StdEncoding.EncodeToString(caCert)
		}
		return values, nil
	}
}
