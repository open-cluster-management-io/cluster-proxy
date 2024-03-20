package controllers

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	clusterv1beta2 "open-cluster-management.io/api/cluster/v1beta2"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/eventhandler"
	"open-cluster-management.io/cluster-proxy/pkg/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ reconcile.Reconciler = &ServiceResolverReconciler{}

type ServiceResolverReconciler struct {
	client.Client
}

func RegisterServiceResolverReconciler(mgr ctrl.Manager) error {
	r := &ServiceResolverReconciler{
		Client: mgr.GetClient(),
	}
	return r.SetupWithManager(mgr)
}

func (c *ServiceResolverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.ManagedProxyServiceResolver{}).
		Watches(
			&proxyv1alpha1.ManagedProxyServiceResolver{},
			&eventhandler.ProxyServiceResolverHandler{},
		).
		Watches(
			&clusterv1beta2.ManagedClusterSet{},
			&eventhandler.ClustersetHandler{
				Client: mgr.GetClient(),
			},
		).
		Complete(c)
}

func (c *ServiceResolverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Info("Reconciling ServiceResolver", "name", req.Name)
	// refreshing service resolvers status
	return ctrl.Result{}, c.refreshManageProxyServiceResolversStatus()
}

func (c *ServiceResolverReconciler) refreshManageProxyServiceResolversStatus() error {
	// list ManagedProxyServiceResolvers
	resolvers := &proxyv1alpha1.ManagedProxyServiceResolverList{}
	if err := c.Client.List(context.TODO(), resolvers); err != nil {
		return err
	}

	var errs []error

	// for each resolver, get it's managedclusterset
	for i := range resolvers.Items {
		resolver := resolvers.Items[i]
		var expectingCondition metav1.Condition
		editing := resolver.DeepCopy()
		currentCondition := meta.FindStatusCondition(editing.Status.Conditions, proxyv1alpha1.ConditionTypeServiceResolverAvaliable)

		// Currently, managedclusterseletor only support clusterset type, and serviceselector only support serviceRef type.
		if !util.IsServiceResolverLegal(&resolver) {
			expectingCondition = metav1.Condition{
				Type:   proxyv1alpha1.ConditionTypeServiceResolverAvaliable,
				Status: metav1.ConditionFalse,
				Reason: "ManagedProxyServiceResolverNotLegal",
			}
		} else {
			// get managedclusterset
			managedClusterSet := &clusterv1beta2.ManagedClusterSet{}
			if err := c.Client.Get(context.TODO(), types.NamespacedName{
				Name: resolver.Spec.ManagedClusterSelector.ManagedClusterSet.Name,
			}, managedClusterSet); err != nil {
				if apierrors.IsNotFound(err) {
					expectingCondition = metav1.Condition{
						Type:   proxyv1alpha1.ConditionTypeServiceResolverAvaliable,
						Status: metav1.ConditionFalse,
						Reason: "ManagedClusterSetNotExisted",
					}
				} else {
					return err
				}
			} else {
				if !managedClusterSet.DeletionTimestamp.IsZero() {
					expectingCondition = metav1.Condition{
						Type:   proxyv1alpha1.ConditionTypeServiceResolverAvaliable,
						Status: metav1.ConditionFalse,
						Reason: "ManagedClusterSetDeleting",
					}
				} else {
					expectingCondition = metav1.Condition{
						Type:   proxyv1alpha1.ConditionTypeServiceResolverAvaliable,
						Status: metav1.ConditionTrue,
						Reason: "ManagedProxyServiceResolverAvaliable",
					}
				}
			}
		}

		// update status; now only consider one condition.
		if currentCondition != nil && currentCondition.Reason == expectingCondition.Reason {
			continue
		}

		meta.SetStatusCondition(&editing.Status.Conditions, expectingCondition)
		errs = append(errs, c.Client.Status().Update(context.TODO(), editing))
	}

	return utilerrors.NewAggregate(errs)
}
