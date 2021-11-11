package config

import "fmt"

var AgentImageName string

func ValidateAgentImage() error {
	if len(AgentImageName) == 0 {
		return fmt.Errorf("should set --agent-image-name")
	}
	return nil
}
