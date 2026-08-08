package serviceproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes/fake"
)

func TestOIDCAuthProviderFactoryBuildsConfigMapBackedProvider(t *testing.T) {
	const (
		namespace = "addon"
		name      = "oidc-ca"
	)
	issuer := newFakeIssuer(t)
	factory := &oidcAuthProviderFactory{options: issuer.defaultOpts()}
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string]string{oidcCAConfigMapKey: string(issuer.caPEM)},
	})

	provider, err := factory.build(t.Context(), authProviderDependencies{
		managedClusterKubeClient: client,
		podNamespace:             namespace,
	})
	if err != nil {
		t.Fatalf("build OIDC provider: %v", err)
	}

	token := issuer.signToken(t, issuer.claims(nil))
	response, ok, err := authenticateUntilSettled(t, provider, token, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("expected authentication success, got ok=%v err=%v", ok, err)
	}
	if want := issuer.server.URL + "#test-user"; response.User.GetName() != want {
		t.Fatalf("expected username %q, got %q", want, response.User.GetName())
	}
}

func TestOIDCAuthProviderFactoryDoesNotBuildWhenControllerCannotSync(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	factory := newOIDCAuthProviderFactory()
	factory.options.issuerURL = "https://issuer.example.com"
	factory.options.clientID = "cluster-proxy"
	factory.options.caConfigMap = "oidc-ca"

	provider, err := factory.build(ctx, authProviderDependencies{
		managedClusterKubeClient: fake.NewSimpleClientset(),
		podNamespace:             "addon",
	})
	if err == nil || !strings.Contains(err.Error(), "failed to sync OIDC CA ConfigMap informer") {
		t.Fatalf("expected controller sync error, got provider=%v err=%v", provider, err)
	}
	if provider != nil {
		t.Fatalf("expected no provider after controller startup failure, got %v", provider)
	}
}

func TestProcessAuthentication_OIDCToken(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: rejectTokenReview,
		oidc: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "oidc:alice",
					Groups: []string{"oidc:team-a"},
				},
			}, true, nil
		}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer dex-token")

	if err := s.processAuthentication(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// OIDC users are impersonated without any cluster:hub: prefix
	if req.Header.Get("Impersonate-User") != "oidc:alice" {
		t.Fatalf("expected impersonate user 'oidc:alice', got '%s'", req.Header.Get("Impersonate-User"))
	}
	groups := req.Header.Values("Impersonate-Group")
	if !slices.Equal(groups, []string{"oidc:team-a", user.AllAuthenticated}) {
		t.Fatalf("unexpected impersonate groups: %v", groups)
	}
	if req.Header.Get("Authorization") != "Bearer fake-sa-token" {
		t.Fatalf("expected authorization header to use impersonation token, got '%s'", req.Header.Get("Authorization"))
	}
}

func TestProcessAuthentication_OIDCTokenAlreadyAuthenticatedGroup(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: rejectTokenReview,
		oidc: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "oidc:bob",
					Groups: []string{user.AllAuthenticated},
				},
			}, true, nil
		}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer dex-token")

	if err := s.processAuthentication(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if groups := req.Header.Values("Impersonate-Group"); !slices.Equal(groups, []string{user.AllAuthenticated}) {
		t.Fatalf("expected system:authenticated exactly once, got %v", groups)
	}
}

func TestProcessAuthentication_OIDCTokenRejected(t *testing.T) {
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: rejectTokenReview,
		oidc: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("token expired: %w", ErrTokenNotAuthenticated)
		}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer expired-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if !strings.Contains(err.Error(), "neither valid for managed cluster nor the configured OIDC issuer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessAuthentication_OIDCInfraError(t *testing.T) {
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: rejectTokenReview,
		oidc: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, errors.New("issuer unreachable")
		}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "oidc authentication failed") {
		t.Fatalf("expected oidc authentication error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "issuer unreachable") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}
