package userserver

import (
	"fmt"
	"sync"

	"sigs.k8s.io/yaml"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

// ExposedService describes a single service that is permitted to be reached
// via the service proxy path. Namespace and Service are required. Port and
// Protocol are optional: when omitted (empty string) they act as wildcards
// and any value in the incoming request will match.
//
// Future extension: an optional Clusters []string field can be added here to
// scope an entry to specific managed clusters. tsc.Cluster is already
// available in IsAllowed, so the matching logic will naturally support it.
type ExposedService struct {
	Namespace string `json:"namespace" yaml:"namespace"`
	Service   string `json:"service"   yaml:"service"`
	// Port is optional. When empty, any port is permitted.
	Port string `json:"port,omitempty" yaml:"port,omitempty"`
	// Protocol is optional. When empty, any protocol is permitted.
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
}

// matches reports whether this entry permits the given target service config.
func (e ExposedService) matches(tsc utils.TargetServiceConfig) bool {
	if e.Namespace != tsc.Namespace {
		return false
	}
	if e.Service != tsc.Service {
		return false
	}
	if e.Port != "" && e.Port != tsc.Port {
		return false
	}
	if e.Protocol != "" && e.Protocol != tsc.Proto {
		return false
	}
	return true
}

// parseAllExposedServices iterates over every key in a ConfigMap's data map,
// parses each value as a YAML list of ExposedService entries, and returns the
// combined slice. Any key name is accepted. If any key's value fails to parse
// the key name is included in the returned error so operators can locate the
// problem quickly.
func parseAllExposedServices(data map[string]string) ([]ExposedService, error) {
	var all []ExposedService
	for key, raw := range data {
		services, err := parseExposedServices(raw)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		all = append(all, services...)
	}
	return all, nil
}

// parseExposedServices unmarshals the YAML value stored under any key
// of the ConfigMap data into a slice of ExposedService entries.
// It validates that every entry has non-empty Namespace and Service fields.
func parseExposedServices(data string) ([]ExposedService, error) {
	if data == "" {
		return nil, nil
	}
	var services []ExposedService
	if err := yaml.Unmarshal([]byte(data), &services); err != nil {
		return nil, fmt.Errorf("failed to unmarshal exposed services YAML: %w", err)
	}
	for i, svc := range services {
		if svc.Namespace == "" {
			return nil, fmt.Errorf("entry[%d]: namespace is required", i)
		}
		if svc.Service == "" {
			return nil, fmt.Errorf("entry[%d]: service is required", i)
		}
	}
	return services, nil
}

// ServiceAllowlist is a thread-safe, dynamically updatable set of permitted
// services for the service proxy path. A nil or empty allowlist means no
// services are permitted (default-deny).
//
// Callers obtain a read lock only for the duration of the IsAllowed check, so
// concurrent HTTP handlers are not serialised against each other.
type ServiceAllowlist struct {
	mu       sync.RWMutex
	services []ExposedService
}

// update atomically replaces the current allowlist with the supplied entries.
// Passing nil or an empty slice enforces deny-all.
func (a *ServiceAllowlist) update(services []ExposedService) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.services = services
}

// IsAllowed reports whether the given target service config is permitted by
// the current allowlist. Returns false when the allowlist is empty (deny-all).
func (a *ServiceAllowlist) IsAllowed(tsc utils.TargetServiceConfig) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, svc := range a.services {
		if svc.matches(tsc) {
			return true
		}
	}
	return false
}

// Len returns the number of entries currently in the allowlist.
// Primarily useful for logging/diagnostics.
func (a *ServiceAllowlist) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.services)
}
