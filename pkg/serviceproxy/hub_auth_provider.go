package serviceproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const hubAuthProviderID authProviderID = "hub"

var hubAuthProviderMetadata = authProviderMetadata{
	id:                   hubAuthProviderID,
	displayName:          "hub",
	authenticationTarget: "hub cluster",
}

type hubAuthProvider struct {
	authenticator.Token
	impersonateUser impersonateUserFunc
}

func (*hubAuthProvider) Metadata() authProviderMetadata {
	return hubAuthProviderMetadata
}

func (p *hubAuthProvider) ApplyIdentity(ctx context.Context, req *http.Request, hubUser user.Info) error {
	username := hubUser.GetName()
	if strings.HasPrefix(username, "system:serviceaccount:") {
		username = fmt.Sprintf("cluster:hub:%s", username)
	}
	return p.impersonateUser(ctx, req, username, hubUser.GetGroups())
}

type hubAuthProviderFactory struct {
	enableImpersonation bool
	kubeConfig          string
}

func newHubAuthProviderFactory() *hubAuthProviderFactory {
	return &hubAuthProviderFactory{enableImpersonation: true}
}

func (f *hubAuthProviderFactory) addFlags(flags *pflag.FlagSet) {
	flags.StringVar(&f.kubeConfig, "hub-kubeconfig", f.kubeConfig, "The kubeconfig file for connecting to the hub cluster")
	flags.BoolVar(&f.enableImpersonation, "enable-impersonation", f.enableImpersonation, "Whether to enable impersonation")
}

func (*hubAuthProviderFactory) validate() error {
	return nil
}

func (f *hubAuthProviderFactory) enabled() bool {
	return f.enableImpersonation
}

func (f *hubAuthProviderFactory) configFiles() []string {
	if !f.enabled() {
		return nil
	}
	return []string{f.kubeConfig}
}

func (f *hubAuthProviderFactory) build(_ context.Context, dependencies authProviderDependencies) (authProvider, error) {
	hubConfig, err := clientcmd.BuildConfigFromFlags("", f.kubeConfig)
	if err != nil {
		return nil, err
	}
	hubConfig.QPS = dependencies.kubeClientQPS
	hubConfig.Burst = dependencies.kubeClientBurst

	hubKubeClient, err := kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return nil, err
	}

	return &hubAuthProvider{
		Token: newTokenReviewAuthenticator(
			hubKubeClient,
			hubAuthProviderMetadata.displayName,
			dependencies.tokenReviewCacheTTL,
		),
		impersonateUser: dependencies.impersonateUser,
	}, nil
}

var (
	_ authProvider        = (*hubAuthProvider)(nil)
	_ authProviderFactory = (*hubAuthProviderFactory)(nil)
)
