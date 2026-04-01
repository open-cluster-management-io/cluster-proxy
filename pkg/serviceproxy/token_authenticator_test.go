package serviceproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeClient creates a fake kubernetes client that responds to TokenReview
// requests with the given authenticated status and user info.
func newFakeClient(authenticated bool, username string, groups []string) *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		tr := &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: authenticated,
				User: authenticationv1.UserInfo{
					Username: username,
					Groups:   groups,
				},
			},
		}
		return true, tr, nil
	})
	return client
}

func TestTokenReviewAuthenticator_Authenticated(t *testing.T) {
	client := newFakeClient(true, "system:serviceaccount:ns:sa", []string{"system:authenticated"})
	authn := &tokenReviewAuthenticator{client: client, name: "test"}

	resp, ok, err := authn.AuthenticateToken(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected authenticated=true")
	}
	if resp.User.GetName() != "system:serviceaccount:ns:sa" {
		t.Fatalf("expected username 'system:serviceaccount:ns:sa', got '%s'", resp.User.GetName())
	}
	if len(resp.User.GetGroups()) != 1 || resp.User.GetGroups()[0] != "system:authenticated" {
		t.Fatalf("unexpected groups: %v", resp.User.GetGroups())
	}
}

func TestTokenReviewAuthenticator_Unauthenticated(t *testing.T) {
	client := newFakeClient(false, "", nil)
	authn := &tokenReviewAuthenticator{client: client, name: "test"}

	resp, ok, err := authn.AuthenticateToken(context.Background(), "bad-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if resp != nil {
		t.Fatal("expected nil response for unauthenticated token")
	}
}

func TestProcessAuthentication_ManagedClusterToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation:         true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "mc-user"}}, true, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for managed cluster token")
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer mc-token")

	if err := s.processAuthentication(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// For managed cluster tokens, no impersonation headers should be set
	if req.Header.Get("Impersonate-User") != "" {
		t.Fatal("impersonation headers should not be set for managed cluster token")
	}
}

func TestProcessAuthentication_HubServiceAccountToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil // not a managed cluster token
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:serviceaccounts", "system:authenticated"},
				},
			}, true, nil
		}),
	}

	// Write a fake SA token file for getImpersonateToken
	origGetToken := s.getImpersonateToken
	_ = origGetToken // use the method directly but we need to mock it

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	// processAuthentication will fail on getImpersonateToken (no SA token file),
	// but we can verify the error message to confirm the flow reached processHubUser
	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error from getImpersonateToken (no SA token file in test)")
	}
	if got := err.Error(); !strings.Contains(got, "failed to get impersonate token") {
		t.Fatalf("expected impersonate token error, got: %v", err)
	}

	// Verify impersonation headers were set before the token file error
	if req.Header.Get("Impersonate-User") != "cluster:hub:system:serviceaccount:ns:my-sa" {
		t.Fatalf("expected impersonate user with cluster:hub: prefix, got '%s'", req.Header.Get("Impersonate-User"))
	}
}

func TestProcessAuthentication_UnauthenticatedToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if !strings.Contains(err.Error(), "neither valid for managed cluster nor hub cluster") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessHubUser_RegularUser(t *testing.T) {
	s := &serviceProxy{}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated", "admins"},
	}

	// Will fail on getImpersonateToken, but we can check headers were set
	_ = s.processHubUser(context.Background(), req, hubUser)

	// Regular user should NOT get cluster:hub: prefix
	if req.Header.Get("Impersonate-User") != "admin@example.com" {
		t.Fatalf("expected impersonate user 'admin@example.com', got '%s'", req.Header.Get("Impersonate-User"))
	}

	groups := req.Header.Values("Impersonate-Group")
	if len(groups) != 2 {
		t.Fatalf("expected 2 impersonate groups, got %d: %v", len(groups), groups)
	}
}

func TestProcessHubUser_ServiceAccount(t *testing.T) {
	s := &serviceProxy{}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "system:serviceaccount:proxy-test:proxy-bench",
		Groups: []string{"system:serviceaccounts", "system:authenticated"},
	}

	_ = s.processHubUser(context.Background(), req, hubUser)

	expected := "cluster:hub:system:serviceaccount:proxy-test:proxy-bench"
	if req.Header.Get("Impersonate-User") != expected {
		t.Fatalf("expected impersonate user '%s', got '%s'", expected, req.Header.Get("Impersonate-User"))
	}
}

func TestConvertExtra(t *testing.T) {
	extra := map[string]authenticationv1.ExtraValue{
		"example.org/scope": {"read", "write"},
	}
	result := convertExtra(extra)
	if len(result) != 1 {
		t.Fatalf("expected 1 key, got %d", len(result))
	}
	if len(result["example.org/scope"]) != 2 {
		t.Fatalf("expected 2 values, got %d", len(result["example.org/scope"]))
	}

	if convertExtra(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestNewServiceProxy_DefaultValues(t *testing.T) {
	s := newServiceProxy()

	if s.tokenReviewCacheTTL != defaultTokenReviewCacheTTL {
		t.Fatalf("expected default TTL %v, got %v", defaultTokenReviewCacheTTL, s.tokenReviewCacheTTL)
	}
	if s.kubeClientQPS != defaultKubeClientQPS {
		t.Fatalf("expected default QPS %v, got %v", defaultKubeClientQPS, s.kubeClientQPS)
	}
	if s.kubeClientBurst != defaultKubeClientBurst {
		t.Fatalf("expected default burst %v, got %v", defaultKubeClientBurst, s.kubeClientBurst)
	}
}

func TestTokenReviewAuthenticator_TokenSentInRequest(t *testing.T) {
	var capturedToken string
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		tr := createAction.GetObject().(*authenticationv1.TokenReview)
		capturedToken = tr.Spec.Token
		return true, &authenticationv1.TokenReview{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Status:     authenticationv1.TokenReviewStatus{Authenticated: false},
		}, nil
	})

	authn := &tokenReviewAuthenticator{client: client, name: "test"}
	authn.AuthenticateToken(context.Background(), "my-secret-token")

	if capturedToken != "my-secret-token" {
		t.Fatalf("expected token 'my-secret-token' to be sent in TokenReview, got '%s'", capturedToken)
	}
}

func TestTokenReviewAuthenticator_APIError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	authn := &tokenReviewAuthenticator{client: client, name: "test"}
	resp, ok, err := authn.AuthenticateToken(context.Background(), "some-token")
	if err == nil {
		t.Fatal("expected error from API call")
	}
	if ok {
		t.Fatal("expected authenticated=false on API error")
	}
	if resp != nil {
		t.Fatal("expected nil response on API error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected 'connection refused' in error, got: %v", err)
	}
}

func TestProcessAuthentication_ManagedClusterAuthError(t *testing.T) {
	hubCalled := false
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("apiserver unreachable")
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			hubCalled = true
			return nil, false, nil
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "managed cluster authentication failed") {
		t.Fatalf("expected managed cluster error, got: %v", err)
	}
	if hubCalled {
		t.Fatal("hub authenticator should not be called when managed cluster auth errors")
	}
}

func TestProcessAuthentication_HubAuthError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil // not a managed cluster token
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("hub apiserver timeout")
		}),
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "hub cluster auth error") {
		t.Fatalf("expected hub cluster auth error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hub apiserver timeout") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}
