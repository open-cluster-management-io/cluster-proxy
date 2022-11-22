// TODO (skeeey) move this to the util package
package config

import (
	"strings"

	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"

	"k8s.io/klog/v2"
)

// AgentImageName is the image of the spoke addon agent.
// Can be override via "--agent-image-name" on the hub addon manager.
var AgentImageName string

// AgentImageName is the installing namespace of the spoke addon agent.
// Can be override via "--agent-install-namespace" on the hub addon manager.
var AddonInstallNamespace = DefaultAddonInstallNamespace

const DefaultAddonInstallNamespace = "open-cluster-management-cluster-proxy"

func GetParsedAgentImage(defaultAgentImageName string) (string, string, string, error) {
	if len(AgentImageName) == 0 {
		klog.InfoS("AgentImageName is not set, use default value", "defaultAgentImageName", defaultAgentImageName)
		AgentImageName = defaultAgentImageName
	}

	return ParseImage(AgentImageName)
}

func ParseImage(imageName string) (string, string, string, error) {
	imgParts := strings.Split(imageName, "/")

	registry := strings.Join(imgParts[0:len(imgParts)-1], "/")

	parts := strings.Split(imgParts[len(imgParts)-1], ":")
	image := parts[0]

	tag := "latest"
	if len(parts) >= 2 {
		tag = parts[len(parts)-1]
	}

	return registry, image, tag, nil
}

func FindDefaultManagedProxyConfigurationName(cma *addonv1alpha1.ClusterManagementAddOn) string {
	for _, config := range cma.Spec.SupportedConfigs {
		if !IsManagedProxyConfiguration(config.ConfigGroupResource) {
			continue
		}

		if config.DefaultConfig == nil {
			continue
		}

		return config.DefaultConfig.Name
	}

	return ""
}

func IsManagedProxyConfiguration(gr addonv1alpha1.ConfigGroupResource) bool {
	if gr.Group != proxyv1alpha1.GroupVersion.Group {
		return false
	}

	if gr.Resource != "managedproxyconfigurations" {
		return false
	}

	return true
}
