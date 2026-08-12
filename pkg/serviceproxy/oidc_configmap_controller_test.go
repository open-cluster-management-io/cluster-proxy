package serviceproxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	oidcauthenticator "k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/client-go/kubernetes/fake"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type oidcDelegateBuild struct {
	ctx      context.Context
	caBundle string
}

func TestOIDCCAConfigMapControllerRespondsToEvents(t *testing.T) {
	const (
		namespace = "addon"
		name      = "oidc-ca"
	)

	authn := mustNewOIDCAuthenticator(t, oidcOptions{caConfigMap: name})
	builds := make(chan oidcDelegateBuild, 2)
	authn.newDelegate = func(ctx context.Context, caBundle []byte) (oidcauthenticator.AuthenticatorTokenWithHealthCheck, func(), error) {
		builds <- oidcDelegateBuild{ctx: ctx, caBundle: string(caBundle)}
		return &fakeOIDCDelegate{authenticateToken: func(context.Context, string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}}, nil, nil
	}

	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := startOIDCCAConfigMapController(ctx, client, namespace, name, authn); err != nil {
		t.Fatalf("start ConfigMap controller: %v", err)
	}
	assertOIDCUnavailable(t, authn, "is not available")

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string]string{oidcCAConfigMapKey: "event-ca"},
	}
	if _, err := client.CoreV1().ConfigMaps(namespace).Create(t.Context(), configMap, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}
	firstBuild := awaitDelegateBuild(t, builds)
	if firstBuild.caBundle != "event-ca" {
		t.Fatalf("expected event CA, got %q", firstBuild.caBundle)
	}

	if err := client.CoreV1().ConfigMaps(namespace).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete ConfigMap: %v", err)
	}
	select {
	case <-firstBuild.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ConfigMap deletion did not cancel the active delegate")
	}
	assertOIDCUnavailable(t, authn, "is not available")

	if _, err := client.CoreV1().ConfigMaps(namespace).Create(t.Context(), configMap, metav1.CreateOptions{}); err != nil {
		t.Fatalf("recreate ConfigMap: %v", err)
	}
	secondBuild := awaitDelegateBuild(t, builds)
	if secondBuild.caBundle != "event-ca" {
		t.Fatalf("expected recreated event CA, got %q", secondBuild.caBundle)
	}
}

func awaitDelegateBuild(t *testing.T, builds <-chan oidcDelegateBuild) oidcDelegateBuild {
	t.Helper()
	select {
	case build := <-builds:
		return build
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OIDC delegate reconciliation")
		return oidcDelegateBuild{}
	}
}

// newTestOIDCCAConfigMapController builds a controller that reads ConfigMaps
// from a caller-populated indexer instead of a running informer.
func newTestOIDCCAConfigMapController(
	namespace, name string,
	authn *oidcAuthenticator,
) (*oidcCAConfigMapController, cache.Indexer) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	return &oidcCAConfigMapController{
		namespace:     namespace,
		name:          name,
		lister:        listerscorev1.NewConfigMapLister(indexer),
		authenticator: authn,
	}, indexer
}

func TestOIDCCAConfigMapReconciliation(t *testing.T) {
	const (
		namespace = "addon"
		name      = "oidc-ca"
	)

	authn := mustNewOIDCAuthenticator(t, oidcOptions{caConfigMap: name})

	var (
		observedBundles [][]byte
		delegateCtxs    []context.Context
	)
	authn.newDelegate = func(ctx context.Context, caBundle []byte) (oidcauthenticator.AuthenticatorTokenWithHealthCheck, func(), error) {
		observedBundles = append(observedBundles, caBundle)
		delegateCtxs = append(delegateCtxs, ctx)
		username := string(caBundle)
		return &fakeOIDCDelegate{authenticateToken: func(context.Context, string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: username}}, true, nil
		}}, nil, nil
	}

	controller, indexer := newTestOIDCCAConfigMapController(namespace, name, authn)

	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile missing ConfigMap: %v", err)
	}
	assertOIDCUnavailable(t, authn, "is not available")
	if len(observedBundles) != 0 {
		t.Fatalf("request-independent initialization should wait for the ConfigMap, got %d builds", len(observedBundles))
	}

	first := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "1"},
		Data:       map[string]string{oidcCAConfigMapKey: "ca-one"},
	}
	if err := indexer.Add(first); err != nil {
		t.Fatalf("add first ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile first ConfigMap: %v", err)
	}
	assertOIDCUsername(t, authn, "ca-one")
	if len(observedBundles) != 1 || string(observedBundles[0]) != "ca-one" {
		t.Fatalf("unexpected first reconciliation: %q", observedBundles)
	}

	metadataOnly := first.DeepCopy()
	metadataOnly.ResourceVersion = "2"
	metadataOnly.Labels = map[string]string{"changed": "true"}
	if err := indexer.Update(metadataOnly); err != nil {
		t.Fatalf("update ConfigMap metadata: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile metadata-only update: %v", err)
	}
	if len(observedBundles) != 1 {
		t.Fatalf("metadata-only update rebuilt the delegate %d times", len(observedBundles))
	}

	rotated := metadataOnly.DeepCopy()
	rotated.ResourceVersion = "3"
	rotated.Data[oidcCAConfigMapKey] = "ca-two"
	if err := indexer.Update(rotated); err != nil {
		t.Fatalf("rotate ConfigMap CA: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile CA rotation: %v", err)
	}
	assertOIDCUsername(t, authn, "ca-two")
	if len(observedBundles) != 2 || string(observedBundles[1]) != "ca-two" {
		t.Fatalf("unexpected CA rotation: %q", observedBundles)
	}
	assertContextCanceled(t, delegateCtxs[0])

	if err := indexer.Delete(rotated); err != nil {
		t.Fatalf("delete ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile ConfigMap deletion: %v", err)
	}
	assertOIDCUnavailable(t, authn, "is not available")
	assertContextCanceled(t, delegateCtxs[1])

	invalid := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "4"},
	}
	if err := indexer.Add(invalid); err != nil {
		t.Fatalf("add invalid ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile invalid ConfigMap: %v", err)
	}
	assertOIDCUnavailable(t, authn, "does not contain ca.crt")
	if len(observedBundles) != 2 {
		t.Fatalf("invalid ConfigMap unexpectedly rebuilt the delegate %d times", len(observedBundles))
	}

	recovered := invalid.DeepCopy()
	recovered.ResourceVersion = "5"
	recovered.BinaryData = map[string][]byte{oidcCAConfigMapKey: []byte("ca-three")}
	if err := indexer.Update(recovered); err != nil {
		t.Fatalf("repair ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile repaired ConfigMap: %v", err)
	}
	assertOIDCUsername(t, authn, "ca-three")
	if len(observedBundles) != 3 || string(observedBundles[2]) != "ca-three" {
		t.Fatalf("unexpected recovery reconciliation: %q", observedBundles)
	}
}

func TestOIDCCAConfigMapRejectsMalformedBundleAndRecovers(t *testing.T) {
	const (
		namespace = "addon"
		name      = "oidc-ca"
	)
	issuer := newFakeIssuer(t)
	opts := issuer.defaultOpts()
	opts.caConfigMap = name
	authn := mustNewOIDCAuthenticator(t, opts)

	controller, indexer := newTestOIDCCAConfigMapController(namespace, name, authn)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "1"},
		Data:       map[string]string{oidcCAConfigMapKey: "not-a-certificate"},
	}
	if err := indexer.Add(configMap); err != nil {
		t.Fatalf("add malformed CA ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile malformed CA ConfigMap: %v", err)
	}
	assertOIDCUnavailable(t, authn, "failed to load oidc CA bundle")

	repaired := configMap.DeepCopy()
	repaired.ResourceVersion = "2"
	repaired.Data[oidcCAConfigMapKey] = string(issuer.caPEM)
	if err := indexer.Update(repaired); err != nil {
		t.Fatalf("repair CA ConfigMap: %v", err)
	}
	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile repaired CA ConfigMap: %v", err)
	}

	token := issuer.signToken(t, issuer.claims(nil))
	response, ok, err := authenticateUntilSettled(t, authn, token, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("expected authentication recovery, got ok=%v err=%v", ok, err)
	}
	if want := issuer.server.URL + "#test-user"; response.User.GetName() != want {
		t.Fatalf("expected username %q, got %q", want, response.User.GetName())
	}
}

func assertOIDCUnavailable(t *testing.T, authn *oidcAuthenticator, errorSubstring string) {
	t.Helper()
	response, ok, err := authn.AuthenticateToken(t.Context(), "token")
	if err == nil || !strings.Contains(err.Error(), errorSubstring) {
		t.Fatalf("expected unavailable error containing %q, got %v", errorSubstring, err)
	}
	if errors.Is(err, ErrTokenNotAuthenticated) {
		t.Fatalf("configuration error must not be a token rejection: %v", err)
	}
	if ok || response != nil {
		t.Fatalf("expected nil unauthenticated response, got ok=%v response=%v", ok, response)
	}
}

func assertOIDCUsername(t *testing.T, authn *oidcAuthenticator, expected string) {
	t.Helper()
	response, ok, err := authn.AuthenticateToken(t.Context(), "token")
	if err != nil || !ok {
		t.Fatalf("expected authentication success, got ok=%v err=%v", ok, err)
	}
	if response.User.GetName() != expected {
		t.Fatalf("expected username %q, got %q", expected, response.User.GetName())
	}
}

func assertContextCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected replaced delegate context to be canceled")
	}
}

func TestOIDCCABundle(t *testing.T) {
	tests := []struct {
		name       string
		configMap  *corev1.ConfigMap
		wantBundle string
		wantError  string
	}{
		{
			name:       "data",
			configMap:  &corev1.ConfigMap{Data: map[string]string{oidcCAConfigMapKey: "text-ca"}},
			wantBundle: "text-ca",
		},
		{
			name:       "binary data",
			configMap:  &corev1.ConfigMap{BinaryData: map[string][]byte{oidcCAConfigMapKey: []byte("binary-ca")}},
			wantBundle: "binary-ca",
		},
		{
			name:      "missing key",
			configMap: &corev1.ConfigMap{},
			wantError: "does not contain ca.crt",
		},
		{
			name:      "empty data",
			configMap: &corev1.ConfigMap{Data: map[string]string{oidcCAConfigMapKey: ""}},
			wantError: "empty ca.crt entry",
		},
		{
			name:      "empty binary data",
			configMap: &corev1.ConfigMap{BinaryData: map[string][]byte{oidcCAConfigMapKey: {}}},
			wantError: "empty ca.crt entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.configMap.Namespace = "addon"
			tt.configMap.Name = "oidc-ca"
			bundle, err := oidcCABundle(tt.configMap)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(bundle) != tt.wantBundle {
				t.Fatalf("expected bundle %q, got %q", tt.wantBundle, bundle)
			}
		})
	}
}
