package config

import (
	"fmt"
	"strings"
)

var AgentImageName string

func ValidateAgentImage() error {
	if len(AgentImageName) == 0 {
		return fmt.Errorf("should set --agent-image-name")
	}
	return nil
}

func GetParsedAgentImage() (string, string, string, error) {
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
