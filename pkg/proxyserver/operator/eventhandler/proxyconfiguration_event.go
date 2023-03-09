package eventhandler

import (
	"context"

	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ handler.EventHandler = &ManagedProxyConfigurationHandler{}

type ManagedProxyConfigurationHandler struct {
	client.Client
}

func (m ManagedProxyConfigurationHandler) Create(event event.CreateEvent, limitingInterface workqueue.RateLimitingInterface) {
	m.findRelatedAddon(event.Object, limitingInterface)
}

func (m ManagedProxyConfigurationHandler) Update(event event.UpdateEvent, limitingInterface workqueue.RateLimitingInterface) {
	m.findRelatedAddon(event.ObjectNew, limitingInterface)
}

func (m ManagedProxyConfigurationHandler) Delete(event event.DeleteEvent, limitingInterface workqueue.RateLimitingInterface) {
	m.findRelatedAddon(event.Object, limitingInterface)
}

func (m ManagedProxyConfigurationHandler) Generic(event event.GenericEvent, limitingInterface workqueue.RateLimitingInterface) {
	m.findRelatedAddon(event.Object, limitingInterface)
}

func (m ManagedProxyConfigurationHandler) findRelatedAddon(obj runtime.Object, limitingInterface workqueue.RateLimitingInterface) {
	cfg := obj.(*proxyv1alpha1.ManagedProxyConfiguration)
	findRelatedAddon(m.Client, cfg.Name, limitingInterface)
}

func findRelatedAddon(c client.Client, cfgName string, limitingInterface workqueue.RateLimitingInterface) {
	list := &addonv1alpha1.ClusterManagementAddOnList{}
	err := c.List(context.TODO(), list)
	if err != nil {
		return
	}
	for _, addon := range list.Items {
		if addon.Name == common.AddonName {
			// There is only one clustermanagemetnaddon for "cluster-proxy", and the config file of cluster-proxy must be a managedproxyconfiguration. So we don't need to check whether "cluster-proxy" addon match the cfg.
			req := reconcile.Request{}
			req.Namespace = addon.Namespace
			req.Name = addon.Name
			limitingInterface.Add(req)
		}
	}
}
