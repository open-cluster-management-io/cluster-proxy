package serviceproxy

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"open-cluster-management.io/sdk-go/pkg/basecontroller/factory"
)

const (
	oidcCAConfigMapKey         = "ca.crt"
	oidcCAInformerResyncPeriod = 10 * time.Minute
)

type oidcCAConfigMapController struct {
	namespace     string
	name          string
	lister        listerscorev1.ConfigMapLister
	authenticator *oidcAuthenticator
}

// startOIDCCAConfigMapController starts a single-key controller for the OIDC
// CA ConfigMap. The initial reconciliation runs after the informer cache has
// synced, so discovery starts before the service proxy begins serving when the
// ConfigMap already exists.
func startOIDCCAConfigMapController(
	ctx context.Context,
	client kubernetes.Interface,
	namespace string,
	name string,
	authenticator *oidcAuthenticator,
) error {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		client,
		oidcCAInformerResyncPeriod,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
		}),
	)

	configMapInformer := informerFactory.Core().V1().ConfigMaps()
	reconciler := &oidcCAConfigMapController{
		namespace:     namespace,
		name:          name,
		lister:        configMapInformer.Lister(),
		authenticator: authenticator,
	}
	controller := factory.New().
		WithInformersQueueKeysFunc(factory.DefaultQueueKeysFunc, configMapInformer.Informer()).
		WithSync(reconciler.sync).
		ToController("oidc-ca-configmap-controller")

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), configMapInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync OIDC CA ConfigMap informer: %w", ctx.Err())
	}
	if err := reconciler.reconcile(ctx); err != nil {
		return err
	}

	go controller.Run(ctx, 1)
	return nil
}

func (c *oidcCAConfigMapController) sync(
	ctx context.Context,
	_ factory.SyncContext,
	_ string,
) error {
	return c.reconcile(ctx)
}

func (c *oidcCAConfigMapController) reconcile(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	configMap, err := c.lister.ConfigMaps(c.namespace).Get(c.name)
	if apierrors.IsNotFound(err) {
		unavailableError := fmt.Errorf("OIDC CA ConfigMap %s/%s is not available", c.namespace, c.name)
		if c.authenticator.markUnavailable("configmap-missing", unavailableError) {
			logger.Info("OIDC authenticator is waiting for its CA ConfigMap",
				"namespace", c.namespace,
				"name", c.name,
			)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get OIDC CA ConfigMap %s/%s from informer cache: %w", c.namespace, c.name, err)
	}

	caBundle, err := oidcCABundle(configMap)
	if err != nil {
		configurationKey := caConfigurationKey("configmap-invalid", []byte(err.Error()))
		if c.authenticator.markUnavailable(configurationKey, err) {
			logger.Error(err, "OIDC authenticator CA configuration is invalid",
				"namespace", c.namespace,
				"name", c.name,
			)
		}
		return nil
	}

	configurationKey := caConfigurationKey("configmap", caBundle)
	changed, err := c.authenticator.reconcileCABundle(configurationKey, caBundle)
	if err != nil {
		logger.Error(err, "failed to reconcile OIDC authenticator",
			"namespace", c.namespace,
			"name", c.name,
		)
		return nil
	}

	if changed {
		logger.Info("OIDC CA configuration reconciled",
			"namespace", c.namespace,
			"name", c.name,
		)
	}
	return nil
}

func oidcCABundle(configMap *corev1.ConfigMap) ([]byte, error) {
	caBundle, ok := configMap.BinaryData[oidcCAConfigMapKey]
	if data, dataOK := configMap.Data[oidcCAConfigMapKey]; dataOK {
		caBundle, ok = []byte(data), true
	}
	if !ok {
		return nil, fmt.Errorf("OIDC CA ConfigMap %s/%s does not contain %s",
			configMap.Namespace, configMap.Name, oidcCAConfigMapKey)
	}
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("OIDC CA ConfigMap %s/%s has an empty %s entry",
			configMap.Namespace, configMap.Name, oidcCAConfigMapKey)
	}
	return caBundle, nil
}
