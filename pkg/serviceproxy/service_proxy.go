package serviceproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/token/cache"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

	tokenReviewCacheTTL time.Duration
	kubeClientQPS       float32
	kubeClientBurst     int

	hubKubeConfig            string
	hubKubeClient            kubernetes.Interface
	managedClusterKubeClient kubernetes.Interface

	enableImpersonation bool

	managedClusterAuthenticator authenticator.Token
	hubAuthenticator            authenticator.Token

	// getImpersonateTokenFunc reads the service account token used for impersonation.
	// Defaults to reading from the mounted service account token file.
	// Can be overridden in tests.
	getImpersonateTokenFunc func() (string, error)
}

func newServiceProxy() *serviceProxy {
	s := &serviceProxy{
		tokenReviewCacheTTL: defaultTokenReviewCacheTTL,
		kubeClientQPS:       defaultKubeClientQPS,
		kubeClientBurst:     defaultKubeClientBurst,
	}
	s.getImpersonateTokenFunc = s.readImpersonateTokenFromFile
	return s
}

func (s *serviceProxy) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&s.cert, "cert", s.cert, "The path to the certificate of the service proxy server")
	flags.StringVar(&s.key, "key", s.key, "The path to the key of the service proxy server")
	flags.StringVar(&s.additionalServiceCA, "additional-service-ca", s.additionalServiceCA, "The path to the additional CA certificate for services")

	// hubKubeConfig is the kubeconfig file for connecting to the hub cluster
	flags.StringVar(&s.hubKubeConfig, "hub-kubeconfig", "", "The kubeconfig file for connecting to the hub cluster")

	// proxy related flags
	flags.IntVar(&s.maxIdleConns, "max-idle-conns", 100, "The maximum number of idle (keep-alive) connections across all hosts.")
	flags.DurationVar(&s.idleConnTimeout, "idle-conn-timeout", 90*time.Second, "The maximum amount of time an idle (keep-alive) connection will remain idle before closing itself.")
	flags.DurationVar(&s.tLSHandshakeTimeout, "tls-handshake-timeout", 10*time.Second, "The maximum amount of time waiting to wait for a TLS handshake.")
	flags.DurationVar(&s.expectContinueTimeout, "expect-continue-timeout", 1*time.Second, "The amount of time to wait for a server's first response headers after fully writing the request headers if the request has an \"Expect: 100-continue\" header.")
	flags.BoolVar(&s.enableImpersonation, "enable-impersonation", true, "Whether to enable impersonation")

	// token review cache flags
	flags.DurationVar(&s.tokenReviewCacheTTL, "token-review-cache-ttl", defaultTokenReviewCacheTTL, "TTL for cached TokenReview results. Set to 0 to disable caching.")

	// kube client rate limiting flags
	flags.Float32Var(&s.kubeClientQPS, "kube-api-qps", defaultKubeClientQPS, "QPS for kube API clients (managed cluster and hub). Increase if client-side throttling is observed under high concurrency.")
	flags.IntVar(&s.kubeClientBurst, "kube-api-burst", defaultKubeClientBurst, "Burst for kube API clients (managed cluster and hub).")
}

func (s *serviceProxy) Run(ctx context.Context) error {
	const (
		rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	var err error
	customChecks := []healthz.Checker{}

	cc, err := addonutils.NewConfigChecker("cert", s.cert, s.key, rootCAFile, s.hubKubeConfig)
	if err != nil {
		return err
	}
	customChecks = append(customChecks, cc.Check)

	if err := s.validate(); err != nil {
		return err
	}

	// get root CAs
	s.rootCAs = x509.NewCertPool()
	// ca for accessing apiserver

	apiserverPem, err := os.ReadFile(rootCAFile)
	if err != nil {
		return err
	}
	s.rootCAs.AppendCertsFromPEM(apiserverPem)

	// ca for accessing additional services
	if s.additionalServiceCA != "" {
		additionalCAPem, err := os.ReadFile(s.additionalServiceCA)
		if err != nil {
			if os.IsNotExist(err) {
				klog.Infof("additional-service-ca file not found: %s", s.additionalServiceCA)
			} else {
				return err
			}
		} else {
			s.rootCAs.AppendCertsFromPEM(additionalCAPem)

			// add configchecker into http probes when additional-service-ca is provided
			cc, err := addonutils.NewConfigChecker("additional-service-ca", s.additionalServiceCA)
			if err != nil {
				return err
			}
			customChecks = append(customChecks, cc.Check)
		}
	}

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

	// get hubKubeConfig
	hubConfig, err := clientcmd.BuildConfigFromFlags("", s.hubKubeConfig)
	if err != nil {
		return err
	}
	hubConfig.QPS = s.kubeClientQPS
	hubConfig.Burst = s.kubeClientBurst
	s.hubKubeClient, err = kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return err
	}

	// initialize token authenticators with caching
	// The official k8s.io/apiserver token cache provides:
	// - singleflight: concurrent requests for the same token share one API call
	// - striped cache: high-concurrency cache with minimal lock contention
	// - HMAC-SHA256 key derivation: tokens are never stored in plaintext
	managedClusterAuthn := &tokenReviewAuthenticator{client: s.managedClusterKubeClient, name: "managed cluster"}
	hubAuthn := &tokenReviewAuthenticator{client: s.hubKubeClient, name: "hub"}

	if s.tokenReviewCacheTTL > 0 {
		// cacheErrs=false: don't cache API errors (network issues, etc.)
		// failureTTL=successTTL: cache unauthenticated results too, matching kube-apiserver
		// best practice (see k8s.io/apiserver/pkg/authentication/authenticatorfactory/delegating.go).
		// This is critical for impersonation mode where hub tokens always fail managed cluster
		// auth — without failure caching, each singleflight group completion triggers a new
		// API call, causing latency spikes under high concurrency.
		s.managedClusterAuthenticator = cache.New(managedClusterAuthn, false, s.tokenReviewCacheTTL, s.tokenReviewCacheTTL)
		s.hubAuthenticator = cache.New(hubAuthn, false, s.tokenReviewCacheTTL, s.tokenReviewCacheTTL)
		klog.Infof("TokenReview cache enabled with TTL %v", s.tokenReviewCacheTTL)
	} else {
		s.managedClusterAuthenticator = managedClusterAuthn
		s.hubAuthenticator = hubAuthn
		klog.Infof("TokenReview cache disabled")
	}

	podNamespace := os.Getenv("POD_NAMESPACE")
	if len(podNamespace) == 0 {
		klog.Fatalf("Pod namespace is empty, please set the ENV for POD_NAMESPACE")
	}

	sdkTLSConfig, err := sdktls.StartTLSConfigMapWatcher(ctx, s.managedClusterKubeClient, podNamespace, func() {
		klog.Info("TLS ConfigMap changed, restarting")
		os.Exit(0)
	})
	if err != nil {
		klog.Fatalf("failed to start TLS ConfigMap watcher: %v", err)
	}
	klog.Infof("TLS config loaded: minVersion=%s, ciphersuites=%s", sdktls.VersionToString(sdkTLSConfig.MinVersion),
		sdktls.CipherSuitesToString(sdkTLSConfig.CipherSuites))

	tlsConfig := &tls.Config{
		MinVersion:   sdkTLSConfig.MinVersion,
		CipherSuites: sdkTLSConfig.CipherSuites,
	}

	go func() {
		// Currently ServeHealthProbes uses HTTP so our tlsConfig is not needed, however passing through for
		// consistency and in case it's ever updated to use HTTPS in the future
		if err = utils.ServeHealthProbes(":8000", tlsConfig, customChecks...); err != nil {
			klog.Fatal(err)
		}
	}()

	httpserver := &http.Server{
		Addr:      fmt.Sprintf(":%d", constant.ServiceProxyPort),
		TLSConfig: tlsConfig,
		Handler:   s,
	}

	return httpserver.ListenAndServeTLS(s.cert, s.key)
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
		"enableImpersonation", s.enableImpersonation,
		"isKubeAPIServer", url.Host == "kubernetes.default.svc",
	)

	if url.Host == "kubernetes.default.svc" {
		if s.enableImpersonation {
			if err := s.processAuthentication(ctx, req); err != nil {
				logger.Error(err, "authentication failed")
				http.Error(wr, err.Error(), http.StatusUnauthorized)
				return
			}
		}
	}

	logger.V(6).Info("forwarding request to reverse proxy",
		"targetURL", url.String(),
	)

	proxy := httputil.NewSingleHostReverseProxy(url)
	proxy.Transport = &http.Transport{
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

	proxy.ServeHTTP(wr, req)
}

func (s *serviceProxy) validate() error {
	if s.cert == "" {
		return fmt.Errorf("cert is required")
	}
	if s.key == "" {
		return fmt.Errorf("key is required")
	}
	return nil
}

func (s *serviceProxy) readImpersonateTokenFromFile() (string, error) {
	// Read the latest token from the mounted file
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// processAuthentication handles the authentication flow for both managed cluster and hub users.
// It tries managed cluster TokenReview first; if unauthenticated, falls back to hub TokenReview.
func (s *serviceProxy) processAuthentication(ctx context.Context, req *http.Request) error {
	logger := klog.FromContext(ctx)
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")

	logger.V(6).Info("processing authentication for request",
		"tokenPresent", token != "",
		"tokenLength", len(token),
	)

	// try managed cluster authentication first
	managedClusterResp, managedClusterAuthenticated, err := s.managedClusterAuthenticator.AuthenticateToken(ctx, token)
	if err != nil {
		logger.Error(err, "managed cluster authentication failed")
		return fmt.Errorf("managed cluster authentication failed: %v", err)
	}

	if managedClusterAuthenticated {
		logger.V(4).Info("managed cluster authentication succeeded",
			"username", managedClusterResp.User.GetName(),
		)
	} else {
		logger.V(4).Info("managed cluster authentication result",
			"authenticated", false,
		)
	}

	if !managedClusterAuthenticated {
		// try hub authentication
		hubResp, hubAuthenticated, err := s.hubAuthenticator.AuthenticateToken(ctx, token)
		if err != nil {
			logger.Error(err, "hub cluster authentication failed")
			return fmt.Errorf("authentication failed: managed cluster auth: not authenticated, hub cluster auth error: %v", err)
		}
		logger.V(4).Info("hub cluster authentication result",
			"authenticated", hubAuthenticated,
		)

		if !hubAuthenticated {
			logger.Error(nil, "authentication failed: token is neither valid for managed cluster nor hub cluster")
			return fmt.Errorf("authentication failed: token is neither valid for managed cluster nor hub cluster")
		}

		if err := s.processHubUser(ctx, req, hubResp.User); err != nil {
			logger.Error(err, "failed to process hub user")
			return fmt.Errorf("failed to process hub user: %v", err)
		}

		logger.V(6).Info("hub user processed successfully, impersonation headers applied")
	}

	return nil
}

// processHubUser handles the hub user specific operations including impersonation
func (s *serviceProxy) processHubUser(ctx context.Context, req *http.Request, hubUser user.Info) error {
	logger := klog.FromContext(ctx)

	// set impersonate group header
	for _, group := range hubUser.GetGroups() {
		// Here using `Add` instead of `Set` to support multiple groups
		req.Header.Add("Impersonate-Group", group)
	}

	// check if the hub user is serviceaccount kind, if so, add "cluster:hub:" prefix to the username
	username := hubUser.GetName()
	if strings.HasPrefix(username, "system:serviceaccount:") {
		req.Header.Set("Impersonate-User", fmt.Sprintf("cluster:hub:%s", username))
	} else {
		req.Header.Set("Impersonate-User", username)
	}

	logger.V(4).Info("impersonation headers set for hub user",
		"impersonateUser", req.Header.Get("Impersonate-User"),
		"impersonateGroups", hubUser.GetGroups(),
		"isServiceAccount", strings.HasPrefix(username, "system:serviceaccount:"),
	)

	// replace the original token with cluster-proxy service-account token which has impersonate permission
	token, err := s.getImpersonateTokenFunc()
	if err != nil {
		return fmt.Errorf("failed to get impersonate token: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	logger.V(6).Info("original bearer token replaced with service account impersonation token")

	return nil
}
