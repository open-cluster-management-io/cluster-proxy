package serviceproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	apiserver "k8s.io/apiserver/pkg/apis/apiserver"
	apiservervalidation "k8s.io/apiserver/pkg/apis/apiserver/validation"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	authenticationcel "k8s.io/apiserver/pkg/authentication/cel"
	oidcauthenticator "k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/utils/ptr"
)

// oidcHTTPTimeout bounds every HTTP call to the OIDC issuer (discovery,
// distributed claims, and JWKS fetches).
const oidcHTTPTimeout = 15 * time.Second

type oidcOptions struct {
	issuerURL            string
	clientID             string
	usernameClaim        string
	usernamePrefix       string
	groupsClaim          string
	groupsPrefix         string
	caFile               string
	signingAlgs          []string
	requiredClaims       map[string]string
	reservedNamePrefixes []string
}

// oidcAuthenticator lazily creates Kubernetes' OIDC authenticator on first use,
// so an issuer or CA file that is not ready at pod startup does not fail startup.
type oidcAuthenticator struct {
	lifecycleCtx context.Context
	opts         oidcOptions

	mu       sync.Mutex
	delegate oidcauthenticator.AuthenticatorTokenWithHealthCheck
}

func newOIDCAuthenticator(lifecycleCtx context.Context, opts oidcOptions) *oidcAuthenticator {
	return &oidcAuthenticator{lifecycleCtx: lifecycleCtx, opts: opts}
}

// effectiveUsernamePrefix implements the legacy kube-apiserver flag behavior:
// non-email claims are namespaced by the issuer unless an explicit prefix is
// provided, and "-" explicitly disables prefixing.
func effectiveUsernamePrefix(opts oidcOptions) string {
	prefix := opts.usernamePrefix
	if prefix == "" && opts.usernameClaim != "email" {
		prefix = opts.issuerURL + "#"
	}
	if prefix == "-" {
		prefix = ""
	}
	return prefix
}

func buildJWTAuthenticatorConfig(opts oidcOptions) apiserver.JWTAuthenticator {
	config := apiserver.JWTAuthenticator{
		Issuer: apiserver.Issuer{
			URL:       opts.issuerURL,
			Audiences: []string{opts.clientID},
		},
		ClaimMappings: apiserver.ClaimMappings{
			Username: apiserver.PrefixedClaimOrExpression{
				Claim:  opts.usernameClaim,
				Prefix: ptr.To(effectiveUsernamePrefix(opts)),
			},
		},
	}

	// each reserved prefix becomes CEL rules over the final (already prefixed)
	// username and groups; %q keeps the prefix a literal inside the expression
	for _, prefix := range opts.reservedNamePrefixes {
		config.UserValidationRules = append(config.UserValidationRules,
			apiserver.UserValidationRule{
				Expression: fmt.Sprintf("!user.username.startsWith(%q)", prefix),
				Message:    fmt.Sprintf("username cannot use the reserved %s prefix", prefix),
			},
			apiserver.UserValidationRule{
				Expression: fmt.Sprintf("user.groups.all(group, !group.startsWith(%q))", prefix),
				Message:    fmt.Sprintf("groups cannot use the reserved %s prefix", prefix),
			},
		)
	}

	if opts.groupsClaim != "" {
		config.ClaimMappings.Groups = apiserver.PrefixedClaimOrExpression{
			Claim:  opts.groupsClaim,
			Prefix: ptr.To(opts.groupsPrefix),
		}
	}

	for _, claim := range slices.Sorted(maps.Keys(opts.requiredClaims)) {
		config.ClaimValidationRules = append(config.ClaimValidationRules, apiserver.ClaimValidationRule{
			Claim:         claim,
			RequiredValue: opts.requiredClaims[claim],
		})
	}

	return config
}

func validateOIDCOptions(opts oidcOptions) error {
	if (opts.issuerURL == "") != (opts.clientID == "") {
		return fmt.Errorf("--oidc-issuer-url and --oidc-client-id must be specified together")
	}
	// matching kube-apiserver's oidc flag validation, the remaining oidc flags
	// are ignored when the issuer is unset
	if opts.issuerURL == "" {
		return nil
	}

	// an empty prefix matches every name and would silently reject all tokens
	if slices.Contains(opts.reservedNamePrefixes, "") {
		return fmt.Errorf("--oidc-reserved-name-prefixes must not contain an empty prefix")
	}

	config := buildJWTAuthenticatorConfig(opts)
	_, fieldErrs := apiservervalidation.CompileAndValidateJWTAuthenticator(
		authenticationcel.NewDefaultCompiler(), config, nil,
	)
	if err := fieldErrs.ToAggregate(); err != nil {
		return fmt.Errorf("invalid OIDC configuration: %v", err)
	}

	validSigningAlgs := oidcauthenticator.AllValidSigningAlgorithms()
	for _, alg := range opts.signingAlgs {
		if !slices.Contains(validSigningAlgs, alg) {
			return fmt.Errorf("unsupported OIDC signing algorithm %q", alg)
		}
	}

	return nil
}

func (a *oidcAuthenticator) getDelegate() (oidcauthenticator.AuthenticatorTokenWithHealthCheck, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.delegate != nil {
		return a.delegate, nil
	}

	transport := utilnet.SetTransportDefaults(&http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	})
	if a.opts.caFile != "" {
		pool, err := certutil.NewPool(a.opts.caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read oidc CA file: %v", err)
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	delegate, err := oidcauthenticator.New(a.lifecycleCtx, oidcauthenticator.Options{
		Client:               &http.Client{Timeout: oidcHTTPTimeout, Transport: transport},
		SupportedSigningAlgs: a.opts.signingAlgs,
		JWTAuthenticator:     buildJWTAuthenticatorConfig(a.opts),
		APIServerID:          "cluster-proxy-service-proxy",
	})
	if err != nil {
		return nil, err
	}

	a.delegate = delegate
	return delegate, nil
}

// AuthenticateToken delegates OIDC verification and claim mapping to the
// Kubernetes authenticator, reporting provider health failures as
// infrastructure errors and token failures as ErrTokenNotAuthenticated.
func (a *oidcAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	delegate, err := a.getDelegate()
	if err != nil {
		return nil, false, err
	}

	// Kubernetes stores the verifier before clearing the health error. Snapshot
	// health first so initialization completing during authentication cannot turn
	// a transient startup failure into a definitive token rejection.
	healthErr := delegate.HealthCheck()
	resp, authenticated, err := delegate.AuthenticateToken(ctx, token)
	if err == nil {
		return resp, authenticated, nil
	}
	if healthErr != nil {
		return nil, false, healthErr
	}
	return nil, false, fmt.Errorf("%v: %w", err, ErrTokenNotAuthenticated)
}
