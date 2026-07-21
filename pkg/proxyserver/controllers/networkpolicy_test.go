package controllers

import (
	"context"
	"testing"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/assert"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

func newNetworkPolicyTestReconciler(t *testing.T, objs ...client.Object) *ManagedProxyConfigurationReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, clientgoscheme.AddToScheme(scheme))
	assert.NoError(t, proxyv1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return &ManagedProxyConfigurationReconciler{
		Client:        builder.Build(),
		EventRecorder: events.NewInMemoryRecorder("test", clock.RealClock{}),
	}
}

func TestEnsureProxyServerNetworkPolicy_CreateWhenEnabled(t *testing.T) {
	r := newNetworkPolicyTestReconciler(t)
	config := &proxyv1alpha1.ManagedProxyConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster-proxy",
			Generation: 1,
		},
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Namespace: "hub-ns",
			},
			NetworkPolicies: &proxyv1alpha1.NetworkPoliciesConfig{Enabled: true},
		},
	}

	modified, err := r.ensureProxyServerNetworkPolicy(config)
	assert.NoError(t, err)
	assert.True(t, modified)

	np := &networkingv1.NetworkPolicy{}
	err = r.Get(context.TODO(), types.NamespacedName{
		Namespace: "hub-ns",
		Name:      "cluster-proxy-proxy-server",
	}, np)
	assert.NoError(t, err)
	assert.Equal(t, "proxy-server", np.Spec.PodSelector.MatchLabels["proxy.open-cluster-management.io/component-name"])
}

func TestEnsureProxyServerNetworkPolicy_DeleteWhenDisabled(t *testing.T) {
	existing := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-proxy-proxy-server",
			Namespace: "hub-ns",
		},
	}
	r := newNetworkPolicyTestReconciler(t, existing)
	config := &proxyv1alpha1.ManagedProxyConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-proxy"},
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Namespace: "hub-ns",
			},
			NetworkPolicies: &proxyv1alpha1.NetworkPoliciesConfig{Enabled: false},
		},
	}

	modified, err := r.ensureProxyServerNetworkPolicy(config)
	assert.NoError(t, err)
	assert.True(t, modified)

	err = r.Get(context.TODO(), types.NamespacedName{
		Namespace: "hub-ns",
		Name:      "cluster-proxy-proxy-server",
	}, &networkingv1.NetworkPolicy{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestEnsureProxyServerNetworkPolicy_NoopWhenUnsetAndMissing(t *testing.T) {
	r := newNetworkPolicyTestReconciler(t)
	config := &proxyv1alpha1.ManagedProxyConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-proxy"},
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Namespace: "hub-ns",
			},
		},
	}

	modified, err := r.ensureProxyServerNetworkPolicy(config)
	assert.NoError(t, err)
	assert.False(t, modified)
}

func TestDeleteIfExists_DeletesPresentResource(t *testing.T) {
	existing := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-proxy-proxy-server",
			Namespace: "hub-ns",
		},
	}
	r := newNetworkPolicyTestReconciler(t, existing)
	deleted, err := r.deleteIfExists(existing.DeepCopy())
	assert.NoError(t, err)
	assert.True(t, deleted)
}

func TestDeleteIfExists_NoopWhenMissing(t *testing.T) {
	r := newNetworkPolicyTestReconciler(t)
	missing := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-proxy-proxy-server",
			Namespace: "hub-ns",
		},
	}
	deleted, err := r.deleteIfExists(missing)
	assert.NoError(t, err)
	assert.False(t, deleted)
}
