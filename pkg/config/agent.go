package config

import (
	"fmt"
	"strings"

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
	imgParts := strings.Split(AgentImageName, "/")
	if len(imgParts) != 2 && len(imgParts) != 3 {
		// image name without registry is also legal.
		return "", "", "", fmt.Errorf("invalid agent image name: %s", AgentImageName)
	}

	registry := strings.Join(imgParts[0:len(imgParts)-1], "/")

	parts := strings.Split(imgParts[len(imgParts)-1], ":")
	image := parts[0]

	tag := "latest"
	if len(parts) >= 2 {
		tag = parts[len(parts)-1]
	}

	return registry, image, tag, nil
}
