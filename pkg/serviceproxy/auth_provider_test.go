package serviceproxy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeAuthProvider struct {
	authenticator.Token
	id            authProviderID
	applyIdentity func(context.Context, *http.Request, user.Info) error
}

func (p *fakeAuthProvider) Metadata() authProviderMetadata {
	return authProviderMetadata{
		id:                   p.id,
		displayName:          string(p.id),
		authenticationTarget: string(p.id),
	}
}

func (p *fakeAuthProvider) ApplyIdentity(ctx context.Context, req *http.Request, info user.Info) error {
	if p.applyIdentity == nil {
		return nil
	}
	return p.applyIdentity(ctx, req, info)
}

type fakeAuthProviderFactory struct {
	addFlagsFunc    func(*pflag.FlagSet)
	enabledValue    bool
	configFilePaths []string
	validateFunc    func() error
	buildFunc       func(context.Context, authProviderDependencies) (authProvider, error)
}

func (f fakeAuthProviderFactory) addFlags(flags *pflag.FlagSet) {
	if f.addFlagsFunc != nil {
		f.addFlagsFunc(flags)
	}
}

func (f fakeAuthProviderFactory) validate() error {
	if f.validateFunc == nil {
		return nil
	}
	return f.validateFunc()
}

func (f fakeAuthProviderFactory) enabled() bool {
	return f.enabledValue
}

func (f fakeAuthProviderFactory) configFiles() []string {
	if !f.enabled() {
		return nil
	}
	return f.configFilePaths
}

func (f fakeAuthProviderFactory) build(ctx context.Context, dependencies authProviderDependencies) (authProvider, error) {
	return f.buildFunc(ctx, dependencies)
}

func newFakeAuthProvider(id authProviderID) authProvider {
	return &fakeAuthProvider{
		Token: authenticator.TokenFunc(func(context.Context, string) (*authenticator.Response, bool, error) {
			return nil, false, nil
		}),
		id: id,
	}
}

func TestAuthProviderFactoryLifecycleDelegation(t *testing.T) {
	var flagsAdded, validated []string
	newFactory := func(name string, enabled bool, configFiles ...string) authProviderFactory {
		return fakeAuthProviderFactory{
			addFlagsFunc: func(*pflag.FlagSet) {
				flagsAdded = append(flagsAdded, name)
			},
			enabledValue:    enabled,
			configFilePaths: configFiles,
			validateFunc: func() error {
				validated = append(validated, name)
				return nil
			},
		}
	}

	s := &serviceProxy{
		cert: "tls.crt",
		key:  "tls.key",
		authProviderFactories: []authProviderFactory{
			newFactory("enabled", true, "enabled.conf"),
			newFactory("disabled", false, "disabled.conf"),
		},
	}
	s.AddFlags(&cobra.Command{})
	if err := s.validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	if want := []string{"enabled", "disabled"}; !slices.Equal(flagsAdded, want) {
		t.Fatalf("factories receiving flags = %v, want %v", flagsAdded, want)
	}
	if want := []string{"enabled", "disabled"}; !slices.Equal(validated, want) {
		t.Fatalf("validated factories = %v, want %v", validated, want)
	}
	if want := []string{"enabled.conf"}; !slices.Equal(s.authProviderConfigFiles(), want) {
		t.Fatalf("config files = %v, want %v", s.authProviderConfigFiles(), want)
	}
}

func TestInitializeAuthProvidersUsesEnabledFactoriesInRegistryOrder(t *testing.T) {
	const podNamespace = "addon"
	var (
		built                []authProviderID
		builtInPodNamespaces []string
	)
	newFactory := func(id authProviderID, enabled bool) authProviderFactory {
		return fakeAuthProviderFactory{
			enabledValue: enabled,
			buildFunc: func(_ context.Context, dependencies authProviderDependencies) (authProvider, error) {
				built = append(built, id)
				builtInPodNamespaces = append(builtInPodNamespaces, dependencies.podNamespace)
				return newFakeAuthProvider(id), nil
			},
		}
	}

	s := &serviceProxy{
		podNamespace: podNamespace,
		authProviderFactories: []authProviderFactory{
			newFactory("first", true),
			newFactory("disabled", false),
			newFactory("second", true),
		},
	}

	if err := s.initializeAuthProviders(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := []authProviderID{"first", "second"}; !slices.Equal(built, want) {
		t.Fatalf("built providers = %v, want %v", built, want)
	}
	if want := []string{podNamespace, podNamespace}; !slices.Equal(builtInPodNamespaces, want) {
		t.Fatalf("provider pod namespaces = %v, want %v", builtInPodNamespaces, want)
	}
	providerIDs := make([]authProviderID, 0, len(s.authProviders))
	for _, provider := range s.authProviders {
		providerIDs = append(providerIDs, provider.Metadata().id)
	}
	if want := []authProviderID{managedClusterAuthProviderID, "first", "second"}; !slices.Equal(providerIDs, want) {
		t.Fatalf("published providers = %v, want %v", providerIDs, want)
	}
}

func TestInitializeAuthProvidersDoesNotPublishPartialFactoryResults(t *testing.T) {
	buildErr := errors.New("build failed")
	existing := newFakeAuthProvider("existing")
	s := &serviceProxy{
		podNamespace: "addon",
		authProviderFactories: []authProviderFactory{
			fakeAuthProviderFactory{
				enabledValue: true,
				buildFunc: func(context.Context, authProviderDependencies) (authProvider, error) {
					return newFakeAuthProvider("first"), nil
				},
			},
			fakeAuthProviderFactory{
				enabledValue: true,
				buildFunc: func(context.Context, authProviderDependencies) (authProvider, error) {
					return nil, buildErr
				},
			},
		},
		authProviders: []authProvider{existing},
	}

	if err := s.initializeAuthProviders(t.Context()); !errors.Is(err, buildErr) {
		t.Fatalf("error = %v, want %v", err, buildErr)
	}
	if len(s.authProviders) != 1 || s.authProviders[0] != existing {
		t.Fatalf("published providers changed after failed initialization: %v", s.authProviders)
	}
}

func TestInitializeAuthProviders(t *testing.T) {
	tests := []struct {
		name                string
		enableImpersonation bool
		enableOIDC          bool
		wantProviderIDs     []authProviderID
	}{
		{
			name: "disabled",
		},
		{
			name:                "hub only",
			enableImpersonation: true,
			wantProviderIDs:     []authProviderID{"managed-cluster", "hub"},
		},
		{
			name:            "oidc only",
			enableOIDC:      true,
			wantProviderIDs: []authProviderID{"managed-cluster", "oidc"},
		},
		{
			name:                "hub and oidc",
			enableImpersonation: true,
			enableOIDC:          true,
			wantProviderIDs:     []authProviderID{"managed-cluster", "hub", "oidc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hubFactory := &hubAuthProviderFactory{enableImpersonation: tt.enableImpersonation}
			oidcFactory := newOIDCAuthProviderFactory()
			s := &serviceProxy{
				podNamespace:          "addon",
				authProviderFactories: []authProviderFactory{hubFactory, oidcFactory},
			}
			s.managedClusterKubeClient = fake.NewSimpleClientset()
			hubFactory.kubeConfig = filepath.Join(t.TempDir(), "missing-hub-kubeconfig")
			if tt.enableImpersonation {
				hubFactory.kubeConfig = writeTestHubKubeConfig(t)
			}
			if tt.enableOIDC {
				oidcFactory.options.issuerURL = "https://issuer.example.com"
				oidcFactory.options.clientID = "cluster-proxy"
			}
			if err := s.initializeAuthProviders(t.Context()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			providerIDs := make([]authProviderID, 0, len(s.authProviders))
			for _, provider := range s.authProviders {
				providerIDs = append(providerIDs, provider.Metadata().id)
			}
			if !slices.Equal(providerIDs, tt.wantProviderIDs) {
				t.Fatalf("provider IDs = %v, want %v", providerIDs, tt.wantProviderIDs)
			}
		})
	}
}

func TestInitializeAuthProvidersFailureDoesNotPublishPartialProviders(t *testing.T) {
	s := &serviceProxy{
		managedClusterKubeClient: fake.NewSimpleClientset(),
		podNamespace:             "addon",
		authProviderFactories: []authProviderFactory{
			&hubAuthProviderFactory{
				enableImpersonation: true,
				kubeConfig:          filepath.Join(t.TempDir(), "missing-hub-kubeconfig"),
			},
		},
	}

	if err := s.initializeAuthProviders(t.Context()); err == nil {
		t.Fatal("expected hub kubeconfig error")
	}
	if len(s.authProviders) != 0 {
		t.Fatalf("expected no published providers, got %d", len(s.authProviders))
	}
}

func writeTestHubKubeConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hub.kubeconfig")
	contents := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://hub.example.com
  name: hub
contexts:
- context:
    cluster: hub
    user: hub
  name: hub
current-context: hub
users:
- name: hub
  user:
    token: test-token
`)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatalf("failed to write hub kubeconfig: %v", err)
	}
	return path
}

type testAuthenticators struct {
	managedCluster authenticator.Token
	hub            authenticator.Token
	oidc           authenticator.Token
}

func setTestAuthProviders(s *serviceProxy, authenticators testAuthenticators) {
	providers := make([]authProvider, 0, 3)
	if authenticators.managedCluster != nil {
		providers = append(providers, &managedClusterAuthProvider{
			Token: authenticators.managedCluster,
		})
	}
	if authenticators.hub != nil {
		providers = append(providers, &hubAuthProvider{
			Token:           authenticators.hub,
			impersonateUser: s.impersonateUser,
		})
	}
	if authenticators.oidc != nil {
		providers = append(providers, &oidcAuthProvider{
			Token:           authenticators.oidc,
			impersonateUser: s.impersonateUser,
		})
	}
	s.authProviders = providers
}

func TestAuthProvidersDelegateApplyIdentity(t *testing.T) {
	ctx := t.Context()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	info := &user.DefaultInfo{Name: "alice"}

	for _, provider := range []authProvider{
		&hubAuthProvider{
			impersonateUser: func(gotCtx context.Context, gotReq *http.Request, username string, groups []string) error {
				if gotCtx != ctx || gotReq != req || username != info.GetName() || !slices.Equal(groups, info.GetGroups()) {
					t.Fatal("hub provider did not forward ApplyIdentity arguments")
				}
				return nil
			},
		},
		&oidcAuthProvider{
			impersonateUser: func(gotCtx context.Context, gotReq *http.Request, username string, groups []string) error {
				if gotCtx != ctx || gotReq != req || username != info.GetName() || !slices.Equal(groups, []string{user.AllAuthenticated}) {
					t.Fatal("OIDC provider did not forward ApplyIdentity arguments")
				}
				return nil
			},
		},
	} {
		if err := provider.ApplyIdentity(ctx, req, info); err != nil {
			t.Fatalf("%s ApplyIdentity returned an unexpected error: %v", provider.Metadata().id, err)
		}
	}
}
