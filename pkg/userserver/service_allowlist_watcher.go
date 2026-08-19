package userserver

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// startServiceAllowlistWatcher creates a namespace-scoped ConfigMap informer
// that keeps the returned ServiceAllowlist up to date whenever the named
// ConfigMap changes. It blocks until the informer cache is synced, then
// returns with the watcher running in the background until ctx is cancelled.
//
// On Add/Update: the YAML under each key is parsed and combined into a single
// allowlist that is atomically replaced. Any key name is accepted. If any key's
// YAML is invalid the last-known-good list is preserved and an error is logged.
//
// On Delete: the allowlist is set to empty (deny-all).
func startServiceAllowlistWatcher(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
	configMapName string,
) (*ServiceAllowlist, error) {
	allowlist := &ServiceAllowlist{}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		client,
		10*time.Minute,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", configMapName).String()
		}),
	)

	cmInformer := informerFactory.Core().V1().ConfigMaps().Informer()

	// updateFromConfigMap parses the ConfigMap and updates the allowlist.
	// On parse error the existing allowlist is preserved.
	updateFromConfigMap := func(cm *corev1.ConfigMap) {
		services, err := parseAllExposedServices(cm.Data)
		if err != nil {
			klog.Errorf("service allowlist: failed to parse ConfigMap %s/%s, keeping last-known-good list: %v",
				namespace, configMapName, err)
			return
		}
		allowlist.update(services)
		klog.V(2).Infof("service allowlist: loaded %d entries from ConfigMap %s/%s",
			len(services), namespace, configMapName)
	}

	if _, err := cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return
			}
			updateFromConfigMap(cm)
		},
		UpdateFunc: func(_, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok {
				return
			}
			updateFromConfigMap(cm)
		},
		DeleteFunc: func(_ interface{}) {
			allowlist.update(nil)
			klog.V(2).Infof("service allowlist: ConfigMap %s/%s deleted, all service proxy requests will be denied",
				namespace, configMapName)
		},
	}); err != nil {
		return nil, err
	}

	informerFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), cmInformer.HasSynced) {
		return nil, ctx.Err()
	}

	// The informer's Add event fires during the list phase (before
	// WaitForCacheSync returns), so by the time we reach here the allowlist
	// already reflects the current ConfigMap state. Log a warning if the
	// ConfigMap was not found so operators know the deny-all default is active.
	lister := informerFactory.Core().V1().ConfigMaps().Lister()
	if _, err := lister.ConfigMaps(namespace).Get(configMapName); errors.IsNotFound(err) {
		klog.Warningf("service allowlist: ConfigMap %s/%s not found at startup — all service proxy requests will be denied until it is created",
			namespace, configMapName)
	}

	return allowlist, nil
}
