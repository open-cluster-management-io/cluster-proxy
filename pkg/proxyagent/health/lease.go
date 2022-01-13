package health

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"open-cluster-management.io/addon-framework/pkg/lease"
	"open-cluster-management.io/cluster-proxy/pkg/common"
)

func NewAddonHealthUpdater(hubClientCfg *rest.Config, clusterName string) (lease.LeaseUpdater, error) {
	hubClient, err := kubernetes.NewForConfig(hubClientCfg)
	if err != nil {
		return nil, err
	}
	return lease.NewLeaseUpdater(
		hubClient,
		common.AddonName,
		clusterName,
	), nil
}
