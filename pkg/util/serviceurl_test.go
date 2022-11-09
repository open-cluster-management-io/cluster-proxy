package util

import "testing"

func TestGenerateServiceURL(t *testing.T) {
	testcases := []struct {
		cluster   string
		namespace string
		service   string
		expected  string
	}{
		{
			cluster:   "cluster1",
			namespace: "default",
			service:   "service1",
			expected:  "cluster-proxy-1f28e10e03e76d3df8306b102a1da1adc79e744dd27fe48eb",
		},
	}

	for _, tc := range testcases {
		actual := GenerateServiceURL(tc.cluster, tc.namespace, tc.service)
		if actual != tc.expected {
			t.Errorf("expected %s, got %s", tc.expected, actual)
		}
	}
}
