package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/proxyagent/agent"
	cpv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ reconcile.Reconciler = &clusterProfileReconciler{}

var logger = ctrl.Log.WithName("ClusterProfileReconciler")

const OCMAccessProviderName = "open-cluster-management"

type clusterProfileReconciler struct {
	client.Client
}

type execClusterConfig struct {
	ClusterName string `json:"clusterName"`
}

func SetupClusterProfileReconciler(mgr manager.Manager) error {
	reconciler := &clusterProfileReconciler{
		Client: mgr.GetClient(),
	}
	return reconciler.SetupWithManager(mgr)
}

func (r *clusterProfileReconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cpv1alpha1.ClusterProfile{}).
		Complete(r)
}

func (r *clusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger.Info("Start reconcile", "namespace", req.Namespace, "name", req.Name)
	cp := &cpv1alpha1.ClusterProfile{}
	err := r.Get(ctx, req.NamespacedName, cp)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// get the managed proxy configuration
	config := &proxyv1alpha1.ManagedProxyConfiguration{}
	err = r.Get(ctx, types.NamespacedName{
		Name: agent.ManagedClusterConfigurationName,
	}, config)
	switch {
	case errors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// prepare ca for access provider
	caSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: config.Spec.ProxyServer.Namespace, Name: constant.UserServerSecretName}, caSecret)
	switch {
	case errors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// retrieve the cluster-proxy-user-server certificate as ca because it's allowed self-signed leaf trust.
	caData, ok := caSecret.Data["tls.crt"]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("tls.crt not found in secret")
	}

	// ensure cluster name is added to exec extension for per cluster access.
	execExtension := &execClusterConfig{ClusterName: cp.Name}
	rawExecExtension, err := json.Marshal(execExtension)
	if err != nil {
		return ctrl.Result{}, err
	}

	// prepare the access provider to access spoke via user-server.
	ocmAccessProvider := cpv1alpha1.AccessProvider{
		Name: OCMAccessProviderName,
		Cluster: clientcmdapiv1.Cluster{
			Server: fmt.Sprintf("https://%s.%s:9092/%s",
				constant.UserServerServiceName,
				config.Spec.ProxyServer.Namespace, cp.Name),
			CertificateAuthorityData: caData,
			Extensions: []clientcmdapiv1.NamedExtension{
				{
					Name: "client.authentication.k8s.io/exec",
					Extension: runtime.RawExtension{
						Raw: rawExecExtension,
					},
				},
			},
		},
	}

	// set access provider for clusterprofile
	r.setAccessProvider(cp, ocmAccessProvider)
	// Initialize conditions to empty array if nil to avoid validation errors
	if cp.Status.Conditions == nil {
		cp.Status.Conditions = []metav1.Condition{}
	}
	if err = r.Status().Update(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconcile completed", "namespace", req.Namespace, "name", req.Name)
	return ctrl.Result{}, nil
}

func (r *clusterProfileReconciler) setAccessProvider(cp *cpv1alpha1.ClusterProfile, accessProvider cpv1alpha1.AccessProvider) {
	if cp.Status.AccessProviders == nil {
		cp.Status.AccessProviders = []cpv1alpha1.AccessProvider{}
	}

	for i := range cp.Status.AccessProviders {
		if cp.Status.AccessProviders[i].Name == OCMAccessProviderName {
			cp.Status.AccessProviders[i] = accessProvider
			return
		}
	}

	cp.Status.AccessProviders = append(cp.Status.AccessProviders, accessProvider)
}
