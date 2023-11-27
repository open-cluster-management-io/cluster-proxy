package eventhandler

import (
	"context"

	"k8s.io/client-go/util/workqueue"
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ handler.EventHandler = &ClustersetHandler{}

type ClustersetHandler struct {
	client.Client
}

func (m ClustersetHandler) Create(_ context.Context, event event.CreateEvent, limitingInterface workqueue.RateLimitingInterface) {
	clusterset := event.Object.(*clusterv1beta2.ManagedClusterSet)
	m.findClusterProxyAddon(clusterset, limitingInterface)
}

func (m ClustersetHandler) Update(_ context.Context, event event.UpdateEvent, limitingInterface workqueue.RateLimitingInterface) {
	clusterset := event.ObjectNew.(*clusterv1beta2.ManagedClusterSet)
	m.findClusterProxyAddon(clusterset, limitingInterface)
}

func (m ClustersetHandler) Delete(_ context.Context, event event.DeleteEvent, limitingInterface workqueue.RateLimitingInterface) {
	clusterset := event.Object.(*clusterv1beta2.ManagedClusterSet)
	m.findClusterProxyAddon(clusterset, limitingInterface)
}

func (m ClustersetHandler) Generic(_ context.Context, event event.GenericEvent, limitingInterface workqueue.RateLimitingInterface) {
	clusterset := event.Object.(*clusterv1beta2.ManagedClusterSet)
	m.findClusterProxyAddon(clusterset, limitingInterface)
}

// findClusterProxyAddon will triger clustermanagementaddon on all managed clusters to reconcile.
func (m *ClustersetHandler) findClusterProxyAddon(clusterset *clusterv1beta2.ManagedClusterSet, limitingInterface workqueue.RateLimitingInterface) {
	var err error
	// Check whether the clusterset is related with any managedproxyserviceresolver.
	mpsrList := &proxyv1alpha1.ManagedProxyServiceResolverList{}
	err = m.Client.List(context.TODO(), mpsrList, &client.ListOptions{})
	if err != nil {
		return
	}
	for _, mpsr := range mpsrList.Items {
		if !util.IsServiceResolverLegal(&mpsr) {
			continue
		}
		if mpsr.Spec.ManagedClusterSelector.ManagedClusterSet.Name == clusterset.Name {
			req := reconcile.Request{}
			req.Name = mpsr.Name
			limitingInterface.Add(req)
			break
		}
	}
}
