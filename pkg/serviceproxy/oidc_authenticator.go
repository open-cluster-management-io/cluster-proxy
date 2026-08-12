package serviceproxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
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
	caConfigMap          string
	signingAlgs          []string
	requiredClaims       map[string]string
	reservedNamePrefixes []string
}

// An oidcDelegateFactory returns a delegate and a cleanup function that
// releases its transport resources; a failed factory has already released them.
type oidcDelegateFactory func(
	context.Context,
	[]byte,
) (oidcauthenticator.AuthenticatorTokenWithHealthCheck, func(), error)

// oidcAuthenticator atomically exposes the delegate produced for the latest
// observed OIDC CA configuration. Reconciliation, rather than request traffic,
// owns delegate initialization and replacement.
type oidcAuthenticator struct {
	lifecycleCtx context.Context
	opts         oidcOptions

	reconcileMu           sync.Mutex
	observedConfiguration string
	newDelegate           oidcDelegateFactory

	mu                  sync.RWMutex
	delegate            oidcauthenticator.AuthenticatorTokenWithHealthCheck
	delegateCancel      context.CancelFunc
	initializationError error
}

func newOIDCAuthenticator(lifecycleCtx context.Context, opts oidcOptions) (*oidcAuthenticator, error) {
	a := &oidcAuthenticator{
		lifecycleCtx:        lifecycleCtx,
		opts:                opts,
		initializationError: errors.New("oidc authenticator is not initialized"),
	}
	a.newDelegate = a.buildDelegate

	// ConfigMap-backed CA data is initialized by the informer controller after
	// its cache has synced. System-root configurations can start discovery now.
	if opts.caConfigMap != "" {
		return a, nil
	}

	if _, err := a.reconcileCABundle("system-roots", nil); err != nil {
		return nil, err
	}
	return a, nil
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

func (a *oidcAuthenticator) buildDelegate(
	lifecycleCtx context.Context,
	caBundle []byte,
) (oidcauthenticator.AuthenticatorTokenWithHealthCheck, func(), error) {
	transport := utilnet.SetTransportDefaults(&http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	})
	if caBundle != nil {
		pool, err := certutil.NewPoolFromBytes(caBundle)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load oidc CA bundle: %w", err)
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	delegate, err := oidcauthenticator.New(lifecycleCtx, oidcauthenticator.Options{
		Client:               &http.Client{Timeout: oidcHTTPTimeout, Transport: transport},
		SupportedSigningAlgs: a.opts.signingAlgs,
		JWTAuthenticator:     buildJWTAuthenticatorConfig(a.opts),
		APIServerID:          "cluster-proxy-service-proxy",
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, err
	}
	return delegate, transport.CloseIdleConnections, nil
}

func caConfigurationKey(source string, caBundle []byte) string {
	sum := sha256.Sum256(caBundle)
	return fmt.Sprintf("%s:%x", source, sum)
}

// reconcileCABundle applies one observed CA configuration exactly once. It
// disables the old delegate before constructing the replacement so an invalid
// security configuration fails closed.
func (a *oidcAuthenticator) reconcileCABundle(configurationKey string, caBundle []byte) (bool, error) {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()

	if a.observedConfiguration == configurationKey {
		return false, nil
	}
	a.observedConfiguration = configurationKey
	a.replaceDelegate(nil, nil, errors.New("oidc authenticator is initializing"))

	delegateCtx, cancel := context.WithCancel(a.lifecycleCtx)
	delegate, cleanup, err := a.newDelegate(delegateCtx, caBundle)
	if err != nil {
		cancel()
		initializationError := fmt.Errorf("failed to initialize OIDC authenticator: %w", err)
		a.replaceDelegate(nil, nil, initializationError)
		return true, initializationError
	}

	a.replaceDelegate(delegate, func() {
		cancel()
		if cleanup != nil {
			cleanup()
		}
	}, nil)
	return true, nil
}

// markUnavailable records a desired configuration that cannot produce an
// authenticator. Repeated informer events for the same state are no-ops.
func (a *oidcAuthenticator) markUnavailable(configurationKey string, err error) bool {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()

	if a.observedConfiguration == configurationKey {
		return false
	}
	a.observedConfiguration = configurationKey
	a.replaceDelegate(nil, nil, err)
	return true
}

func (a *oidcAuthenticator) replaceDelegate(
	delegate oidcauthenticator.AuthenticatorTokenWithHealthCheck,
	cancel context.CancelFunc,
	initializationError error,
) {
	a.mu.Lock()
	oldCancel := a.delegateCancel
	a.delegate = delegate
	a.delegateCancel = cancel
	a.initializationError = initializationError
	a.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
}

// AuthenticateToken delegates OIDC verification and claim mapping to the
// Kubernetes authenticator, reporting provider health failures as
// infrastructure errors and token failures as ErrTokenNotAuthenticated.
func (a *oidcAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	a.mu.RLock()
	delegate := a.delegate
	initializationError := a.initializationError
	a.mu.RUnlock()

	if delegate == nil {
		return nil, false, initializationError
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
