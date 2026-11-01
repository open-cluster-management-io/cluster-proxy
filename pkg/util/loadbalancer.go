package util

import (
	corev1 "k8s.io/api/core/v1"
)

// LoadBalancerIngressAddress returns the first address published in the status
// of a LoadBalancer service, or an empty string if none has been provisioned
// yet. Depending on the cloud provider, an ingress point carries either an IP
// (e.g. GCP, Azure, MetalLB) or a DNS hostname (e.g. AWS ELB), so both are
// considered.
func LoadBalancerIngressAddress(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if len(ingress.IP) > 0 {
			return ingress.IP
		}
		if len(ingress.Hostname) > 0 {
			return ingress.Hostname
		}
	}
	return ""
}
