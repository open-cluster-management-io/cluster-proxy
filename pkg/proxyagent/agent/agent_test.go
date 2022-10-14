package agent

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/util"
)

func TestFilterMPSR(t *testing.T) {
	testcases := []struct {
		name      string
		resolvers []proxyv1alpha1.ManagedProxyServiceResolver
		mcsMap    map[string]clusterv1beta1.ManagedClusterSet
		expected  []serviceToExpose
	}{
		{
			name: "filter out the resolver with deletion timestamp",
			resolvers: []proxyv1alpha1.ManagedProxyServiceResolver{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "resolver-1",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-1",
								Namespace: "ns-1",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-2", // this one expected to exist
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-2",
								Namespace: "ns-2",
							},
						},
					},
				},
			},
			mcsMap: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
			expected: []serviceToExpose{
				{
					Host:         util.GenerateServiceURL("cluster1", "ns-2", "service-2"),
					ExternalName: "service-2.ns-2",
				},
			},
		},
		{
			name: "filter out the resolver match other managed cluster set",
			resolvers: []proxyv1alpha1.ManagedProxyServiceResolver{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-1",
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-1",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-1",
								Namespace: "ns-1",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "resolver-2",
					},
					Spec: proxyv1alpha1.ManagedProxyServiceResolverSpec{
						ManagedClusterSelector: proxyv1alpha1.ManagedClusterSelector{
							Type: proxyv1alpha1.ManagedClusterSelectorTypeClusterSet,
							ManagedClusterSet: &proxyv1alpha1.ManagedClusterSet{
								Name: "set-2",
							},
						},
						ServiceSelector: proxyv1alpha1.ServiceSelector{
							Type: proxyv1alpha1.ServiceSelectorTypeServiceRef,
							ServiceRef: &proxyv1alpha1.ServiceRef{
								Name:      "service-2",
								Namespace: "ns-2",
							},
						},
					},
				},
			},
			mcsMap: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
			expected: []serviceToExpose{
				{
					Host:         util.GenerateServiceURL("cluster1", "ns-1", "service-1"),
					ExternalName: "service-1.ns-1",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual := managedProxyServiceResolverToFilterServiceToExpose(testcase.resolvers, testcase.mcsMap, "cluster1")
			if len(actual) != len(testcase.expected) {
				t.Errorf("%s, expected %d resolvers, but got %d", testcase.name, len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("%s, expected %v, but got %v", testcase.name, testcase.expected, actual)
			}
		})
	}
}

func TestFilterMCS(t *testing.T) {
	testcases := []struct {
		name          string
		clusterlabels map[string]string
		clusters      []clusterv1beta1.ManagedClusterSet
		expected      map[string]clusterv1beta1.ManagedClusterSet
	}{
		{
			name: "filter out the cluster with deletion timestamp",
			clusterlabels: map[string]string{
				clusterv1beta1.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta1.ManagedClusterSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "set-1",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
				},
			},
			expected: map[string]clusterv1beta1.ManagedClusterSet{},
		},
		{
			name: "filter out the cluster without the current cluster label",
			clusterlabels: map[string]string{
				clusterv1beta1.ClusterSetLabel: "set-1",
			},
			clusters: []clusterv1beta1.ManagedClusterSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-2",
					},
				},
			},
			expected: map[string]clusterv1beta1.ManagedClusterSet{
				"set-1": {
					ObjectMeta: metav1.ObjectMeta{
						Name: "set-1",
					},
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual, err := managedClusterSetsToFilteredMap(testcase.clusters, testcase.clusterlabels)
			if err != nil {
				t.Errorf("expected no error, but got %v", err)
			}
			if len(actual) != len(testcase.expected) {
				t.Errorf("expected %d clusters, but got %d", len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("expected %v, but got %v", testcase.expected, actual)
			}
		})
	}
}

func TestRemoveDupAndSortservicesToExpose(t *testing.T) {
	testcases := []struct {
		name     string
		services []serviceToExpose
		expected []serviceToExpose
	}{
		{
			name: "remove duplicate and sort other services",
			services: []serviceToExpose{
				{
					Host: "service-3",
				},
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-1",
				},
			},
			expected: []serviceToExpose{
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-3",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual := removeDupAndSortServices(testcase.services)
			if len(actual) != len(testcase.expected) {
				t.Errorf("expected %d services, but got %d", len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("expected %v, but got %v", testcase.expected, actual)
			}
		})
	}
}
