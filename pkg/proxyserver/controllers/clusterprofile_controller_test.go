package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	cpv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

func TestSetAccessProvider(t *testing.T) {
	tests := []struct {
		name              string
		existingProviders []cpv1alpha1.AccessProvider
		newProvider       cpv1alpha1.AccessProvider
		expectedCount     int
		expectedProvider  cpv1alpha1.AccessProvider
	}{
		{
			name:              "add provider to empty list",
			existingProviders: nil,
			newProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://test-server:9092/cluster1",
					CertificateAuthorityData: []byte("test-ca-data"),
				},
			},
			expectedCount: 1,
			expectedProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://test-server:9092/cluster1",
					CertificateAuthorityData: []byte("test-ca-data"),
				},
			},
		},
		{
			name: "add provider to existing list with different provider",
			existingProviders: []cpv1alpha1.AccessProvider{
				{
					Name: "other-provider",
					Cluster: clientcmdapiv1.Cluster{
						Server: "https://other-server:9092/cluster1",
					},
				},
			},
			newProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://test-server:9092/cluster1",
					CertificateAuthorityData: []byte("test-ca-data"),
				},
			},
			expectedCount: 2,
			expectedProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://test-server:9092/cluster1",
					CertificateAuthorityData: []byte("test-ca-data"),
				},
			},
		},
		{
			name: "update existing provider",
			existingProviders: []cpv1alpha1.AccessProvider{
				{
					Name: "open-cluster-management",
					Cluster: clientcmdapiv1.Cluster{
						Server:                   "https://old-server:9092/cluster1",
						CertificateAuthorityData: []byte("old-ca-data"),
					},
				},
			},
			newProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://new-server:9092/cluster1",
					CertificateAuthorityData: []byte("new-ca-data"),
					Extensions: []clientcmdapiv1.NamedExtension{
						{
							Name: "client.authentication.k8s.io/exec",
							Extension: runtime.RawExtension{
								Raw: []byte(`{"clusterName":"cluster1"}`),
							},
						},
					},
				},
			},
			expectedCount: 1,
			expectedProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://new-server:9092/cluster1",
					CertificateAuthorityData: []byte("new-ca-data"),
					Extensions: []clientcmdapiv1.NamedExtension{
						{
							Name: "client.authentication.k8s.io/exec",
							Extension: runtime.RawExtension{
								Raw: []byte(`{"clusterName":"cluster1"}`),
							},
						},
					},
				},
			},
		},
		{
			name: "update provider in list with multiple providers",
			existingProviders: []cpv1alpha1.AccessProvider{
				{
					Name: "provider-1",
					Cluster: clientcmdapiv1.Cluster{
						Server: "https://server1:9092/cluster1",
					},
				},
				{
					Name: "open-cluster-management",
					Cluster: clientcmdapiv1.Cluster{
						Server:                   "https://old-server:9092/cluster1",
						CertificateAuthorityData: []byte("old-ca-data"),
					},
				},
				{
					Name: "provider-3",
					Cluster: clientcmdapiv1.Cluster{
						Server: "https://server3:9092/cluster1",
					},
				},
			},
			newProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://new-server:9092/cluster1",
					CertificateAuthorityData: []byte("new-ca-data"),
				},
			},
			expectedCount: 3,
			expectedProvider: cpv1alpha1.AccessProvider{
				Name: "open-cluster-management",
				Cluster: clientcmdapiv1.Cluster{
					Server:                   "https://new-server:9092/cluster1",
					CertificateAuthorityData: []byte("new-ca-data"),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &cpv1alpha1.ClusterProfile{
				Status: cpv1alpha1.ClusterProfileStatus{
					AccessProviders: tt.existingProviders,
				},
			}

			r := &clusterProfileReconciler{}
			r.setAccessProvider(cp, tt.newProvider)

			assert.Equal(t, tt.expectedCount, len(cp.Status.AccessProviders), "AccessProviders count should match")

			// Find the open-cluster-management provider
			var foundProvider *cpv1alpha1.AccessProvider
			for i := range cp.Status.AccessProviders {
				if cp.Status.AccessProviders[i].Name == "open-cluster-management" {
					foundProvider = &cp.Status.AccessProviders[i]
					break
				}
			}

			assert.NotNil(t, foundProvider, "open-cluster-management provider should exist")
			assert.Equal(t, tt.expectedProvider.Name, foundProvider.Name)
			assert.Equal(t, tt.expectedProvider.Cluster.Server, foundProvider.Cluster.Server)
			assert.Equal(t, tt.expectedProvider.Cluster.CertificateAuthorityData, foundProvider.Cluster.CertificateAuthorityData)
			assert.Equal(t, len(tt.expectedProvider.Cluster.Extensions), len(foundProvider.Cluster.Extensions))
		})
	}
}
