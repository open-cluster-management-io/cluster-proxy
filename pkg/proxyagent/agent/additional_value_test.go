package agent

import (
	"fmt"
	"testing"

	"github.com/stolostron/cluster-lifecycle-api/constants"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

func TestGetNodeSelector(t *testing.T) {
	testcases := []struct {
		name string
		// input}
		cluster *clusterv1.ManagedCluster
		err     error
		expect  map[string]string
	}{
		{
			name:    "managedCluster is not local-cluster, expect empty nodeSelector",
			cluster: newCluster("cluster", false),
			expect:  map[string]string{},
		},
		{
			name: "managedCluster is local-cluster, but no annotation, expect empty nodeSelector",
			cluster: newClusterWithLabelsAnnotations("cluster", false,
				map[string]string{constants.SelfManagedClusterLabelKey: "true"}, nil),
			expect: map[string]string{},
		},
		{
			name: "managedCluster is local-cluster with incorrect annotation, expect err",
			cluster: newClusterWithLabelsAnnotations("cluster", false,
				map[string]string{constants.SelfManagedClusterLabelKey: "true"},
				map[string]string{
					annotationNodeSelector: "kubernetes.io/os=linux",
				}),
			err: fmt.Errorf("incorrect annotation"),
		},
		{
			name: "managedCluster is local-cluster with correct annotation, expect nodeSelector",
			cluster: newClusterWithLabelsAnnotations("local-cluster", false,
				map[string]string{constants.SelfManagedClusterLabelKey: "true"},
				map[string]string{
					annotationNodeSelector: `{"kubernetes.io/os":"linux"}`,
				}),
			expect: map[string]string{
				"kubernetes.io/os": "linux",
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual, err := getNodeSelector(testcase.cluster)
			if err != nil && testcase.err == nil {
				t.Errorf("expected no error, but got %v", err)
			}
			// compare actual and expected map
			for k, v := range testcase.expect {
				if actual[k] != v {
					t.Errorf("expected %v, but got %v", testcase.expect, actual)
				}
			}
			for k, v := range actual {
				if testcase.expect[k] != v {
					t.Errorf("expected %v, but got %v", testcase.expect, actual)
				}
			}
		})
	}
}

func newClusterWithLabelsAnnotations(name string, accepted bool, labels map[string]string, annotations map[string]string) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: clusterv1.ManagedClusterSpec{
			HubAcceptsClient: accepted,
		},
	}
}
