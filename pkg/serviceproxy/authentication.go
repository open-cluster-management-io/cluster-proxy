package serviceproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/klog/v2"
)

func (s *serviceProxy) readImpersonateTokenFromFile() (string, error) {
	// Read the latest token from the mounted file
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// impersonateHeaderPrefix is the common prefix of the impersonation headers declared by
// authenticationv1: Impersonate-User, Impersonate-Group, Impersonate-Uid and Impersonate-Extra-<key>.
const impersonateHeaderPrefix = "Impersonate-"

func isImpersonationHeader(key string) bool {
	return len(key) >= len(impersonateHeaderPrefix) &&
		strings.EqualFold(key[:len(impersonateHeaderPrefix)], impersonateHeaderPrefix)
}

// hasClientImpersonationHeaders reports whether the request contains a Kubernetes impersonation
// header. Matching the complete prefix case-insensitively also covers future impersonation headers.
func hasClientImpersonationHeaders(headers http.Header) bool {
	for key := range headers {
		if isImpersonationHeader(key) {
			return true
		}
	}
	return false
}

func deleteImpersonationHeaders(headers http.Header) {
	for key := range headers {
		if isImpersonationHeader(key) {
			delete(headers, key)
		}
	}
}

// processAuthentication tries each configured authentication provider in order.
// Infrastructure errors stop the flow; only an unauthenticated result falls
// through to the next provider.
func (s *serviceProxy) processAuthentication(ctx context.Context, req *http.Request) error {
	logger := klog.FromContext(ctx)
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")

	for _, provider := range s.authProviders {
		metadata := provider.Metadata()
		resp, authenticated, err := provider.AuthenticateToken(ctx, token)
		if err != nil {
			if errors.Is(err, ErrTokenNotAuthenticated) {
				authenticated = false
			} else {
				return fmt.Errorf("%s authentication failed: %v", metadata.displayName, err)
			}
		}

		logger.V(4).Info("authentication result",
			"provider", metadata.id,
			"authenticated", authenticated,
		)

		if !authenticated {
			continue
		}

		if err := provider.ApplyIdentity(ctx, req, resp.User); err != nil {
			return fmt.Errorf("failed to apply %s identity: %v", metadata.displayName, err)
		}
		return nil
	}

	return s.authenticationFailureError()
}

func (s *serviceProxy) authenticationFailureError() error {
	if len(s.authProviders) == 0 {
		return errors.New("authentication failed: token is not valid for the managed cluster")
	}

	metadata := make([]authProviderMetadata, 0, len(s.authProviders))
	for _, provider := range s.authProviders {
		metadata = append(metadata, provider.Metadata())
	}

	switch len(metadata) {
	case 1:
		return fmt.Errorf("authentication failed: token is not valid for %s", metadata[0].standaloneTarget())
	case 2:
		return fmt.Errorf("authentication failed: token is neither valid for %s nor %s", metadata[0].authenticationTarget, metadata[1].authenticationTarget)
	default:
		targets := make([]string, 0, len(metadata))
		for _, providerMetadata := range metadata {
			targets = append(targets, providerMetadata.authenticationTarget)
		}
		return fmt.Errorf("authentication failed: token is not valid for %s, or %s",
			strings.Join(targets[:len(targets)-1], ", "),
			targets[len(targets)-1],
		)
	}
}

type impersonateUserFunc func(context.Context, *http.Request, string, []string) error

// applyExternalIdentity carries an identity that is unknown to the managed cluster
// into the request via impersonation headers.
func applyExternalIdentity(ctx context.Context, req *http.Request, externalUser user.Info, impersonateUser impersonateUserFunc) error {
	groups := slices.Clone(externalUser.GetGroups())
	if !slices.Contains(groups, user.AllAuthenticated) {
		groups = append(groups, user.AllAuthenticated)
	}
	return impersonateUser(ctx, req, externalUser.GetName(), groups)
}

// impersonateUser sets the impersonation headers for the given identity and
// replaces the original token with the cluster-proxy service-account token
// which has impersonate permission.
func (s *serviceProxy) impersonateUser(ctx context.Context, req *http.Request, username string, groups []string) error {
	logger := klog.FromContext(ctx)

	// Ensure no client-supplied impersonation values survive when applying the authenticated identity.
	deleteImpersonationHeaders(req.Header)
	for _, group := range groups {
		// Add (not Set) so multiple groups are preserved.
		req.Header.Add(authenticationv1.ImpersonateGroupHeader, group)
	}

	req.Header.Set(authenticationv1.ImpersonateUserHeader, username)

	logger.V(4).Info("impersonation headers set",
		"impersonateUser", username,
		"impersonateGroups", groups,
	)

	token, err := s.getImpersonateTokenFunc()
	if err != nil {
		return fmt.Errorf("failed to get impersonate token: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	logger.V(6).Info("original bearer token replaced with service account impersonation token")

	return nil
}
