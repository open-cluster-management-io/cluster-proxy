package serviceproxy

import (
	"context"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// tokenReviewAuthenticator implements authenticator.Token by calling the
// Kubernetes TokenReview API against a specific cluster.
type tokenReviewAuthenticator struct {
	client kubernetes.Interface
	name   string // cluster name for logging (e.g., "managed cluster", "hub")
}

// AuthenticateToken calls the TokenReview API and returns the result.
func (a *tokenReviewAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	logger := klog.FromContext(ctx)
	logger.V(6).Info("creating TokenReview", "cluster", a.name)

	tokenReview, err := a.client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, false, err
	}

	logger.V(6).Info("TokenReview completed",
		"cluster", a.name,
		"authenticated", tokenReview.Status.Authenticated,
		"username", tokenReview.Status.User.Username,
		"groups", tokenReview.Status.User.Groups,
	)

	if !tokenReview.Status.Authenticated {
		if tokenReview.Status.Error != "" {
			return nil, false, fmt.Errorf("%s TokenReview error: %s", a.name, tokenReview.Status.Error)
		}
		return nil, false, nil
	}

	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   tokenReview.Status.User.Username,
			UID:    tokenReview.Status.User.UID,
			Groups: tokenReview.Status.User.Groups,
			Extra:  convertExtra(tokenReview.Status.User.Extra),
		},
	}, true, nil
}

// convertExtra converts authenticationv1.ExtraValue (map[string]ExtraValue)
// to the format expected by user.Info (map[string][]string).
func convertExtra(extra map[string]authenticationv1.ExtraValue) map[string][]string {
	if extra == nil {
		return nil
	}
	result := make(map[string][]string, len(extra))
	for k, v := range extra {
		result[k] = []string(v)
	}
	return result
}
