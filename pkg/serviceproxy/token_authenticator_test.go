package serviceproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestNewTokenReviewAuthenticatorCache(t *testing.T) {
	client := fake.NewSimpleClientset()

	uncached := newTokenReviewAuthenticator(client, managedClusterAuthProviderMetadata.displayName, 0)
	if _, ok := uncached.(*tokenReviewAuthenticator); !ok {
		t.Fatalf("uncached authenticator type = %T, want *tokenReviewAuthenticator", uncached)
	}

	cached := newTokenReviewAuthenticator(client, managedClusterAuthProviderMetadata.displayName, time.Second)
	if _, ok := cached.(*tokenReviewAuthenticator); ok {
		t.Fatalf("cached authenticator was not wrapped: %T", cached)
	}
}
