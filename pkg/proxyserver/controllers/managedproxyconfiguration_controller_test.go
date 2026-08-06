package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"open-cluster-management.io/cluster-proxy/pkg/common"
)

func TestRenderedResourceHashChangesWithRenderedContent(t *testing.T) {
	config := newTestConfig(3)
	baseline, err := renderedResourceHash(newProxyServerDeployment(config, "IfNotPresent", nil))
	if err != nil {
		t.Fatal(err)
	}

	changed := config.DeepCopy()
	changed.Spec.ProxyServer.Image = "example.invalid/cluster-proxy:new"
	updated, err := renderedResourceHash(newProxyServerDeployment(changed, "IfNotPresent", nil))
	if err != nil {
		t.Fatal(err)
	}

	assert.NotEqual(t, baseline, updated)
}

func TestRenderedResourceHashChangesWithRoleRules(t *testing.T) {
	config := newTestConfig(3)
	baselineRole := newProxyServerRole(config)
	baseline, err := renderedResourceHash(baselineRole)
	if err != nil {
		t.Fatal(err)
	}

	changedRole := baselineRole.DeepCopy()
	changedRole.Rules[0].Verbs = append(changedRole.Rules[0].Verbs, "get")
	updated, err := renderedResourceHash(changedRole)
	if err != nil {
		t.Fatal(err)
	}

	assert.NotEqual(t, baseline, updated)
}

func TestRenderedResourceHashIgnoresMetadata(t *testing.T) {
	deploy := newProxyServerDeployment(newTestConfig(3), "IfNotPresent", nil)
	baseline, err := renderedResourceHash(deploy)
	if err != nil {
		t.Fatal(err)
	}

	// ensure() stamps these before the conflict retry re-hashes the resource
	deploy.Annotations[common.AnnotationKeyConfigurationGeneration] = "9"
	deploy.Annotations[common.AnnotationKeyRenderedHash] = baseline
	deploy.SetResourceVersion("42")

	stamped, err := renderedResourceHash(deploy)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, baseline, stamped)
}

func TestEnsureUpdatesRenderedDeploymentWithoutGenerationChange(t *testing.T) {
	config := newTestConfig(3)
	config.Name = "cluster-proxy"
	config.Spec.ProxyServer.Namespace = "proxy-system"
	config.Spec.ProxyServer.Image = "example.invalid/cluster-proxy:old"

	existing := newProxyServerDeployment(config, "IfNotPresent", nil)
	existingHash, err := renderedResourceHash(existing)
	if err != nil {
		t.Fatal(err)
	}
	existing.Annotations[common.AnnotationKeyConfigurationGeneration] = "1"
	existing.Annotations[common.AnnotationKeyRenderedHash] = existingHash

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	reconciler := &ManagedProxyConfigurationReconciler{Client: fakeClient}

	changed := config.DeepCopy()
	changed.Spec.ProxyServer.Image = "example.invalid/cluster-proxy:new"
	desired := newProxyServerDeployment(changed, "IfNotPresent", nil)
	desiredHash, err := renderedResourceHash(desired)
	if err != nil {
		t.Fatal(err)
	}

	created, updated, err := reconciler.ensure(
		1,
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		desired,
	)
	if err != nil {
		t.Fatal(err)
	}
	assert.False(t, created)
	assert.True(t, updated)

	actual := &appsv1.Deployment{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(existing), actual); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, changed.Spec.ProxyServer.Image, actual.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, desiredHash, actual.Annotations[common.AnnotationKeyRenderedHash])
}
