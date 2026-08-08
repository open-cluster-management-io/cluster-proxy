package serviceproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

func TestProcessAuthentication_ManagedClusterToken(t *testing.T) {
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "mc-user"}}, true, nil
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for managed cluster token")
			return nil, false, nil
		}),
	})

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
		authenticationEnabled      bool
		clientImpersonationHeaders bool
		wantAuthenticationCalls    int
	}{
		{
			name:                    "disabled without client impersonation headers",
			wantAuthenticationCalls: 0,
		},
		{
			name:                       "disabled with client impersonation headers",
			clientImpersonationHeaders: true,
			wantAuthenticationCalls:    0,
		},
		{
			name:                       "hub authentication enabled with client impersonation headers",
			authenticationEnabled:      true,
			clientImpersonationHeaders: true,
			wantAuthenticationCalls:    0,
		},
		{
			name:                    "hub authentication enabled without client impersonation headers",
			authenticationEnabled:   true,
			wantAuthenticationCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authenticationCalls := 0
			transport := &requestCapturingRoundTripper{}
			s := &serviceProxy{
				getImpersonateTokenFunc: func() (string, error) {
					t.Fatal("proxy service account token should not be read")
					return "", nil
				},
				proxyTransport: transport,
			}
			if tt.authenticationEnabled {
				setTestAuthProviders(s, testAuthenticators{
					managedCluster: authenticator.TokenFunc(
						func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
							authenticationCalls++
							return &authenticator.Response{User: &user.DefaultInfo{Name: "managed-user"}}, true, nil
						},
					),
					hub: authenticator.TokenFunc(
						func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
							t.Fatal("hub authenticator should not be called")
							return nil, false, nil
						},
					),
				})
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
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil // not a managed cluster token
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:serviceaccounts", "system:authenticated"},
				},
			}, true, nil
		}),
	})

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
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
	})

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

func TestProcessAuthentication_AllProvidersRejectInOrder(t *testing.T) {
	var calls []string
	reject := func(name string) authenticator.Token {
		return authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			calls = append(calls, name)
			return nil, false, nil
		})
	}
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: reject("managed cluster"),
		hub:            reject("hub"),
		oidc:           reject("oidc"),
	})

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	err := s.processAuthentication(req.Context(), req)
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if want := "not valid for managed cluster, hub cluster, or the configured OIDC issuer"; !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %v", want, err)
	}
	if want := []string{"managed cluster", "hub", "oidc"}; !slices.Equal(calls, want) {
		t.Fatalf("provider calls = %v, want %v", calls, want)
	}
}

func TestProcessAuthentication_GetImpersonateTokenError(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "", fmt.Errorf("token file not found")
		},
	}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "system:serviceaccount:ns:my-sa",
					Groups: []string{"system:authenticated"},
				},
			}, true, nil
		}),
	})

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
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("apiserver unreachable")
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			t.Fatal("hub authenticator should not be called for fatal managed cluster errors")
			return nil, false, nil
		}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected fatal error when managed cluster auth has infrastructure failure")
	}
	if want := "managed cluster authentication failed: apiserver unreachable"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestProcessAuthentication_OpenShiftTokenReviewError_FallsBackToHub(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf(
				"managed cluster TokenReview: invalid bearer token, token lookup failed: %w",
				ErrTokenNotAuthenticated,
			)
		}),
		hub: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "kube:admin",
					Groups: []string{"system:cluster-admins", "system:authenticated"},
				},
			}, true, nil
		}),
	})

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
	s := &serviceProxy{}
	setTestAuthProviders(s, testAuthenticators{
		managedCluster: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, nil // not a managed cluster token
			}),
		hub: authenticator.TokenFunc(
			func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
				return nil, false, fmt.Errorf("hub apiserver timeout")
			}),
	})

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "hub authentication failed") {
		t.Fatalf("expected hub authentication error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hub apiserver timeout") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}
