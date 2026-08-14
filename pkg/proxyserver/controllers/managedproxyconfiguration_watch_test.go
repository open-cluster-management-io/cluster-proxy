package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
)

func TestManagedProxyConfigurationOwnerHandlerEnqueuesNonControllerOwnerOnSecretUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := proxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{proxyv1alpha1.GroupVersion})
	mapper.Add(proxyv1alpha1.GroupVersion.WithKind("ManagedProxyConfiguration"), meta.RESTScopeRoot)

	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "proxy-system",
		Name:      "proxy-server-ca",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: proxyv1alpha1.GroupVersion.String(),
			Kind:       "ManagedProxyConfiguration",
			Name:       "cluster-proxy",
		}},
	}}
	updatedSecret := secret.DeepCopy()
	updatedSecret.ResourceVersion = "2"
	managedProxyConfigurationOwnerHandler(scheme, mapper).Update(
		context.Background(),
		event.UpdateEvent{ObjectOld: secret, ObjectNew: updatedSecret},
		queue,
	)

	if queue.Len() != 1 {
		t.Fatalf("expected one request, got %d", queue.Len())
	}
	request, shutdown := queue.Get()
	if shutdown {
		t.Fatal("workqueue unexpectedly shut down")
	}
	defer queue.Done(request)
	assert.Equal(t, reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster-proxy"}}, request)
}
