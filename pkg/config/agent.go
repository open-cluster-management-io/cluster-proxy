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

func GetParsedAgentImage() (string, string, string) {
	parts := strings.Split(AgentImageName, ":")
	tag := "latest"
	if len(parts) >= 2 {
		tag = parts[len(parts)-1]
	}
	imgParts := strings.Split(parts[0], "/")
	return strings.Join(imgParts[0:len(imgParts)-1], "/"), imgParts[len(imgParts)-1], tag
}
