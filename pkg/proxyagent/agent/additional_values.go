package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// annotationNodeSelector is key name of nodeSelector annotation synced from mch
	annotationNodeSelector = "open-cluster-management/nodeSelector"
)

func GetClusterProxyAdditionalValueFunc(
	runtimeClient client.Client,
	nativeClient kubernetes.Interface,
	signerNamespace string,
	enableServiceProxy bool,
) addonfactory.GetValuesFunc {
	return func(cluster *clusterv1.ManagedCluster,
		addon *addonv1alpha1.ManagedClusterAddOn) (addonfactory.Values, error) {
		proxyConfig := &proxyv1alpha1.ManagedProxyConfiguration{}
		if err := runtimeClient.Get(context.TODO(), types.NamespacedName{
			Name: ManagedClusterConfigurationName,
		}, proxyConfig); err != nil {
			return nil, err
		}

		// get image of proxy-agent(cluster-proxy-addon)
		clusterProxyAddonImage := proxyConfig.Spec.ProxyAgent.Image

		// get image of agent-addon(cluster-proxy)
		var clusterProxyImage string
		if len(config.AgentImageName) == 0 {
			clusterProxyImage = clusterProxyAddonImage
		} else {
			clusterProxyImage = config.AgentImageName
		}

		registry, image, tag, err := config.ParseImage(clusterProxyImage)
		if err != nil {
			return nil, err
		}

		values := map[string]interface{}{
			"registry":        registry,
			"image":           image,
			"tag":             tag,
			"proxyAgentImage": clusterProxyAddonImage,
		}

		// get node selector
		nodeSelector, err := getNodeSelector(cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to get nodeSelector from managedCluster. %v", err)
		}
		if len(nodeSelector) != 0 {
			values["nodeSelector"] = nodeSelector
		}

		// get service-proxy cert and key
		if enableServiceProxy {
			values["enableServiceProxy"] = true
			serviceProxySecretKey, serviceProxySecretCert, err := getServerCertificatesFromSecret(nativeClient, signerNamespace)
			if err != nil {
				return nil, err
			}
			values["serviceProxySecretCert"] = base64.StdEncoding.EncodeToString(serviceProxySecretCert)
			values["serviceProxySecretKey"] = base64.StdEncoding.EncodeToString(serviceProxySecretKey)
		}

		return values, nil
	}
}

func getNodeSelector(managedCluster *clusterv1.ManagedCluster) (map[string]string, error) {
	nodeSelector := map[string]string{}
	if common.IsClusterSelfManaged(managedCluster) {
		annotations := managedCluster.GetAnnotations()
		if nodeSelectorString, ok := annotations[annotationNodeSelector]; ok {
			if err := json.Unmarshal([]byte(nodeSelectorString), &nodeSelector); err != nil {
				return nodeSelector, fmt.Errorf("failed to unmarshal nodeSelector annotation of cluster %s, %v", managedCluster.GetName(), err)
			}
		}
	}

	return nodeSelector, nil
}

func getServerCertificatesFromSecret(nativeClient kubernetes.Interface, secretNamespace string) ([]byte, []byte, error) {
	secret, err := nativeClient.CoreV1().Secrets(secretNamespace).Get(context.TODO(), constant.ServerCertSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to get secret %s in the namespace %s: %v", constant.ServerCertSecretName, secretNamespace, err)

	}
	cert, ok := secret.Data["tls.crt"]
	if !ok {
		return nil, nil, fmt.Errorf("secret %s does not contain tls.crt", constant.ServerCertSecretName)
	}
	key, ok := secret.Data["tls.key"]
	if !ok {
		return nil, nil, fmt.Errorf("secret %s does not contain tls.key", constant.ServerCertSecretName)
	}
	return key, cert, nil
}
