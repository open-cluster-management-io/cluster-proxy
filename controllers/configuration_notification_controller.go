package controllers

import (
	"context"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func RegisterConfigurationNotifyReconciler(mgr manager.Manager) error {
	r := &ConfigurationNotifyReconciler{
		Client: mgr.GetClient(),
	}
	return r.SetupWithManager(mgr)
}

var _ reconcile.Reconciler = &ConfigurationNotifyReconciler{}

type ConfigurationNotifyReconciler struct {
	Client client.Client
}

func (c *ConfigurationNotifyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.ManagedProxyConfiguration{}).
		Complete(c)
}

func (c *ConfigurationNotifyReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {

	cfg := &proxyv1alpha1.ManagedProxyConfiguration{}
	if err := c.Client.Get(ctx, request.NamespacedName, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "failed getting proxy configuration")
	}

	addonList := &addonv1alpha1.ManagedClusterAddOnList{}
	if err := c.Client.List(ctx, addonList); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed listing managed cluster addon")
	}

	processing := make([]*addonv1alpha1.ManagedClusterAddOn, 0)
	for _, addon := range addonList.Items {
		const addonCRDName = "managedproxyconfigurations.proxy.open-cluster-management.io"
		if addon.Status.AddOnConfiguration.CRDName != addonCRDName {
			continue
		}
		if addon.Status.AddOnConfiguration.CRName != cfg.Name {
			continue
		}
		addon := addon
		processing = append(processing, &addon)
	}

	for _, addon := range processing {
		if addon.Status.AddOnConfiguration.LastObservedGeneration != cfg.Generation {
			addon.Status.AddOnConfiguration.LastObservedGeneration = cfg.Generation
			if err := c.Client.Status().Update(ctx, addon); err != nil {
				return reconcile.Result{}, errors.Wrapf(err, "failed refreshing generation")
			}
		}
	}

	return reconcile.Result{}, nil
}
