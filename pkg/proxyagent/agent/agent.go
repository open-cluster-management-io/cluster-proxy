package agent

import (
	"context"
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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/utils"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
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
	ProxyAgentSignerName = "open-cluster-management.io/proxy-agent-signer"

	// serviceDomain must added because coreDNS is not recursive resolver, and go dns client assume it is.
	// See more details: https://coredns.io/manual/setups/#recursive-resolver; https://github.com/golang/go/blob/6f445a9db55f65e55c5be29d3c506ecf3be37915/src/net/dnsclient_unix.go#L666
	// TODO: the serviceDomain should be configurable.
	serviceDomain = "svc.cluster.local"
)

func NewAgentAddon(
	signer selfsigned.SelfSigner,
	signerNamespace string,
	v1CSRSupported bool,
	runtimeClient client.Client,
	nativeClient kubernetes.Interface,
	agentInstallAll bool,
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
	if v1CSRSupported {
		regConfigs = append(regConfigs, addonv1alpha1.RegistrationConfig{
			SignerName: ProxyAgentSignerName,
			Subject: addonv1alpha1.Subject{
				User: common.SubjectUserClusterProxyAgent,
				Groups: []string{
					common.SubjectGroupClusterProxy,
				},
			},
		})
	}

	agentFactory := addonfactory.NewAgentAddonFactory(common.AddonName, FS, "manifests/charts/addon-agent").
		WithAgentRegistrationOption(&agent.RegistrationOption{
			CSRConfigurations: func(cluster *clusterv1.ManagedCluster) []addonv1alpha1.RegistrationConfig {
				return regConfigs
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
		WithConfigGVRs(
			schema.GroupVersionResource{
				Group:    proxyv1alpha1.GroupVersion.Group,
				Version:  proxyv1alpha1.GroupVersion.Version,
				Resource: "managedproxyconfigurations",
			},
			addonfactory.AddOnDeploymentConfigGVR,
		).
		WithGetValuesFuncs(
			GetClusterProxyValueFunc(runtimeClient, nativeClient, signerNamespace, caCertData, v1CSRSupported),
			addonfactory.GetAddOnDeloymentConfigValues(
				addonfactory.NewAddOnDeloymentConfigGetter(addonClient),
				toAgentAddOnChartValues,
			),
		)

	if agentInstallAll {
		agentFactory.WithInstallStrategy(agent.InstallAllStrategy(config.AddonInstallNamespace))
	}

	return agentFactory.BuildHelmAgentAddon()
}

func GetClusterProxyValueFunc(
	runtimeClient client.Client,
	nativeClient kubernetes.Interface,
	signerNamespace string,
	caCertData []byte,
	v1CSRSupported bool) addonfactory.GetValuesFunc {
	return func(cluster *clusterv1.ManagedCluster,
		addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {

		managedProxyConfigurations := []string{}
		for _, configReference := range addon.Status.ConfigReferences {
			if config.IsManagedProxyConfiguration(configReference.ConfigGroupResource) {
				managedProxyConfigurations = append(managedProxyConfigurations, configReference.Name)
			}
		}

		// only handle there is only one managed proxy configuration for one addon
		// TODO may consider to handle mutiple managed proxy configurations for one addon
		if len(managedProxyConfigurations) != 1 {
			return nil, fmt.Errorf("unexpected managed proxy configurations: %v", managedProxyConfigurations)
		}

		proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
		if err := runtimeClient.Get(context.TODO(), types.NamespacedName{
			Name: managedProxyConfigurations[0],
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

		// If v1 CSR is not supported in the hub cluster, copy and apply the secret named
		// "agent-client" to the managed clusters.
		certDataBase64, keyDataBase64 := "", ""
		if !v1CSRSupported {
			agentClientSecret, err := nativeClient.CoreV1().
				Secrets(signerNamespace).
				Get(context.TODO(), common.AgentClientSecretName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			certDataBase64 = base64.StdEncoding.EncodeToString(agentClientSecret.Data[corev1.TLSCertKey])
			keyDataBase64 = base64.StdEncoding.EncodeToString(agentClientSecret.Data[corev1.TLSPrivateKeyKey])
		}

		registry, image, tag, err := config.GetParsedAgentImage(proxyConfig.Spec.ProxyAgent.Image)
		if err != nil {
			return nil, err
		}

		// Get agentIndentifiers and servicesToExpose.
		// agetnIdentifiers is used in `--agent-identifiers` flag in addon-agent-deployment.yaml.
		// servicesToExpose defines the services we want to expose to the hub.

		// List all available managedClusterSets
		managedClusterSetList := &clusterv1beta1.ManagedClusterSetList{}
		err = runtimeClient.List(context.TODO(), managedClusterSetList)
		if err != nil {
			return nil, err
		}

		managedClusterSetMap, err := managedClusterSetsToFilteredMap(managedClusterSetList.Items, cluster.Labels)
		if err != nil {
			return nil, err
		}

		// List all available serviceResolvers
		serviceResolverList := &proxyv1alpha1.ManagedProxyServiceResolverList{}
		err = runtimeClient.List(context.TODO(), serviceResolverList)
		if err != nil {
			return nil, err
		}

		servicesToExpose := removeDupAndSortServices(managedProxyServiceResolverToFilterServiceToExpose(serviceResolverList.Items, managedClusterSetMap, cluster.Name))

		// add default agentIdentifiers
		var aids []string
		aids = append(aids, fmt.Sprintf("host=%s", cluster.Name))
		aids = append(aids, fmt.Sprintf("host=%s.%s", cluster.Name, config.AddonInstallNamespace))

		// add servicesToExpose into aids
		for _, s := range servicesToExpose {
			aids = append(aids, fmt.Sprintf("host=%s", s.Host))
		}
		agentIdentifiers := strings.Join(aids, "&")

		return map[string]interface{}{
			"agentDeploymentName":           "cluster-proxy-proxy-agent",
			"serviceDomain":                 serviceDomain,
			"includeNamespaceCreation":      true,
			"spokeAddonNamespace":           addon.Spec.InstallNamespace,
			"additionalProxyAgentArgs":      proxyConfig.Spec.ProxyAgent.AdditionalArgs,
			"clusterName":                   cluster.Name,
			"registry":                      registry,
			"image":                         image,
			"tag":                           tag,
			"proxyAgentImage":               proxyConfig.Spec.ProxyAgent.Image,
			"proxyAgentImagePullSecrets":    proxyConfig.Spec.ProxyAgent.ImagePullSecrets,
			"replicas":                      proxyConfig.Spec.ProxyAgent.Replicas,
			"base64EncodedCAData":           base64.StdEncoding.EncodeToString(caCertData),
			"serviceEntryPoint":             serviceEntryPoint,
			"serviceEntryPointPort":         serviceEntryPointPort,
			"agentDeploymentAnnotations":    annotations,
			"addonAgentArgs":                addonAgentArgs,
			"includeStaticProxyAgentSecret": !v1CSRSupported,
			"staticProxyAgentSecretCert":    certDataBase64,
			"staticProxyAgentSecretKey":     keyDataBase64,
			// support to access not only but also other other services on managed cluster
			"agentIdentifiers": agentIdentifiers,
			"servicesToExpose": servicesToExpose,
		}, nil
	}
}

type serviceToExpose struct {
	Host         string `json:"host"`
	ExternalName string `json:"externalName"`
}

func CustomSignerWithExpiry(customSignerName string, caKey, caData []byte, duration time.Duration) agent.CSRSignerFunc {
	return func(csr *csrv1.CertificateSigningRequest) []byte {
		if csr.Spec.SignerName != customSignerName {
			return nil
		}
		return utils.DefaultSignerWithExpiry(caKey, caData, duration)(csr)
	}
}

const (
	ApiserverNetworkProxyLabelAddon = "open-cluster-management.io/addon"
	AgentSecretName                 = "cluster-proxy-open-cluster-management.io-proxy-agent-signer-client-cert"
	AgentCASecretName               = "cluster-proxy-ca"
)

func managedClusterSetsToFilteredMap(managedClusterSets []clusterv1beta1.ManagedClusterSet, clusterlabels map[string]string) (map[string]clusterv1beta1.ManagedClusterSet, error) {
	managedClusterSetMap := map[string]clusterv1beta1.ManagedClusterSet{}
	for i := range managedClusterSets {
		mcs := managedClusterSets[i]

		// deleted managedClusterSets are not included in the list
		if !mcs.DeletionTimestamp.IsZero() {
			continue
		}

		// only cluseterSet cover current cluster include in the list.
		selector, err := clusterv1beta1.BuildClusterSelector(&mcs)
		if err != nil {
			return nil, err
		}
		if !selector.Matches(labels.Set(clusterlabels)) {
			continue
		}

		managedClusterSetMap[mcs.Name] = mcs
	}
	return managedClusterSetMap, nil
}

func managedProxyServiceResolverToFilterServiceToExpose(serviceResolvers []proxyv1alpha1.ManagedProxyServiceResolver, managedClusterSetMap map[string]clusterv1beta1.ManagedClusterSet, clusterName string) []serviceToExpose {
	servicesToExpose := []serviceToExpose{}
	for i := range serviceResolvers {
		sr := serviceResolvers[i]

		// illegal serviceResolvers are not included in the list
		if !util.IsServiceResolverLegal(&sr) {
			continue
		}

		// deleted serviceResolvers are not included in the list
		if !sr.DeletionTimestamp.IsZero() {
			continue
		}

		// filter serviceResolvers by managedClusterSet
		if _, ok := managedClusterSetMap[sr.Spec.ManagedClusterSelector.ManagedClusterSet.Name]; !ok {
			continue
		}

		servicesToExpose = append(servicesToExpose, convertManagedProxyServiceResolverToService(clusterName, sr))
	}
	return servicesToExpose
}

func convertManagedProxyServiceResolverToService(clusterName string, sr proxyv1alpha1.ManagedProxyServiceResolver) serviceToExpose {
	return serviceToExpose{
		Host:         util.GenerateServiceURL(clusterName, sr.Spec.ServiceSelector.ServiceRef.Namespace, sr.Spec.ServiceSelector.ServiceRef.Name),
		ExternalName: fmt.Sprintf("%s.%s", sr.Spec.ServiceSelector.ServiceRef.Name, sr.Spec.ServiceSelector.ServiceRef.Namespace),
	}
}

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

func toAgentAddOnChartValues(config addonv1alpha1.AddOnDeploymentConfig) (addonfactory.Values, error) {
	values := addonfactory.Values{}
	for _, variable := range config.Spec.CustomizedVariables {
		values[variable.Name] = variable.Value
	}

	if config.Spec.NodePlacement != nil {
		values["nodeSelector"] = config.Spec.NodePlacement.NodeSelector
		values["tolerations"] = config.Spec.NodePlacement.Tolerations
	}

	return values, nil
}
