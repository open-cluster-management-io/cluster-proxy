package userserver

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	addoninformers "open-cluster-management.io/api/client/addon/informers/externalversions"

	"open-cluster-management.io/cluster-proxy/pkg/constant"
)

const managedClusterAddonResyncPeriod = 30 * time.Minute

// clusterTransportPool owns the reusable transports for managed clusters where
// the cluster-proxy add-on is installed. Membership changes and transport
// creation use the same exclusive lock so an informer delete cannot race with
// creation of a new cached transport. Cache hits use the shared side of the lock.
type clusterTransportPool struct {
	mu           sync.RWMutex
	allowed      map[string]struct{}
	transports   map[string]*http.Transport
	newTransport func(clusterName string) *http.Transport
}

func newClusterTransportPool(newTransport func(clusterName string) *http.Transport) *clusterTransportPool {
	return &clusterTransportPool{
		allowed:      map[string]struct{}{},
		transports:   map[string]*http.Transport{},
		newTransport: newTransport,
	}
}

func (p *clusterTransportPool) allow(clusterName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.allowed[clusterName] = struct{}{}
}

func (p *clusterTransportPool) getOrCreate(clusterName string) (*http.Transport, bool) {
	p.mu.RLock()
	_, allowed := p.allowed[clusterName]
	transport, cached := p.transports[clusterName]
	p.mu.RUnlock()

	if !allowed {
		return nil, false
	}
	if cached {
		return transport, true
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.allowed[clusterName]; !ok {
		return nil, false
	}
	if transport, ok := p.transports[clusterName]; ok {
		return transport, true
	}

	transport = p.newTransport(clusterName)
	p.transports[clusterName] = transport
	return transport, true
}

func (p *clusterTransportPool) isAllowed(clusterName string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, ok := p.allowed[clusterName]
	return ok
}

func (p *clusterTransportPool) remove(clusterName string) {
	p.mu.Lock()
	delete(p.allowed, clusterName)
	transport := p.transports[clusterName]
	delete(p.transports, clusterName)
	p.mu.Unlock()

	if transport != nil {
		transport.CloseIdleConnections()
	}
}

func (p *clusterTransportPool) closeAll() {
	p.mu.Lock()
	clear(p.allowed)
	transports := p.transports
	p.transports = map[string]*http.Transport{}
	p.mu.Unlock()

	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

// startManagedClusterAddonWatcher keeps the transport pool membership aligned
// with ManagedClusterAddOn/cluster-proxy resources and waits for the initial
// list before returning. Until that list is complete, the user-server must not
// accept requests because the pool intentionally defaults to denying access.
func startManagedClusterAddonWatcher(
	ctx context.Context,
	client addonclient.Interface,
	pool *clusterTransportPool,
) error {
	informerFactory := addoninformers.NewSharedInformerFactoryWithOptions(
		client,
		managedClusterAddonResyncPeriod,
		addoninformers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", constant.AddonName).String()
		}),
	)
	informer := informerFactory.Addon().V1beta1().ManagedClusterAddOns().Informer()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if addon, ok := clusterProxyAddon(obj); ok {
				pool.allow(addon.Namespace)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if addon, ok := clusterProxyAddon(newObj); ok {
				pool.allow(addon.Namespace)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if addon, ok := deletedClusterProxyAddon(obj); ok {
				pool.remove(addon.Namespace)
			}
		},
	}); err != nil {
		return fmt.Errorf("register managed cluster add-on event handler: %w", err)
	}

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("managed cluster add-on informer cache did not sync")
	}

	return nil
}

func clusterProxyAddon(obj interface{}) (*addonv1beta1.ManagedClusterAddOn, bool) {
	addon, ok := obj.(*addonv1beta1.ManagedClusterAddOn)
	return addon, ok && addon.Name == constant.AddonName && addon.Namespace != ""
}

func deletedClusterProxyAddon(obj interface{}) (*addonv1beta1.ManagedClusterAddOn, bool) {
	if addon, ok := clusterProxyAddon(obj); ok {
		return addon, true
	}

	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		klog.Errorf("transport pool: unexpected managed cluster add-on delete object %T", obj)
		return nil, false
	}
	addon, ok := clusterProxyAddon(tombstone.Obj)
	if !ok {
		klog.Errorf("transport pool: unexpected managed cluster add-on tombstone object %T", tombstone.Obj)
	}
	return addon, ok
}
