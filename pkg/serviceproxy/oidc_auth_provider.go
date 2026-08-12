package serviceproxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
)

const oidcAuthProviderID authProviderID = "oidc"

var oidcAuthProviderMetadata = authProviderMetadata{
	id:                   oidcAuthProviderID,
	displayName:          "oidc",
	authenticationTarget: "the configured OIDC issuer",
}

type oidcAuthProvider struct {
	authenticator.Token
	impersonateUser impersonateUserFunc
}

func (*oidcAuthProvider) Metadata() authProviderMetadata {
	return oidcAuthProviderMetadata
}

func (p *oidcAuthProvider) ApplyIdentity(ctx context.Context, req *http.Request, info user.Info) error {
	return applyExternalIdentity(ctx, req, info, p.impersonateUser)
}

type oidcAuthProviderFactory struct {
	options oidcOptions
}

func newOIDCAuthProviderFactory() *oidcAuthProviderFactory {
	return &oidcAuthProviderFactory{
		options: oidcOptions{
			usernameClaim:        "sub",
			reservedNamePrefixes: []string{"system:"},
			signingAlgs:          []string{"RS256"},
		},
	}
}

func (f *oidcAuthProviderFactory) addFlags(flags *pflag.FlagSet) {
	flags.StringVar(&f.options.issuerURL, "oidc-issuer-url", f.options.issuerURL, "The URL of the OIDC issuer, only the https scheme is accepted. Setting this enables OIDC token authentication after the managed cluster TokenReview and, when enabled, the hub TokenReview.")
	flags.StringVar(&f.options.clientID, "oidc-client-id", f.options.clientID, "The client ID that OIDC ID tokens must be issued for. Must be set together with --oidc-issuer-url.")
	flags.StringVar(&f.options.usernameClaim, "oidc-username-claim", f.options.usernameClaim, "The OIDC claim to use as the username.")
	flags.StringVar(&f.options.usernamePrefix, "oidc-username-prefix", f.options.usernamePrefix, "The prefix prepended to username claims. If unset, non-email claims use '<issuer-url>#'; use '-' to disable prefixing.")
	flags.StringVar(&f.options.groupsClaim, "oidc-groups-claim", f.options.groupsClaim, "The OIDC claim to use as the user's groups. The claim value is expected to be a string or an array of strings.")
	flags.StringVar(&f.options.groupsPrefix, "oidc-groups-prefix", f.options.groupsPrefix, "The prefix prepended to group claims to prevent clashes with existing groups.")
	flags.StringSliceVar(&f.options.reservedNamePrefixes, "oidc-reserved-name-prefixes", f.options.reservedNamePrefixes, "Comma-separated list of prefixes that authenticated OIDC usernames and groups must not use. The list replaces the default; set an empty value to disable the check.")
	flags.StringVar(&f.options.caConfigMap, "oidc-ca-configmap", f.options.caConfigMap, "The name of a ConfigMap in POD_NAMESPACE containing the OIDC issuer CA under ca.crt. The ConfigMap is watched and reconciled when it changes. Defaults to the host's root CAs.")
	flags.StringSliceVar(&f.options.signingAlgs, "oidc-signing-algs", f.options.signingAlgs, "Comma-separated list of allowed JOSE asymmetric signing algorithms for OIDC tokens.")
	flags.Var(cliflag.NewMapStringStringNoSplit(&f.options.requiredClaims), "oidc-required-claim", "A key=value pair that must be present in the OIDC ID token. Repeat this flag to require multiple claims.")
}

func (f *oidcAuthProviderFactory) validate() error {
	return validateOIDCOptions(f.options)
}

func (f *oidcAuthProviderFactory) enabled() bool {
	return f.options.issuerURL != ""
}

func (*oidcAuthProviderFactory) configFiles() []string {
	return nil
}

func (f *oidcAuthProviderFactory) build(ctx context.Context, dependencies authProviderDependencies) (authProvider, error) {
	authn, err := newOIDCAuthenticator(ctx, f.options)
	if err != nil {
		return nil, err
	}
	if f.options.caConfigMap != "" {
		if err := startOIDCCAConfigMapController(
			ctx,
			dependencies.managedClusterKubeClient,
			dependencies.podNamespace,
			f.options.caConfigMap,
			authn,
		); err != nil {
			return nil, fmt.Errorf("failed to start OIDC CA ConfigMap controller: %w", err)
		}
	}

	return &oidcAuthProvider{
		Token:           authn,
		impersonateUser: dependencies.impersonateUser,
	}, nil
}

func (f *oidcAuthProviderFactory) logConfiguration() {
	klog.Infof("OIDC authentication enabled: issuer=%s, clientID=%s, usernameClaim=%s", f.options.issuerURL, f.options.clientID, f.options.usernameClaim)
}

var (
	_ authProvider        = (*oidcAuthProvider)(nil)
	_ authProviderFactory = (*oidcAuthProviderFactory)(nil)
)
