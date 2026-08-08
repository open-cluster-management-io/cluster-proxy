package serviceproxy

import (
	"net/http"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apiserver/pkg/authentication/user"
)

func TestHubAuthProviderFactoryFlags(t *testing.T) {
	factory := newHubAuthProviderFactory()
	s := &serviceProxy{authProviderFactories: []authProviderFactory{factory}}
	cmd := &cobra.Command{}
	s.AddFlags(cmd)

	if !factory.enabled() {
		t.Fatal("hub provider should be enabled by default")
	}
	if err := cmd.ParseFlags([]string{
		"--enable-impersonation=false",
		"--hub-kubeconfig=/var/run/secrets/hub/kubeconfig",
	}); err != nil {
		t.Fatalf("unexpected flag parsing error: %v", err)
	}
	if factory.enabled() {
		t.Fatal("hub provider should be disabled by --enable-impersonation=false")
	}
	if got := factory.configFiles(); len(got) != 0 {
		t.Fatalf("disabled provider config files = %v, want none", got)
	}

	factory.enableImpersonation = true
	if got, want := factory.configFiles(), []string{"/var/run/secrets/hub/kubeconfig"}; !slices.Equal(got, want) {
		t.Fatalf("config files = %v, want %v", got, want)
	}
}

func TestHubAuthProviderApplyIdentity_RegularUser(t *testing.T) {
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

	provider := &hubAuthProvider{impersonateUser: s.impersonateUser}
	if err := provider.ApplyIdentity(ctx, req, hubUser); err != nil {
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

func TestHubAuthProviderApplyIdentity_ServiceAccount(t *testing.T) {
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

	provider := &hubAuthProvider{impersonateUser: s.impersonateUser}
	if err := provider.ApplyIdentity(ctx, req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "cluster:hub:system:serviceaccount:proxy-test:proxy-bench"
	if req.Header.Get("Impersonate-User") != expected {
		t.Fatalf("expected impersonate user '%s', got '%s'", expected, req.Header.Get("Impersonate-User"))
	}
}

func TestHubAuthProviderApplyIdentity_IgnoresClientInjectedImpersonationHeaders(t *testing.T) {
	s := &serviceProxy{
		getImpersonateTokenFunc: func() (string, error) {
			return "fake-sa-token", nil
		},
	}
	ctx := t.Context()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
	req.Header.Set("Authorization", "Bearer original-token")
	req.Header.Set("X-Unrelated", "keep-me")
	req.Header.Set(authenticationv1.ImpersonateUserHeader, "system:admin")
	req.Header.Add(authenticationv1.ImpersonateGroupHeader, "system:masters")
	req.Header.Set(authenticationv1.ImpersonateUIDHeader, "escalated-uid")
	req.Header.Set(authenticationv1.ImpersonateUserExtraHeaderPrefix+"scopes.authorization.openshift.io", "user:full")
	req.Header["IMPERSONATE-FUTURE"] = []string{"future-value"}

	hubUser := &user.DefaultInfo{
		Name:   "admin@example.com",
		Groups: []string{"system:authenticated"},
	}
	provider := &hubAuthProvider{impersonateUser: s.impersonateUser}
	if err := provider.ApplyIdentity(ctx, req, hubUser); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := req.Header.Get(authenticationv1.ImpersonateUserHeader); got != "admin@example.com" {
		t.Fatalf("expected authenticated user, got %q", got)
	}
	groups := req.Header.Values(authenticationv1.ImpersonateGroupHeader)
	if len(groups) != 1 || groups[0] != "system:authenticated" {
		t.Fatalf("expected only authenticated group after applying hub identity, got %v", groups)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUIDHeader); got != "" {
		t.Fatalf("expected client-supplied UID to be removed, got %q", got)
	}
	if got := req.Header.Get(authenticationv1.ImpersonateUserExtraHeaderPrefix + "scopes.authorization.openshift.io"); got != "" {
		t.Fatalf("expected client-supplied extra to be removed, got %q", got)
	}
	if _, found := req.Header["IMPERSONATE-FUTURE"]; found {
		t.Fatal("expected future impersonation header to be removed")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer fake-sa-token" {
		t.Fatalf("expected service account authorization token, got %q", got)
	}
	if got := req.Header.Get("X-Unrelated"); got != "keep-me" {
		t.Fatalf("expected unrelated header to be preserved, got %q", got)
	}
}
