package features

import (
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

const (
	// owner: @morvencao
	// alpha: v0.1
	// ClusterProfileAccessProvider enables the ClusterProfile controller to manage cluster
	// access providers for the cluster-inventory-api integration.
	ClusterProfileAccessProvider featuregate.Feature = "ClusterProfileAccessProvider"
)

var (
	// FeatureGates is the mutable feature gate for cluster-proxy
	FeatureGates featuregate.MutableFeatureGate = featuregate.NewFeatureGate()
)

func init() {
	if err := FeatureGates.Add(DefaultClusterProxyFeatureGates); err != nil {
		klog.Fatalf("Unexpected error: %v", err)
	}
}

// DefaultClusterProxyFeatureGates defines the default feature gates for cluster-proxy
var DefaultClusterProxyFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	ClusterProfileAccessProvider: {Default: false, PreRelease: featuregate.Alpha},
}
