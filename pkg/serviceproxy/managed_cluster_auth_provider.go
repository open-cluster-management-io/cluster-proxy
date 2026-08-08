package serviceproxy

import (
	"context"
	"net/http"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
)

type managedClusterAuthProvider struct {
	authenticator.Token
}

const managedClusterAuthProviderID authProviderID = "managed-cluster"

var managedClusterAuthProviderMetadata = authProviderMetadata{
	id:                      managedClusterAuthProviderID,
	displayName:             "managed cluster",
	authenticationTarget:    "managed cluster",
	standaloneFailureTarget: "the managed cluster",
}

func (*managedClusterAuthProvider) Metadata() authProviderMetadata {
	return managedClusterAuthProviderMetadata
}

func (*managedClusterAuthProvider) ApplyIdentity(context.Context, *http.Request, user.Info) error {
	return nil
}

func newManagedClusterAuthProvider(dependencies authProviderDependencies) authProvider {
	return &managedClusterAuthProvider{
		Token: newTokenReviewAuthenticator(
			dependencies.managedClusterKubeClient,
			managedClusterAuthProviderMetadata.displayName,
			dependencies.tokenReviewCacheTTL,
		),
	}
}

var _ authProvider = (*managedClusterAuthProvider)(nil)
