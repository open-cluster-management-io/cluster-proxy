package eventhandler

import (
	"context"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ handler.EventHandler = &ProxyServiceResolverHandler{}

type ProxyServiceResolverHandler struct{}

func (m ProxyServiceResolverHandler) Create(_ context.Context, event event.CreateEvent, queue workqueue.RateLimitingInterface) {
	req := reconcile.Request{}
	req.Name = event.Object.GetName()
	queue.Add(req)
}

func (m ProxyServiceResolverHandler) Update(_ context.Context, event event.UpdateEvent, queue workqueue.RateLimitingInterface) {
	req := reconcile.Request{}
	req.Name = event.ObjectNew.GetName()
	queue.Add(req)
}

func (m ProxyServiceResolverHandler) Delete(_ context.Context, event event.DeleteEvent, queue workqueue.RateLimitingInterface) {
	req := reconcile.Request{}
	req.Name = event.Object.GetName()
	queue.Add(req)
}

func (m ProxyServiceResolverHandler) Generic(_ context.Context, event event.GenericEvent, queue workqueue.RateLimitingInterface) {
	req := reconcile.Request{}
	req.Name = event.Object.GetName()
	queue.Add(req)
}
