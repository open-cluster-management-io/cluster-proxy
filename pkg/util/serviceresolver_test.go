package util

import (
	"testing"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

func TestIsServiceResolverLegal(t *testing.T) {
	testcases := []struct {
		name     string
		mpsr     *proxyv1alpha1.ManagedProxyServiceResolver
		expected bool
	}{
		{
			name:     "managed cluster selector type mismatch",
			mpsr:     &proxyv1alpha1.ManagedProxyServiceResolver{},
			expected: false,
		},
		{
			name: "cluster set nil",
			mpsr: &proxyv1alpha1.ManagedProxyServiceResolver{
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: "Test",
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "clusterSet1",
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "service selector type mismatch",
			mpsr: &proxyv1alpha1.ManagedProxyServiceResolver{
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "clusterSet1",
						},
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: "Test",
						ServiceRef: &proxyv1alpha1.ServiceRef{
							Namespace: "ns1",
							Name:      "service1",
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "service ref nil",
			mpsr: &proxyv1alpha1.ManagedProxyServiceResolver{
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "clusterSet1",
						},
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
					},
				},
			},
			expected: false,
		},
		{
			name: "legal",
			mpsr: &proxyv1alpha1.ManagedProxyServiceResolver{
				Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
					ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
						Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
						ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
							Name: "clusterSet1",
						},
					},
					ServiceSelector: proxyv1alpha1.ServiceSelector{
						Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
						ServiceRef: &proxyv1alpha1.ServiceRef{
							Namespace: "ns1",
							Name:      "service1",
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			actual := IsServiceResolverLegal(tc.mpsr)
			if actual != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, actual)
			}
		})
	}
}
