package client

import (
	"context"
	"fmt"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	clusterv1client "open-cluster-management.io/api/client/cluster/clientset/versioned"
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	"open-cluster-management.io/cluster-proxy/pkg/generated/clientset/versioned"
	"open-cluster-management.io/cluster-proxy/pkg/util"
)

func GetProxyHost(ctx context.Context, kubeconfig *rest.Config, clusterName string, namespace string, serviceName string) (string, error) {
	client := versioned.NewForConfigOrDie(kubeconfig)
	mpsrList, err := client.ProxyV1alpha1().ManagedProxyServiceResolvers().List(ctx, v1.ListOptions{})
	if err != nil {
		return "", err
	}

	// Get labels of the managedCluster
	clusterClient, err := clusterv1client.NewForConfig(kubeconfig)
	if err != nil {
		return "", err
	}
	managedCluster, err := clusterClient.ClusterV1().ManagedClusters().Get(ctx, clusterName, v1.GetOptions{})
	if err != nil {
		return "", err
	}

	// Return when namespace, serviceName and labels of the managedCluster are all matched
	for _, sr := range mpsrList.Items {
		if !util.IsServiceResolverLegal(&sr) {
			continue
		}

		set, err := clusterClient.ClusterV1beta2().ManagedClusterSets().Get(ctx, sr.Spec.ManagedClusterSelector.ManagedClusterSet.Name, v1.GetOptions{})
		if err != nil {
			return "", err
		}
		selector, err := clusterv1beta2.BuildClusterSelector(set)
		if err != nil {
			return "", err
		}
		if !selector.Matches(labels.Set(managedCluster.Labels)) {
			continue
		}

		if sr.Spec.ServiceSelector.ServiceRef.Namespace != namespace || sr.Spec.ServiceSelector.ServiceRef.Name != serviceName {
			continue
		}

		return util.GenerateServiceURL(clusterName, namespace, serviceName), nil
	}

	return "", fmt.Errorf("Not found any suitable ManagedProxyServiceResolver for (cluster:%s, namespace: %s, service: %s)", clusterName, namespace, serviceName)
}
