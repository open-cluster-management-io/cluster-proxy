package config

import (
	"testing"

	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
)

func TestGetParsedAgentImage(t *testing.T) {
	testcases := []struct {
		agentImageName string
		expectErr      bool
		registry       string
		image          string
		tag            string
	}{
		{
			// no registry
			// no tag
			"open-cluster-management.io/cluster-proxy-agent",
			false,
			"open-cluster-management.io",
			"cluster-proxy-agent",
			"latest",
		},
		{
			// no tag
			"quay.io/open-cluster-management.io/cluster-proxy-agent",
			false,
			"quay.io/open-cluster-management.io",
			"cluster-proxy-agent",
			"latest",
		},
		{
			"quay.io/open-cluster-management.io/cluster-proxy-agent:v0.1.0",
			false,
			"quay.io/open-cluster-management.io",
			"cluster-proxy-agent",
			"v0.1.0",
		},
		{
			// registry with port
			"quay.io:443/open-cluster-management.io/cluster-proxy-agent:v0.1.0",
			false,
			"quay.io:443/open-cluster-management.io",
			"cluster-proxy-agent",
			"v0.1.0",
		},
		{
			// registry with port
			// no tag
			"quay.io:443/open-cluster-management.io/cluster-proxy-agent",
			false,
			"quay.io:443/open-cluster-management.io",
			"cluster-proxy-agent",
			"latest",
		},
		{
			// empty image name
			"",
			false,
			"quay.io/open-cluster-management.io",
			"cluster-proxy-agent",
			"latest",
		},
	}

	for _, c := range testcases {
		AgentImageName = c.agentImageName
		r, i, tag, err := GetParsedAgentImage("quay.io/open-cluster-management.io/cluster-proxy-agent")
		if err != nil {
			if c.expectErr {
				continue
			}
			t.Errorf("GetParsedAgentImage() error: %v", err)
		}

		if r != c.registry || i != c.image || tag != c.tag {
			t.Errorf("expect %s, %s, %s, but get %s, %s, %s", c.registry, c.image, c.tag, r, i, tag)
		}
	}
}

func TestFindDefaultManagedProxyConfigurationName(t *testing.T) {
	cases := []struct {
		name               string
		cma                *addonv1alpha1.ClusterManagementAddOn
		expectedConfigName string
	}{
		{
			name: "no config",
			cma:  &addonv1alpha1.ClusterManagementAddOn{},
		},
		{
			name: "non proxy.open-cluster-management.io",
			cma: &addonv1alpha1.ClusterManagementAddOn{
				Spec: addonv1alpha1.ClusterManagementAddOnSpec{
					SupportedConfigs: []addonv1alpha1.ConfigMeta{
						{
							ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
								Group:    "test.io",
								Resource: "tests",
							},
						},
					},
				},
			},
		},
		{
			name: "non managed proxy config",
			cma: &addonv1alpha1.ClusterManagementAddOn{
				Spec: addonv1alpha1.ClusterManagementAddOnSpec{
					SupportedConfigs: []addonv1alpha1.ConfigMeta{
						{
							ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
								Group:    "proxy.open-cluster-management.io",
								Resource: "tests",
							},
						},
					},
				},
			},
		},
		{
			name: "no defautl config",
			cma: &addonv1alpha1.ClusterManagementAddOn{
				Spec: addonv1alpha1.ClusterManagementAddOnSpec{
					SupportedConfigs: []addonv1alpha1.ConfigMeta{
						{
							ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
								Group:    "proxy.open-cluster-management.io",
								Resource: "managedproxyconfigurations",
							},
						},
					},
				},
			},
		},
		{
			name: "has managed proxy config",
			cma: &addonv1alpha1.ClusterManagementAddOn{
				Spec: addonv1alpha1.ClusterManagementAddOnSpec{
					SupportedConfigs: []addonv1alpha1.ConfigMeta{
						{
							ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
								Group:    "proxy.open-cluster-management.io",
								Resource: "managedproxyconfigurations",
							},
							DefaultConfig: &addonv1alpha1.ConfigReferent{
								Name: "cluster-proxy",
							},
						},
					},
				},
			},
			expectedConfigName: "cluster-proxy",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual := FindDefaultManagedProxyConfigurationName(c.cma)
			if actual != c.expectedConfigName {
				t.Errorf("expected %q, but %q", c.expectedConfigName, actual)
			}
		})
	}

}
