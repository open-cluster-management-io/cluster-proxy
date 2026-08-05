package serviceproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
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

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer mc-token")

	if err := s.processAuthentication(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer mc-token" {
		t.Fatalf("expected original authorization header, got %q", got)
	}
	if req.Header.Get(authenticationv1.ImpersonateUserHeader) != "" {
		t.Fatal("impersonation headers should not be set for managed cluster token")
	}
}

func TestHasClientImpersonationHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		expected bool
	}{
		{
			name: "none",
			headers: http.Header{
				"Authorization": {"Bearer token"},
			},
			expected: false,
		},
		{
			name: "lowercase user",
			headers: http.Header{
				"impersonate-user": {"alice"},
			},
			expected: true,
		},
		{
			name: "uppercase group",
			headers: http.Header{
				"IMPERSONATE-GROUP": {"developers"},
			},
			expected: true,
		},
		{
			name: "mixed-case UID",
			headers: http.Header{
				"ImPeRsOnAtE-Uid": {"uid"},
			},
			expected: true,
		},
		{
			name: "lowercase extra",
			headers: http.Header{
				"impersonate-extra-example.org%2Fscope": {"read"},
			},
			expected: true,
		},
		{
			name: "unknown empty impersonation header",
			headers: http.Header{
				"Impersonate-Future": {""},
			},
			expected: true,
		},
		{
			name: "similar header",
			headers: http.Header{
				"X-Impersonate-User": {"alice"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasClientImpersonationHeaders(tt.headers); got != tt.expected {
				t.Fatalf("expected %t, got %t", tt.expected, got)
			}
		})
	}
}

type requestCapturingRoundTripper struct {
	request *http.Request
}

func (t *requestCapturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.request = req.Clone(req.Context())
	t.request.Header = req.Header.Clone()

	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader("ok")),
		ContentLength: 2,
		Request:       req,
	}, nil
}

func (t *requestCapturingRoundTripper) CloseIdleConnections() {}

func TestServeHTTPAuthenticationRouting(t *testing.T) {
	tests := []struct {
		name                       string
		enableImpersonation        bool
		clientImpersonationHeaders bool
		wantAuthenticationCalls    int
	}{
		{
			name:                    "disabled without client impersonation headers",
			enableImpersonation:     false,
			wantAuthenticationCalls: 0,
		},
		{
			name:                       "disabled with client impersonation headers",
			enableImpersonation:        false,
			clientImpersonationHeaders: true,
			wantAuthenticationCalls:    0,
		},
		{
			name:                       "enabled with client impersonation headers",
			enableImpersonation:        true,
			clientImpersonationHeaders: true,
			wantAuthenticationCalls:    0,
		},
		{
			name:                    "enabled without client impersonation headers",
			enableImpersonation:     true,
			wantAuthenticationCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticationCalls := 0
			transport := &requestCapturingRoundTripper{}
			s := &serviceProxy{
				enableImpersonation: tt.enableImpersonation,
				managedClusterAuthenticator: authenticator.TokenFunc(
					func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
						authenticationCalls++
						return &authenticator.Response{User: &user.DefaultInfo{Name: "managed-user"}}, true, nil
					},
				),
				hubAuthenticator: authenticator.TokenFunc(
					func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
						t.Fatal("hub authenticator should not be called")
						return nil, false, nil
					},
				),
				oidcAuthenticator: authenticator.TokenFunc(
					func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
						t.Fatal("OIDC authenticator should not be called")
						return nil, false, nil
					},
				),
				getImpersonateTokenFunc: func() (string, error) {
					t.Fatal("proxy service account token should not be read")
					return "", nil
				},
				proxyTransport: transport,
			}

			req := httptest.NewRequest(http.MethodGet, "https://service-proxy.example/api/v1/pods", nil)
			req.Header.Set("Authorization", "Bearer original-token")
			req.Header.Add("X-Unrelated", "first")
			req.Header.Add("X-Unrelated", "second")
			if tt.clientImpersonationHeaders {
				req.Header.Set(authenticationv1.ImpersonateUserHeader, "alice")
				req.Header.Add(authenticationv1.ImpersonateGroupHeader, "developers")
				req.Header.Add(authenticationv1.ImpersonateGroupHeader, "auditors")
				req.Header.Set(authenticationv1.ImpersonateUIDHeader, "alice-uid")
				req.Header.Add("Impersonate-Extra-example.org%2Fscope", "read")
				req.Header.Add("Impersonate-Extra-example.org%2Fscope", "write")
				req.Header.Set("Impersonate-Future", "future-value")
			}
			req.Header.Set(utils.HeaderClusterProxyProto, "https")
			req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
			req.Header.Set(utils.HeaderClusterProxyService, "kubernetes")
			req.Header.Set(utils.HeaderClusterProxyPort, "443")
			originalHeaders := req.Header.Clone()
			recorder := httptest.NewRecorder()

			s.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("unexpected status: got %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			if authenticationCalls != tt.wantAuthenticationCalls {
				t.Fatalf("authentication called %d times, want %d", authenticationCalls, tt.wantAuthenticationCalls)
			}
			if transport.request == nil {
				t.Fatal("request was not forwarded")
			}
			for key, want := range originalHeaders {
				if got := transport.request.Header.Values(key); !slices.Equal(got, want) {
					t.Errorf("forwarded %s header values = %q, want %q", key, got, want)
				}
			}
		})
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

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(ctx, req)
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

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")

	err := s.processAuthentication(ctx, req)
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
	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated", "admins"},
	}

	if err := s.processHubUser(ctx, req, hubUser); err != nil {
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
	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	hubUser := &user.DefaultInfo{
		Name:   "system:serviceaccount:proxy-test:proxy-bench",
		Groups: []string{"system:serviceaccounts", "system:authenticated"},
	}

	if err := s.processHubUser(ctx, req, hubUser); err != nil {
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

func TestTokenReviewAuthenticator_StatusError_KnownRejection(t *testing.T) {
	tests := []struct {
		name        string
		statusError string
	}{
		{
			name:        "Kubernetes: invalid bearer token",
			statusError: "invalid bearer token",
		},
		{
			name:        "OpenShift: invalid bearer token with token lookup failed",
			statusError: "[invalid bearer token, token lookup failed]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authenticationv1.TokenReview{
					Status: authenticationv1.TokenReviewStatus{
						Authenticated: false,
						Error:         tt.statusError,
					},
				}, nil
			})

			authn := &tokenReviewAuthenticator{client: client, name: "test"}
			resp, ok, err := authn.AuthenticateToken(context.Background(), "bad-token")
			if err == nil {
				t.Fatal("expected error when Status.Error is set")
			}
			if ok {
				t.Fatal("expected authenticated=false")
			}
			if resp != nil {
				t.Fatal("expected nil response")
			}
			if !errors.Is(err, ErrTokenNotAuthenticated) {
				t.Fatalf("expected ErrTokenNotAuthenticated, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.statusError) {
				t.Fatalf("expected Status.Error in error message, got: %v", err)
			}
		})
	}
}

func TestTokenReviewAuthenticator_StatusError_UnknownError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "webhook authenticator connection reset",
			},
		}, nil
	})

	authn := &tokenReviewAuthenticator{client: client, name: "test"}
	resp, ok, err := authn.AuthenticateToken(context.Background(), "some-token")
	if err == nil {
		t.Fatal("expected error when Status.Error is set")
	}
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if resp != nil {
		t.Fatal("expected nil response")
	}
	if errors.Is(err, ErrTokenNotAuthenticated) {
		t.Fatal("unknown Status.Error should NOT be wrapped with ErrTokenNotAuthenticated")
	}
	if !strings.Contains(err.Error(), "webhook authenticator connection reset") {
		t.Fatalf("expected Status.Error in error message, got: %v", err)
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

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected error from getImpersonateTokenFunc")
	}
	if !strings.Contains(err.Error(), "failed to get impersonate token") {
		t.Fatalf("expected impersonate token error, got: %v", err)
	}
}

func TestProcessAuthentication_ManagedClusterFatalError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("apiserver unreachable")
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for fatal managed cluster errors")
			return nil, false, nil
		}),
	}

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected fatal error when managed cluster auth has infrastructure failure")
	}
	if !strings.Contains(err.Error(), "apiserver unreachable") {
		t.Fatalf("expected original error preserved, got: %v", err)
	}
}

func TestProcessAuthentication_OpenShiftTokenReviewError_FallsBackToHub(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf(
				"managed cluster TokenReview: invalid bearer token, token lookup failed: %w",
				ErrTokenNotAuthenticated,
			)
		}),
		hubAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "kube:admin",
					Groups: []string{"system:cluster-admins", "system:authenticated"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer hub-only-token")

	err := s.processAuthentication(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Header.Get("Impersonate-User") != "kube:admin" {
		t.Fatalf("expected impersonate user 'kube:admin', got '%s'", req.Header.Get("Impersonate-User"))
	}
	if req.Header.Get("Authorization") != "Bearer fake-sa-token" {
		t.Fatalf("expected authorization header to use impersonation token, got '%s'", req.Header.Get("Authorization"))
	}
}

func TestProcessAuthentication_HubAuthError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation: true,
		managedClusterAuthenticator: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, nil // not a managed cluster token
			}),
		hubAuthenticator: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, fmt.Errorf("hub apiserver timeout")
			}),
	}

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
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

func TestProcessHubUser_IgnoresClientInjectedGroups(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Add(authenticationv1.ImpersonateGroupHeader, "system:masters")

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated"},
	}
	if err := s.processHubUser(ctx, req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	groups := req.Header.Values(authenticationv1.ImpersonateGroupHeader)
	if len(groups) != 1 || groups[0] != "system:authenticated" {
		t.Fatalf("expected only authenticated group after processHubUser, got %v", groups)
	}
}
