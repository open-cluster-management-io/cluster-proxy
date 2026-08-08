package serviceproxy

import (
	"context"
	"net/http"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type authProvider interface {
	authenticator.Token

	Metadata() authProviderMetadata
	ApplyIdentity(context.Context, *http.Request, user.Info) error
}

type authProviderID string

type authProviderMetadata struct {
	id                      authProviderID
	displayName             string
	authenticationTarget    string
	standaloneFailureTarget string
}

func (m authProviderMetadata) standaloneTarget() string {
	if m.standaloneFailureTarget != "" {
		return m.standaloneFailureTarget
	}
	return m.authenticationTarget
}

type authProviderFactory interface {
	addFlags(*pflag.FlagSet)
	validate() error
	enabled() bool
	// configFiles returns files consumed by the provider when it is enabled.
	// Disabled providers return no files.
	configFiles() []string
	build(context.Context, authProviderDependencies) (authProvider, error)
}

type authProviderFactoryLogger interface {
	logConfiguration()
}

type authProviderDependencies struct {
	managedClusterKubeClient kubernetes.Interface
	podNamespace             string
	kubeClientQPS            float32
	kubeClientBurst          int
	tokenReviewCacheTTL      time.Duration
	impersonateUser          impersonateUserFunc
}

func defaultAuthProviderFactories() []authProviderFactory {
	return []authProviderFactory{
		newHubAuthProviderFactory(),
		newOIDCAuthProviderFactory(),
	}
}

func (s *serviceProxy) authProviderDependencies(podNamespace string) authProviderDependencies {
	return authProviderDependencies{
		managedClusterKubeClient: s.managedClusterKubeClient,
		podNamespace:             podNamespace,
		kubeClientQPS:            s.kubeClientQPS,
		kubeClientBurst:          s.kubeClientBurst,
		tokenReviewCacheTTL:      s.tokenReviewCacheTTL,
		impersonateUser:          s.impersonateUser,
	}
}

func (s *serviceProxy) initializeAuthProviders(ctx context.Context, podNamespace string) error {
	enabledFactories := make([]authProviderFactory, 0, len(s.authProviderFactories))

	for _, factory := range s.authProviderFactories {
		if factory.enabled() {
			enabledFactories = append(enabledFactories, factory)
		}
	}

	if len(enabledFactories) == 0 {
		s.authProviders = nil
		return nil
	}

	dependencies := s.authProviderDependencies(podNamespace)
	providers := make([]authProvider, 0, len(enabledFactories)+1)
	providers = append(providers, newManagedClusterAuthProvider(dependencies))

	for _, factory := range enabledFactories {
		provider, err := factory.build(ctx, dependencies)
		if err != nil {
			return err
		}
		providers = append(providers, provider)
	}

	if dependencies.tokenReviewCacheTTL > 0 {
		klog.Infof("TokenReview cache enabled with TTL %v", dependencies.tokenReviewCacheTTL)
	} else {
		klog.Infof("TokenReview cache disabled")
	}
	for _, factory := range enabledFactories {
		if logger, ok := factory.(authProviderFactoryLogger); ok {
			logger.logConfiguration()
		}
	}

	s.authProviders = providers
	return nil
}

func (s *serviceProxy) activeAuthProviderIDs() []authProviderID {
	ids := make([]authProviderID, 0, len(s.authProviders))
	for _, provider := range s.authProviders {
		ids = append(ids, provider.Metadata().id)
	}
	return ids
}

func (s *serviceProxy) authProviderConfigFiles() []string {
	var configFiles []string
	for _, factory := range s.authProviderFactories {
		configFiles = append(configFiles, factory.configFiles()...)
	}
	return configFiles
}
