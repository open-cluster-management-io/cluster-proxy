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
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/proxyagent/agent"
	cpv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type clusterProfileReconciler struct {
	client.Client
	serviceGetter corev1client.ServicesGetter
}

type execClusterConfig struct {
	ClusterName string `json:"clusterName"`
}

func SetupClusterProfileReconciler(mgr manager.Manager, nativeClient kubernetes.Interface) error {
	reconciler := &clusterProfileReconciler{
		Client:        mgr.GetClient(),
		serviceGetter: nativeClient.CoreV1(),
	}
	return reconciler.SetupWithManager(mgr)
}

func (r *clusterProfileReconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cpv1alpha1.ClusterProfile{}).
		Complete(r)
}

func (r *clusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Info("Start reconcile", "name", req.Name)
	cp := &cpv1alpha1.ClusterProfile{}
	err := r.Get(ctx, req.NamespacedName, cp)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

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

	caSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: config.Spec.ProxyServer.Namespace, Name: constant.UserServerSecretName}, caSecret)
	switch {
	case errors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	caData, ok := caSecret.Data["ca.crt"]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("ca.crt not found in secret")
	}

	execExtension := &execClusterConfig{ClusterName: cp.Name}
	rawExecExtension, err := json.Marshal(execExtension)
	if err != nil {
		return ctrl.Result{}, err
	}

	ocmAccessProvider := cpv1alpha1.AccessProvider{
		Name: "open-cluster-management",
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

	r.setAccessProvider(cp, ocmAccessProvider)
	// Initialize conditions to empty array if nil to avoid validation errors
	if cp.Status.Conditions == nil {
		cp.Status.Conditions = []metav1.Condition{}
	}
	err = r.Status().Update(ctx, cp)
	return ctrl.Result{}, err
}

func (r *clusterProfileReconciler) setAccessProvider(cp *cpv1alpha1.ClusterProfile, accessProvider cpv1alpha1.AccessProvider) {
	if cp.Status.AccessProviders == nil {
		cp.Status.AccessProviders = []cpv1alpha1.AccessProvider{}
	}

	for i := range cp.Status.AccessProviders {
		if cp.Status.AccessProviders[i].Name == "open-cluster-management" {
			cp.Status.AccessProviders[i] = accessProvider
			return
		}
	}

	cp.Status.AccessProviders = append(cp.Status.AccessProviders, accessProvider)
}
