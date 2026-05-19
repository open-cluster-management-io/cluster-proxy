package serviceproxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
		enableImpersonation: true,
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
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify impersonation headers were set
	if req.Header.Get("Impersonate-User") != "cluster:hub:system:serviceaccount:ns:my-sa" {
		t.Fatalf("expected impersonate user with cluster:hub: prefix, got '%s'", req.Header.Get("Impersonate-User"))
	}

	// Verify the original token was replaced with the impersonation token
	if req.Header.Get("Authorization") != "Bearer fake-sa-token" {
		t.Fatalf("expected authorization header to use impersonation token, got '%s'", req.Header.Get("Authorization"))
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
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated", "admins"},
	}

	if err := s.processHubUser(context.Background(), req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "system:serviceaccount:proxy-test:proxy-bench",
		Groups: []string{"system:serviceaccounts", "system:authenticated"},
	}

	if err := s.processHubUser(context.Background(), req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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

func TestManagedKubeconfigConfigAndToken(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://managed.example.com:6443
contexts:
- name: managed
  context:
    cluster: managed
    user: cluster-proxy
current-context: managed
users:
- name: cluster-proxy
  user:
    token: managed-token
`
	path := t.TempDir() + "/kubeconfig"
	if err := os.WriteFile(path, []byte(kubeconfig), 0600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}

	s := &serviceProxy{managedKubeConfig: path}
	config, err := s.managedRESTConfig()
	if err != nil {
		t.Fatalf("unexpected managedRESTConfig error: %v", err)
	}
	if config.Host != "https://managed.example.com:6443" {
		t.Fatalf("unexpected managed host: %s", config.Host)
	}

	token, err := s.readImpersonateTokenFromManagedKubeconfig()
	if err != nil {
		t.Fatalf("unexpected token read error: %v", err)
	}
	if token != "managed-token" {
		t.Fatalf("expected managed-token, got %q", token)
	}
}

func TestParseManagedAPIServerURL(t *testing.T) {
	url, err := parseManagedAPIServerURL("https://managed.example.com:6443")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if url.Host != "managed.example.com:6443" {
		t.Fatalf("unexpected host: %s", url.Host)
	}

	if _, err := parseManagedAPIServerURL("managed.example.com:6443"); err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestServiceProxyRelayURLAndAuthorizationHeader(t *testing.T) {
	relayURLTemplate, err := buildServiceRelayURL("https://managed.example.com:6443", "addon-ns")
	if err != nil {
		t.Fatalf("unexpected buildServiceRelayURL error: %v", err)
	}
	s := &serviceProxy{
		managedAPIServerURL:     "https://managed.example.com:6443",
		hostedServiceProxyMode:  ServiceProxyModeRelay,
		relayURLTemplate:        relayURLTemplate,
		getImpersonateTokenFunc: func() (string, error) { return "managed-token", nil },
	}

	relayURL, err := s.serviceRelayURL()
	if err != nil {
		t.Fatalf("unexpected relay URL error: %v", err)
	}
	if relayURL.String() != "https://managed.example.com:6443/api/v1/namespaces/addon-ns/services/http:cluster-proxy-service-relay:7444/proxy" {
		t.Fatalf("unexpected relay URL %s", relayURL.String())
	}

	req, _ := http.NewRequest("GET", "https://example.com/ping", nil)
	req.Header.Set("Authorization", "Bearer original-token")
	req.Header.Set("Cluster-Proxy-Authorization", "Bearer spoofed-token")
	if err := s.prepareRelayRequest(req); err != nil {
		t.Fatalf("unexpected prepare relay request error: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer managed-token" {
		t.Fatalf("expected managed token authorization, got %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Cluster-Proxy-Authorization") != "Bearer original-token" {
		t.Fatalf("expected original authorization in internal header, got %q", req.Header.Get("Cluster-Proxy-Authorization"))
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

func TestTokenReviewAuthenticator_StatusErrorUnauthenticated(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "Credentials are expired",
			},
		}, nil
	})

	authn := &tokenReviewAuthenticator{client: client, name: "test"}
	resp, ok, err := authn.AuthenticateToken(context.Background(), "expired-token")
	if err != nil {
		t.Fatalf("unexpected error when Status.Error is set on unauthenticated TokenReview: %v", err)
	}
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if resp != nil {
		t.Fatal("expected nil response")
	}
}

func TestProcessAuthentication_GetImpersonateTokenError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:authenticated"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "", fmt.Errorf("token file not found")
		},
	}

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(context.Background(), req)
	if err == nil {
		t.Fatal("expected error from getImpersonateTokenFunc")
	}
	if !strings.Contains(err.Error(), "failed to get impersonate token") {
		t.Fatalf("expected impersonate token error, got: %v", err)
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
