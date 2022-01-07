package framework

import (
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	proxyv1alpha1.AddToScheme(scheme)
	clusterv1.AddToScheme(scheme)
	addonv1alpha1.AddToScheme(scheme)
	k8sscheme.AddToScheme(scheme)
}
