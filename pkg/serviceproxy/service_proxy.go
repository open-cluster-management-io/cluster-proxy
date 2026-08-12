package serviceproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/healthz"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

func NewServiceProxyCommand() *cobra.Command {
	serviceProxyServer := newServiceProxy()

	cmd := &cobra.Command{
		Use:   "service-proxy",
		Short: "service-proxy",
		Long:  `A http proxy server, receives http requests from proxy-agent and forwards to the target service.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serviceProxyServer.Run(cmd.Context())
		},
	}

	serviceProxyServer.AddFlags(cmd)
	return cmd
}

const (
	// defaultTokenReviewCacheTTL is the default TTL for cached TokenReview results.
	// Cached entries expire after this duration, forcing a fresh TokenReview API call.
	// A short TTL (10s) is sufficient because the primary goal is deduplicating
	// concurrent requests for the same token, not long-term caching.
	defaultTokenReviewCacheTTL = 10 * time.Second

	// defaultKubeClientQPS is the default QPS for kube clients used by service-proxy.
	// The default client-go value (5) is too low for high-concurrency TokenReview workloads,
	// causing client-side throttling delays of 1min+ when many requests are proxied simultaneously.
	defaultKubeClientQPS = 50.0

	// defaultKubeClientBurst is the default burst for kube clients used by service-proxy.
	defaultKubeClientBurst = 100
)

type serviceProxy struct {
	cert, key           string
	additionalServiceCA string
	rootCAs             *x509.CertPool

	maxIdleConns          int
	idleConnTimeout       time.Duration
	tLSHandshakeTimeout   time.Duration
	expectContinueTimeout time.Duration
	drain                 utils.DrainConfig

	tokenReviewCacheTTL time.Duration
	kubeClientQPS       float32
	kubeClientBurst     int
	podNamespace        string

	managedClusterKubeClient kubernetes.Interface

	authProviderFactories []authProviderFactory
	authProviders         []authProvider

	proxyTransport closeIdleRoundTripper

	// getImpersonateTokenFunc reads the service account token used for impersonation.
	// Defaults to reading from the mounted service account token file.
	// Can be overridden in tests.
	getImpersonateTokenFunc func() (string, error)
}

type closeIdleRoundTripper interface {
	http.RoundTripper
	CloseIdleConnections()
}

func newServiceProxy() *serviceProxy {
	s := &serviceProxy{
		tokenReviewCacheTTL:   defaultTokenReviewCacheTTL,
		kubeClientQPS:         defaultKubeClientQPS,
		kubeClientBurst:       defaultKubeClientBurst,
		authProviderFactories: defaultAuthProviderFactories(),
	}
	s.getImpersonateTokenFunc = s.readImpersonateTokenFromFile
	return s
}

func (s *serviceProxy) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&s.cert, "cert", s.cert, "The path to the certificate of the service proxy server")
	flags.StringVar(&s.key, "key", s.key, "The path to the key of the service proxy server")
	flags.StringVar(&s.additionalServiceCA, "additional-service-ca", s.additionalServiceCA, "The path to the additional CA certificate for services")

	// proxy related flags
	flags.IntVar(&s.maxIdleConns, "max-idle-conns", 100, "The maximum number of idle (keep-alive) connections across all hosts.")
	flags.DurationVar(&s.idleConnTimeout, "idle-conn-timeout", 90*time.Second, "The maximum amount of time an idle (keep-alive) connection will remain idle before closing itself.")
	flags.DurationVar(&s.tLSHandshakeTimeout, "tls-handshake-timeout", 10*time.Second, "The maximum amount of time waiting to wait for a TLS handshake.")
	flags.DurationVar(&s.expectContinueTimeout, "expect-continue-timeout", 1*time.Second, "The amount of time to wait for a server's first response headers after fully writing the request headers if the request has an \"Expect: 100-continue\" header.")
	s.drain.AddFlags(flags)

	// token review cache flags
	flags.DurationVar(&s.tokenReviewCacheTTL, "token-review-cache-ttl", defaultTokenReviewCacheTTL, "TTL for cached TokenReview results. Set to 0 to disable caching.")

	for _, factory := range s.authProviderFactories {
		factory.addFlags(flags)
	}

	// kube client rate limiting flags
	flags.Float32Var(&s.kubeClientQPS, "kube-api-qps", defaultKubeClientQPS, "QPS for Kubernetes API clients. Increase if client-side throttling is observed under high concurrency.")
	flags.IntVar(&s.kubeClientBurst, "kube-api-burst", defaultKubeClientBurst, "Burst for Kubernetes API clients.")
}

func (s *serviceProxy) Run(ctx context.Context) error {
	const (
		rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var err error
	customChecks := []healthz.Checker{}
	providerConfigFiles := s.authProviderConfigFiles()
	configFiles := make([]string, 0, 3+len(providerConfigFiles))
	configFiles = append(configFiles, s.cert, s.key, rootCAFile)
	configFiles = append(configFiles, providerConfigFiles...)
	cc, err := addonutils.NewConfigChecker("cert", configFiles...)
	if err != nil {
		return err
	}
	customChecks = append(customChecks, cc.Check)

	if err := s.validate(); err != nil {
		return err
	}

	rootCAs, additionalCALoaded, err := loadRootCAs(rootCAFile, s.additionalServiceCA)
	if err != nil {
		return err
	}
	s.rootCAs = rootCAs
	if additionalCALoaded {
		// add configchecker into http probes when additional-service-ca is provided
		cc, err := addonutils.NewConfigChecker("additional-service-ca", s.additionalServiceCA)
		if err != nil {
			return err
		}
		customChecks = append(customChecks, cc.Check)
	}

	s.proxyTransport = s.newProxyTransport()
	defer s.closeIdleConnections()

	// init managedClusterKubeClient
	// managedClusterKubeClient is the kubeClient of current cluster using in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %v", err)
	}
	config.QPS = s.kubeClientQPS
	config.Burst = s.kubeClientBurst

	s.managedClusterKubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	s.podNamespace = os.Getenv("POD_NAMESPACE")
	if len(s.podNamespace) == 0 {
		return errors.New("pod namespace is empty, please set the POD_NAMESPACE environment variable")
	}

	if err := s.initializeAuthProviders(runCtx); err != nil {
		return err
	}

	sdkTLSConfig, err := sdktls.StartTLSConfigMapWatcher(runCtx, s.managedClusterKubeClient, s.podNamespace, func() {
		klog.Info("TLS ConfigMap changed, shutting down gracefully for restart")
		cancel()
	})
	if err != nil {
		return fmt.Errorf("failed to start TLS ConfigMap watcher: %w", err)
	}
	klog.Infof("TLS config loaded: minVersion=%s, ciphersuites=%s", sdktls.VersionToString(sdkTLSConfig.MinVersion),
		sdktls.CipherSuitesToString(sdkTLSConfig.CipherSuites))

	tlsConfig := &tls.Config{
		MinVersion:   sdkTLSConfig.MinVersion,
		CipherSuites: sdkTLSConfig.CipherSuites,
	}

	healthServer := utils.NewHealthProbeServer(":8000", customChecks...)
	publicServer := utils.NewProxyHTTPServer(fmt.Sprintf(":%d", constant.ServiceProxyPort), tlsConfig, s)

	klog.Infof("starting service proxy HTTPS server on %d and health server on 8000", constant.ServiceProxyPort)
	return utils.RunHTTPServers(
		runCtx,
		s.drain,
		publicServer,
		s.cert,
		s.key,
		healthServer,
	)
}

// loadRootCAs builds the root CA pool from the apiserver CA and, when
// configured and present, the additional service CA. The returned bool
// reports whether the additional service CA was loaded.
func loadRootCAs(rootCAFile, additionalServiceCA string) (*x509.CertPool, bool, error) {
	// ca for accessing apiserver
	rootCAs, err := certutil.NewPool(rootCAFile)
	if err != nil {
		return nil, false, err
	}

	// ca for accessing additional services
	if additionalServiceCA == "" {
		return rootCAs, false, nil
	}
	additionalCAPem, err := os.ReadFile(additionalServiceCA)
	if os.IsNotExist(err) {
		klog.Infof("additional-service-ca file not found: %s", additionalServiceCA)
		return rootCAs, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	additionalCerts, err := certutil.ParseCertsPEM(additionalCAPem)
	if err != nil {
		return nil, false, fmt.Errorf("failed to parse additional service CA %s: %w", additionalServiceCA, err)
	}
	for _, cert := range additionalCerts {
		rootCAs.AddCert(cert)
	}
	return rootCAs, true, nil
}

func (s *serviceProxy) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := klog.FromContext(ctx)

	if klog.V(4).Enabled() {
		dump, err := httputil.DumpRequest(req, true)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		klog.V(4).Infof("request:\n %s", string(dump))
	}

	url, err := utils.GetTargetServiceURLFromRequest(req)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		logger.Error(err, "failed to get target service url from request")
		return
	}

	// Enrich logger with request-scoped fields so all downstream logs
	// are traceable by request without repeating these values.
	logger = logger.WithValues(
		"targetHost", url.Host,
		"method", req.Method,
		"path", req.URL.Path,
	)
	ctx = klog.NewContext(ctx, logger)

	logger.V(4).Info("service proxy received request",
		"targetScheme", url.Scheme,
		"authProviders", s.activeAuthProviderIDs(),
		"isKubeAPIServer", url.Host == utils.KubeAPIServerHost,
	)

	if url.Host == utils.KubeAPIServerHost {
		clientImpersonationRequested := hasClientImpersonationHeaders(req.Header)
		// Delegate client impersonation unchanged to the target API server, which authenticates
		// the original token and authorizes the requested impersonation through its own RBAC.
		if len(s.authProviders) > 0 && !clientImpersonationRequested {
			if err := s.processAuthentication(ctx, req); err != nil {
				logger.Error(err, "authentication failed")
				http.Error(wr, err.Error(), http.StatusUnauthorized)
				return
			}
		} else {
			logger.V(4).Info("skipping proxy-side authentication",
				"clientImpersonationRequested", clientImpersonationRequested,
			)
		}
	}

	logger.V(6).Info("forwarding request to reverse proxy",
		"targetURL", url.String(),
	)

	if s.proxyTransport == nil {
		err := errors.New("service proxy transport is not initialized")
		logger.Error(err, "cannot forward request")
		http.Error(wr, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(url)
	proxy.Transport = s.proxyTransport
	proxy.ServeHTTP(wr, req)
}

func (s *serviceProxy) newProxyTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          s.maxIdleConns,
		IdleConnTimeout:       s.idleConnTimeout,
		TLSHandshakeTimeout:   s.tLSHandshakeTimeout,
		ExpectContinueTimeout: s.expectContinueTimeout,
		// Not using our global TLSConfig for outbound will rely on server settings
		TLSClientConfig: &tls.Config{
			RootCAs:    s.rootCAs,
			MinVersion: tls.VersionTLS12,
		},
		// golang http pkg automatically upgrade http connection to http2 connection, but http2 can not upgrade to SPDY which used in "kubectl exec".
		// set ForceAttemptHTTP2 = false to prevent auto http2 upgration
		ForceAttemptHTTP2: false,
	}
}

func (s *serviceProxy) closeIdleConnections() {
	if s.proxyTransport != nil {
		s.proxyTransport.CloseIdleConnections()
	}
}

func (s *serviceProxy) validate() error {
	if err := s.drain.Validate(); err != nil {
		return err
	}
	if s.cert == "" {
		return fmt.Errorf("cert is required")
	}
	if s.key == "" {
		return fmt.Errorf("key is required")
	}
	for _, factory := range s.authProviderFactories {
		if err := factory.validate(); err != nil {
			return err
		}
	}
	return nil
}
