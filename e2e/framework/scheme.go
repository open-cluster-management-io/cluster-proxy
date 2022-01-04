package framework

import (
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	proxyv1alpha1.AddToScheme(scheme)
	clusterv1.AddToScheme(scheme)
}
