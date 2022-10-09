package eventhandler

import (
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

var _ handler.EventHandler = &ClusterManagementAddonHandler{}

type ClusterManagementAddonHandler struct {
}

func (c ClusterManagementAddonHandler) Create(event event.CreateEvent, limitingInterface workqueue.RateLimitingInterface) {
	c.process(event.Object, limitingInterface)
}

func (c ClusterManagementAddonHandler) Update(event event.UpdateEvent, limitingInterface workqueue.RateLimitingInterface) {
	c.process(event.ObjectNew, limitingInterface)
}

func (c ClusterManagementAddonHandler) Delete(event event.DeleteEvent, limitingInterface workqueue.RateLimitingInterface) {
	c.process(event.Object, limitingInterface)
}

func (c ClusterManagementAddonHandler) Generic(event event.GenericEvent, limitingInterface workqueue.RateLimitingInterface) {
	c.process(event.Object, limitingInterface)
}

func (c ClusterManagementAddonHandler) process(obj runtime.Object, limitingInterface workqueue.RateLimitingInterface) {
	a := obj.(*addonv1alpha1.ClusterManagementAddOn)
	if a.Name == common.AddonName {
		req := reconcile.Request{}
		req.Name = a.Spec.AddOnConfiguration.CRName
		limitingInterface.Add(req)
	}
}
