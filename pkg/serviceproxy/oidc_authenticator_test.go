package serviceproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
)

// rejectTokenReview rejects every token, so requests fall through to the OIDC
// authenticator.
var rejectTokenReview = authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	return nil, false, nil
})

// fakeIssuer is a minimal OIDC issuer built on go-oidc's oidctest server,
// serving the discovery document and a JWKS for a generated RSA key, so tests
// can sign and verify real ID tokens.
type fakeIssuer struct {
	server         *httptest.Server
	key            *rsa.PrivateKey
	caPEM          []byte
	caFile         string
	discoveryTried atomic.Bool
	failDiscovery  atomic.Bool
}

type fakeOIDCDelegate struct {
	healthErr         error
	authenticateToken func(context.Context, string) (*authenticator.Response, bool, error)
}

func (f *fakeOIDCDelegate) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	return f.authenticateToken(ctx, token)
}

func (f *fakeOIDCDelegate) HealthCheck() error {
	return f.healthErr
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	f := &fakeIssuer{key: key}

	oidcServer := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{
			PublicKey: key.Public(),
			KeyID:     "test-key",
			Algorithm: oidc.RS256,
		}},
	}
	// intercept discovery so tests can observe attempts and simulate an outage
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			f.discoveryTried.Store(true)
			if f.failDiscovery.Load() {
				http.Error(w, "discovery unavailable", http.StatusInternalServerError)
				return
			}
		}
		oidcServer.ServeHTTP(w, r)
	})

	f.server = httptest.NewTLSServer(handler)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	f.caFile = filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(f.caFile, f.caPEM, 0600); err != nil {
		t.Fatalf("failed to write fake issuer CA file: %v", err)
	}
	t.Cleanup(f.server.Close)
	oidcServer.SetIssuer(f.server.URL)
	return f
}

func authenticateUntilSettled(t *testing.T, authn authenticator.Token, token string, timeout time.Duration) (*authenticator.Response, bool, error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		resp, ok, err := authn.AuthenticateToken(context.Background(), token)
		if err == nil || errors.Is(err, ErrTokenNotAuthenticated) {
			return resp, ok, err
		}
		if time.Now().After(deadline) {
			t.Fatalf("OIDC authenticator did not settle within %v: %v", timeout, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// defaultOpts returns options that authenticate this issuer's default tokens.
func (f *fakeIssuer) defaultOpts() oidcOptions {
	return oidcOptions{
		issuerURL:            f.server.URL,
		clientID:             "test-client",
		usernameClaim:        "sub",
		caFile:               f.caFile,
		signingAlgs:          []string{oidc.RS256},
		reservedNamePrefixes: []string{"system:"}, // the --oidc-reserved-name-prefixes flag default
	}
}

// claims returns a valid default claim set for this issuer, with overrides applied.
func (f *fakeIssuer) claims(overrides map[string]any) map[string]any {
	claims := map[string]any{
		"iss": f.server.URL,
		"aud": "test-client",
		"sub": "test-user",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
	maps.Copy(claims, overrides)
	return claims
}

func (f *fakeIssuer) signToken(t *testing.T, claims map[string]any) string {
	t.Helper()

	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("failed to marshal claims: %v", err)
	}
	return oidctest.SignIDToken(f.key, "test-key", oidc.RS256, string(payload))
}

func TestOIDCAuthenticator_ClaimMapping(t *testing.T) {
	issuer := newFakeIssuer(t)

	tests := []struct {
		name         string
		opts         func(*oidcOptions) // applied on top of defaultOpts()
		claims       map[string]any
		wantName     string // "<issuer>" is substituted with the fake issuer's URL
		wantGroups   []string
		wantRejected bool
	}{
		{
			name:     "default sub claim",
			wantName: "<issuer>#test-user",
		},
		{
			name:     "explicitly disable username prefix",
			opts:     func(o *oidcOptions) { o.usernamePrefix = "-" },
			wantName: "test-user",
		},
		{
			name:     "email claim with prefix",
			opts:     func(o *oidcOptions) { o.usernameClaim, o.usernamePrefix = "email", "oidc:" },
			claims:   map[string]any{"email": "admin@example.com"},
			wantName: "oidc:admin@example.com",
		},
		{
			name:     "email claim with email_verified true",
			opts:     func(o *oidcOptions) { o.usernameClaim = "email" },
			claims:   map[string]any{"email": "admin@example.com", "email_verified": true},
			wantName: "admin@example.com",
		},
		{
			name:         "email claim with email_verified false",
			opts:         func(o *oidcOptions) { o.usernameClaim = "email" },
			claims:       map[string]any{"email": "admin@example.com", "email_verified": false},
			wantRejected: true,
		},
		{
			name:       "groups list with prefix",
			opts:       func(o *oidcOptions) { o.groupsClaim, o.groupsPrefix = "groups", "oidc:" },
			claims:     map[string]any{"groups": []string{"team-a", "team-b"}},
			wantName:   "<issuer>#test-user",
			wantGroups: []string{"oidc:team-a", "oidc:team-b"},
		},
		{
			name:       "groups as single string",
			opts:       func(o *oidcOptions) { o.groupsClaim = "groups" },
			claims:     map[string]any{"groups": "team-a"},
			wantName:   "<issuer>#test-user",
			wantGroups: []string{"team-a"},
		},
		{
			name:     "groups claim absent",
			opts:     func(o *oidcOptions) { o.groupsClaim = "groups" },
			wantName: "<issuer>#test-user",
		},
		{
			name:         "group with reserved system prefix",
			opts:         func(o *oidcOptions) { o.groupsClaim = "groups" },
			claims:       map[string]any{"groups": []string{"system:masters", "team-a"}},
			wantRejected: true,
		},
		{
			name:         "expired token",
			claims:       map[string]any{"exp": time.Now().Add(-time.Hour).Unix()},
			wantRejected: true,
		},
		{
			name:         "wrong audience",
			claims:       map[string]any{"aud": "other-client"},
			wantRejected: true,
		},
		{
			name:         "username claim missing",
			opts:         func(o *oidcOptions) { o.usernameClaim = "email" },
			wantRejected: true,
		},
		{
			name:         "username with reserved system prefix",
			opts:         func(o *oidcOptions) { o.usernamePrefix = "-" },
			claims:       map[string]any{"sub": "system:admin"},
			wantRejected: true,
		},
		{
			name:         "username with custom reserved prefix",
			opts:         func(o *oidcOptions) { o.usernamePrefix, o.reservedNamePrefixes = "-", []string{"system:", "dev:"} },
			claims:       map[string]any{"sub": "dev:admin"},
			wantRejected: true,
		},
		{
			name:         "group with custom reserved prefix",
			opts:         func(o *oidcOptions) { o.groupsClaim, o.reservedNamePrefixes = "groups", []string{"system:", "dev:"} },
			claims:       map[string]any{"groups": []string{"dev:ops", "team-a"}},
			wantRejected: true,
		},
		{
			name:       "custom reserved prefixes replace the default",
			opts:       func(o *oidcOptions) { o.groupsClaim, o.reservedNamePrefixes = "groups", []string{"dev:"} },
			claims:     map[string]any{"groups": []string{"system:masters"}},
			wantName:   "<issuer>#test-user",
			wantGroups: []string{"system:masters"},
		},
		{
			name:       "reserved prefix check disabled",
			opts:       func(o *oidcOptions) { o.groupsClaim, o.reservedNamePrefixes = "groups", []string{} },
			claims:     map[string]any{"groups": []string{"system:masters"}},
			wantName:   "<issuer>#test-user",
			wantGroups: []string{"system:masters"},
		},
		{
			name:         "non-string group member",
			opts:         func(o *oidcOptions) { o.groupsClaim = "groups" },
			claims:       map[string]any{"groups": []any{"team-a", 42}},
			wantRejected: true,
		},
		{
			name:     "required claims match",
			opts:     func(o *oidcOptions) { o.requiredClaims = map[string]string{"hd": "example.com", "tenant": "tenant-id"} },
			claims:   map[string]any{"hd": "example.com", "tenant": "tenant-id"},
			wantName: "<issuer>#test-user",
		},
		{
			name:         "required claim missing",
			opts:         func(o *oidcOptions) { o.requiredClaims = map[string]string{"hd": "example.com"} },
			wantRejected: true,
		},
		{
			name:         "required claim value mismatch",
			opts:         func(o *oidcOptions) { o.requiredClaims = map[string]string{"hd": "example.com"} },
			claims:       map[string]any{"hd": "other.example.com"},
			wantRejected: true,
		},
		{
			name:         "signing algorithm not allowed",
			opts:         func(o *oidcOptions) { o.signingAlgs = []string{oidc.ES256} },
			wantRejected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := issuer.defaultOpts()
			if tt.opts != nil {
				tt.opts(&opts)
			}
			authn := newOIDCAuthenticator(t.Context(), opts)

			token := issuer.signToken(t, issuer.claims(tt.claims))
			resp, ok, err := authenticateUntilSettled(t, authn, token, 5*time.Second)
			if tt.wantRejected {
				if ok {
					t.Fatal("expected authenticated=false")
				}
				if !errors.Is(err, ErrTokenNotAuthenticated) {
					t.Fatalf("expected ErrTokenNotAuthenticated, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				t.Fatal("expected authenticated=true")
			}
			wantName := strings.ReplaceAll(tt.wantName, "<issuer>", issuer.server.URL)
			if resp.User.GetName() != wantName {
				t.Fatalf("expected username %q, got %q", wantName, resp.User.GetName())
			}
			if !slices.Equal(resp.User.GetGroups(), tt.wantGroups) {
				t.Fatalf("expected groups %v, got %v", tt.wantGroups, resp.User.GetGroups())
			}
		})
	}
}

func TestOIDCAuthenticator_HealthSnapshot(t *testing.T) {
	t.Run("startup failure remains an infrastructure error when initialization completes during authentication", func(t *testing.T) {
		startupErr := errors.New("authenticator not initialized")
		delegate := &fakeOIDCDelegate{healthErr: startupErr}
		delegate.authenticateToken = func(context.Context, string) (*authenticator.Response, bool, error) {
			delegate.healthErr = nil
			return nil, false, errors.New("oidc: authenticator not initialized")
		}
		authn := &oidcAuthenticator{delegate: delegate}

		resp, ok, err := authn.AuthenticateToken(t.Context(), "token")
		if !errors.Is(err, startupErr) {
			t.Fatalf("expected startup error, got: %v", err)
		}
		if errors.Is(err, ErrTokenNotAuthenticated) {
			t.Fatalf("startup error must not be classified as a token rejection: %v", err)
		}
		if ok || resp != nil {
			t.Fatalf("expected unauthenticated nil response, got ok=%v resp=%v", ok, resp)
		}
	})

	t.Run("healthy authenticator failure is a token rejection", func(t *testing.T) {
		delegate := &fakeOIDCDelegate{
			authenticateToken: func(context.Context, string) (*authenticator.Response, bool, error) {
				return nil, false, errors.New("invalid signature")
			},
		}
		authn := &oidcAuthenticator{delegate: delegate}

		resp, ok, err := authn.AuthenticateToken(t.Context(), "token")
		if !errors.Is(err, ErrTokenNotAuthenticated) {
			t.Fatalf("expected token rejection, got: %v", err)
		}
		if ok || resp != nil {
			t.Fatalf("expected unauthenticated nil response, got ok=%v resp=%v", ok, resp)
		}
	})

	t.Run("unhealthy authenticator preserves a normal unauthenticated result", func(t *testing.T) {
		delegate := &fakeOIDCDelegate{
			healthErr: errors.New("authenticator not initialized"),
			authenticateToken: func(context.Context, string) (*authenticator.Response, bool, error) {
				return nil, false, nil
			},
		}
		authn := &oidcAuthenticator{delegate: delegate}

		resp, ok, err := authn.AuthenticateToken(t.Context(), "foreign-token")
		if err != nil {
			t.Fatalf("expected normal unauthenticated result, got: %v", err)
		}
		if ok || resp != nil {
			t.Fatalf("expected unauthenticated nil response, got ok=%v resp=%v", ok, resp)
		}
	})
}

func TestOIDCAuthenticator_MalformedToken(t *testing.T) {
	issuer := newFakeIssuer(t)
	authn := newOIDCAuthenticator(t.Context(), issuer.defaultOpts())

	for _, token := range []string{"garbage", "a.b", "a.!!!.c", ""} {
		resp, ok, err := authn.AuthenticateToken(context.Background(), token)
		if ok {
			t.Fatalf("expected authenticated=false for token %q", token)
		}
		if resp != nil {
			t.Fatalf("expected nil response for token %q", token)
		}
		if err != nil {
			t.Fatalf("expected a normal unauthenticated result for token %q, got: %v", token, err)
		}
	}
}

func TestOIDCAuthenticator_ForeignIssuer(t *testing.T) {
	issuer := newFakeIssuer(t)

	authn := newOIDCAuthenticator(t.Context(), issuer.defaultOpts())
	token := issuer.signToken(t, issuer.claims(map[string]any{"iss": "https://foreign.example.com"}))

	_, ok, err := authn.AuthenticateToken(context.Background(), token)
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if err != nil {
		t.Fatalf("expected a normal unauthenticated result, got: %v", err)
	}
}

func TestOIDCAuthenticator_IssuerUnreachable(t *testing.T) {
	issuer := newFakeIssuer(t)
	token := issuer.signToken(t, issuer.claims(nil))
	issuer.server.Close()

	authn := newOIDCAuthenticator(t.Context(), issuer.defaultOpts())
	resp, ok, err := authn.AuthenticateToken(context.Background(), token)
	if err == nil {
		t.Fatal("expected error when issuer is unreachable")
	}
	if errors.Is(err, ErrTokenNotAuthenticated) {
		t.Fatal("issuer unreachable should NOT be wrapped with ErrTokenNotAuthenticated")
	}
	if ok {
		t.Fatal("expected authenticated=false")
	}
	if resp != nil {
		t.Fatal("expected nil response")
	}
}

func TestOIDCAuthenticator_DiscoveryRetry(t *testing.T) {
	issuer := newFakeIssuer(t)
	authn := newOIDCAuthenticator(t.Context(), issuer.defaultOpts())
	token := issuer.signToken(t, issuer.claims(nil))

	// first request fails discovery -> infrastructure error, not cached
	issuer.failDiscovery.Store(true)
	_, ok, err := authn.AuthenticateToken(context.Background(), token)
	if err == nil || ok {
		t.Fatalf("expected discovery failure, got ok=%v err=%v", ok, err)
	}
	if errors.Is(err, ErrTokenNotAuthenticated) {
		t.Fatal("discovery failure should NOT be wrapped with ErrTokenNotAuthenticated")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !issuer.discoveryTried.Load() {
		if time.Now().After(deadline) {
			t.Fatal("expected the initial discovery attempt")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// once the issuer recovers, Kubernetes' background discovery loop succeeds
	// without a pod restart
	issuer.failDiscovery.Store(false)
	resp, ok, err := authenticateUntilSettled(t, authn, token, 15*time.Second)
	if err != nil {
		t.Fatalf("unexpected error after issuer recovery: %v", err)
	}
	if !ok {
		t.Fatal("expected authenticated=true after issuer recovery")
	}
	wantName := issuer.server.URL + "#test-user"
	if resp.User.GetName() != wantName {
		t.Fatalf("expected username %q, got %q", wantName, resp.User.GetName())
	}

	// the verifier is cached after the first success: discovery going down
	// again must not affect verification of further tokens
	issuer.failDiscovery.Store(true)
	if _, ok, err := authn.AuthenticateToken(context.Background(), token); err != nil || !ok {
		t.Fatalf("expected cached verifier to keep working, got ok=%v err=%v", ok, err)
	}
}

func TestOIDCAuthenticator_CAFileMissing(t *testing.T) {
	issuer := newFakeIssuer(t)
	token := issuer.signToken(t, issuer.claims(nil))

	caFile := filepath.Join(t.TempDir(), "missing.crt")
	opts := issuer.defaultOpts()
	opts.caFile = caFile
	authn := newOIDCAuthenticator(t.Context(), opts)

	_, ok, err := authn.AuthenticateToken(context.Background(), token)
	if err == nil || ok {
		t.Fatalf("expected CA file error, got ok=%v err=%v", ok, err)
	}
	if errors.Is(err, ErrTokenNotAuthenticated) {
		t.Fatal("missing CA file should NOT be wrapped with ErrTokenNotAuthenticated")
	}
	if !strings.Contains(err.Error(), "failed to read oidc CA file") {
		t.Fatalf("expected CA file read error, got: %v", err)
	}

	if err := os.WriteFile(caFile, issuer.caPEM, 0600); err != nil {
		t.Fatalf("failed to create late OIDC CA file: %v", err)
	}
	resp, ok, err := authenticateUntilSettled(t, authn, token, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("expected authentication to recover after CA creation, got ok=%v err=%v", ok, err)
	}
	if wantName := issuer.server.URL + "#test-user"; resp.User.GetName() != wantName {
		t.Fatalf("expected username %q, got %q", wantName, resp.User.GetName())
	}
}

func TestProcessAuthentication_OIDCToken(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation:         true,
		managedClusterAuthenticator: rejectTokenReview,
		hubAuthenticator:            rejectTokenReview,
		oidcAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "oidc:alice",
					Groups: []string{"oidc:team-a"},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

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
		enableImpersonation:         true,
		managedClusterAuthenticator: rejectTokenReview,
		hubAuthenticator:            rejectTokenReview,
		oidcAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return &authenticator.Response{
				User: &user.DefaultInfo{
					Name:   "oidc:bob",
					Groups: []string{user.AllAuthenticated},
				},
			}, true, nil
		}),
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}

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
	s := &serviceProxy{
		enableImpersonation:         true,
		managedClusterAuthenticator: rejectTokenReview,
		hubAuthenticator:            rejectTokenReview,
		oidcAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, fmt.Errorf("token expired: %w", ErrTokenNotAuthenticated)
		}),
	}

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer expired-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if !strings.Contains(err.Error(), "not valid for managed cluster, hub cluster, or the configured OIDC issuer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessAuthentication_OIDCInfraError(t *testing.T) {
	s := &serviceProxy{
		enableImpersonation:         true,
		managedClusterAuthenticator: rejectTokenReview,
		hubAuthenticator:            rejectTokenReview,
		oidcAuthenticator: authenticator.TokenFunc(func(ctx context.Context, token string) (*authenticator.Response, bool, error) {
			return nil, false, errors.New("issuer unreachable")
		}),
	}

	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	err := s.processAuthentication(ctx, req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "oidc auth error") {
		t.Fatalf("expected oidc auth error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "issuer unreachable") {
		t.Fatalf("expected original error message preserved, got: %v", err)
	}
}

func TestValidate_OIDCFlags(t *testing.T) {
	tests := []struct {
		name            string
		oidc            oidcOptions
		noImpersonation bool
		wantErr         string
	}{
		{
			name: "oidc disabled",
		},
		{
			name: "valid oidc configuration",
			oidc: oidcOptions{
				issuerURL:     "https://dex.example.com/dex",
				clientID:      "cluster-proxy",
				usernameClaim: "sub",
				signingAlgs:   []string{oidc.RS256},
			},
		},
		{
			name:    "issuer without client id",
			oidc:    oidcOptions{issuerURL: "https://dex.example.com/dex"},
			wantErr: "--oidc-issuer-url and --oidc-client-id must be specified together",
		},
		{
			name: "non-https issuer",
			oidc: oidcOptions{
				issuerURL:     "http://dex.example.com/dex",
				clientID:      "cluster-proxy",
				usernameClaim: "sub",
				signingAlgs:   []string{oidc.RS256},
			},
			wantErr: "URL scheme must be https",
		},
		{
			name:            "issuer without impersonation",
			oidc:            oidcOptions{issuerURL: "https://dex.example.com/dex", clientID: "cluster-proxy"},
			noImpersonation: true,
			wantErr:         "--oidc-issuer-url requires --enable-impersonation=true",
		},
		{
			name:    "client id without issuer",
			oidc:    oidcOptions{clientID: "cluster-proxy"},
			wantErr: "--oidc-issuer-url and --oidc-client-id must be specified together",
		},
		{
			name: "groups claim without issuer",
			oidc: oidcOptions{groupsClaim: "groups"},
		},
		{
			name: "unsupported signing algorithm",
			oidc: oidcOptions{
				issuerURL:     "https://dex.example.com/dex",
				clientID:      "cluster-proxy",
				usernameClaim: "sub",
				signingAlgs:   []string{"HS256"},
			},
			wantErr: `unsupported OIDC signing algorithm "HS256"`,
		},
		{
			name: "empty reserved name prefix",
			oidc: oidcOptions{
				issuerURL:            "https://dex.example.com/dex",
				clientID:             "cluster-proxy",
				usernameClaim:        "sub",
				signingAlgs:          []string{oidc.RS256},
				reservedNamePrefixes: []string{"system:", ""},
			},
			wantErr: "--oidc-reserved-name-prefixes must not contain an empty prefix",
		},
		{
			// a prefix containing CEL string syntax must be quoted into the
			// expression, not break out of it
			name: "reserved name prefix requiring CEL escaping",
			oidc: oidcOptions{
				issuerURL:            "https://dex.example.com/dex",
				clientID:             "cluster-proxy",
				usernameClaim:        "sub",
				signingAlgs:          []string{oidc.RS256},
				reservedNamePrefixes: []string{`quote"backslash\:`},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serviceProxy{cert: "tls.crt", key: "tls.key", enableImpersonation: !tt.noImpersonation, oidc: tt.oidc}

			err := s.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestOIDCReservedNamePrefixesDisabled(t *testing.T) {
	s := newServiceProxy()
	cmd := &cobra.Command{}
	s.AddFlags(cmd)

	if err := cmd.ParseFlags([]string{"--oidc-reserved-name-prefixes="}); err != nil {
		t.Fatalf("unexpected flag parsing error: %v", err)
	}
	if len(s.oidc.reservedNamePrefixes) != 0 {
		t.Fatalf("expected no reserved name prefixes, got %q", s.oidc.reservedNamePrefixes)
	}
}

func TestOIDCFlagParsing(t *testing.T) {
	s := newServiceProxy()
	cmd := &cobra.Command{}
	s.AddFlags(cmd)

	if !slices.Equal(s.oidc.reservedNamePrefixes, []string{"system:"}) {
		t.Fatalf("expected reserved name prefixes to default to [system:], got %v", s.oidc.reservedNamePrefixes)
	}

	err := cmd.ParseFlags([]string{
		"--oidc-signing-algs=RS256,ES256",
		"--oidc-required-claim=hd=example.com",
		"--oidc-required-claim=tenant=value=with=equals",
		"--oidc-reserved-name-prefixes=system:,dev:",
	})
	if err != nil {
		t.Fatalf("unexpected flag parsing error: %v", err)
	}
	if !slices.Equal(s.oidc.signingAlgs, []string{"RS256", "ES256"}) {
		t.Fatalf("unexpected signing algorithms: %v", s.oidc.signingAlgs)
	}
	if !slices.Equal(s.oidc.reservedNamePrefixes, []string{"system:", "dev:"}) {
		t.Fatalf("unexpected reserved name prefixes: %v", s.oidc.reservedNamePrefixes)
	}
	wantRequiredClaims := map[string]string{
		"hd":     "example.com",
		"tenant": "value=with=equals",
	}
	if !maps.Equal(s.oidc.requiredClaims, wantRequiredClaims) {
		t.Fatalf("expected required claims %v, got %v", wantRequiredClaims, s.oidc.requiredClaims)
	}
}
