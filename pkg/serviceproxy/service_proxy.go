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
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
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

type serviceProxy struct {
	cert, key    string
	ocpserviceCA string
	rootCAs      *x509.CertPool

	maxIdleConns          int
	idleConnTimeout       time.Duration
	tLSHandshakeTimeout   time.Duration
	expectContinueTimeout time.Duration

	hubKubeConfig            string
	hubKubeClient            kubernetes.Interface
	managedClusterKubeClient kubernetes.Interface
}

func newServiceProxy() *serviceProxy {
	return &serviceProxy{}
}

func (s *serviceProxy) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&s.cert, "cert", s.cert, "The path to the certificate of the service proxy server")
	flags.StringVar(&s.key, "key", s.key, "The path to the key of the service proxy server")
	flags.StringVar(&s.ocpserviceCA, "ocpservice-ca", s.ocpserviceCA, "The path to the CA certificate of the ocp services")

	// hubKubeConfig is the kubeconfig file for connecting to the hub cluster
	flags.StringVar(&s.hubKubeConfig, "hub-kubeconfig", "", "The kubeconfig file for connecting to the hub cluster")

	// proxy related flags
	flags.IntVar(&s.maxIdleConns, "max-idle-conns", 100, "The maximum number of idle (keep-alive) connections across all hosts.")
	flags.DurationVar(&s.idleConnTimeout, "idle-conn-timeout", 90*time.Second, "The maximum amount of time an idle (keep-alive) connection will remain idle before closing itself.")
	flags.DurationVar(&s.tLSHandshakeTimeout, "tls-handshake-timeout", 10*time.Second, "The maximum amount of time waiting to wait for a TLS handshake.")
	flags.DurationVar(&s.expectContinueTimeout, "expect-continue-timeout", 1*time.Second, "The amount of time to wait for a server's first response headers after fully writing the request headers if the request has an \"Expect: 100-continue\" header.")
}

func (s *serviceProxy) Run(ctx context.Context) error {
	const (
		rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	var err error
	customChecks := []healthz.Checker{}

	cc, err := addonutils.NewConfigChecker("cert", s.cert, s.key, rootCAFile)
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
	// ca for accessing ocp services
	ocpserviceCAPem, err := os.ReadFile(s.ocpserviceCA)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("ocpservice-ca is not provided")
		} else {
			return err
		}
	} else {
		s.rootCAs.AppendCertsFromPEM(ocpserviceCAPem)

		// add configchecker into http probes only when ocpservice-ca is provided
		cc, err := addonutils.NewConfigChecker("ocpservice-ca", s.ocpserviceCA)
		if err != nil {
			return err
		}
		customChecks = append(customChecks, cc.Check)
	}

	// init managedClusterKubeClient
	// managedClusterKubeClient is the kubeClient of current cluster using in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to get in-cluster config: %v", err)
	}

	s.managedClusterKubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	// get hubKubeConfig
	hubConfig, err := clientcmd.BuildConfigFromFlags("", s.hubKubeConfig)
	if err != nil {
		return err
	}
	s.hubKubeClient, err = kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return err
	}

	go func() {
		if err = utils.ServeHealthProbes(":8000", customChecks...); err != nil {
			klog.Fatal(err)
		}
	}()

	httpserver := &http.Server{
		Addr: fmt.Sprintf(":%d", constant.ServiceProxyPort),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		Handler: s,
	}

	return httpserver.ListenAndServeTLS(s.cert, s.key)
}

func (s *serviceProxy) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
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
		klog.Errorf("failed to get target service url from request: %v", err)
		return
	}

	if url.Host == "kubernetes.default.svc" {
		if err := s.processAuthentication(req); err != nil {
			klog.ErrorS(err, "authentication failed")
			http.Error(wr, err.Error(), http.StatusUnauthorized)
			return
		}
	}

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
		TLSClientConfig: &tls.Config{
			RootCAs:    s.rootCAs,
			MinVersion: tls.VersionTLS12,
		},
		// golang http pkg automaticly upgrade http connection to http2 connection, but http2 can not upgrade to SPDY which used in "kubectl exec".
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

func (s *serviceProxy) hubUserAuthenticatedAndInfo(token string) (bool, *authenticationv1.UserInfo, error) {
	tokenReview, err := s.hubKubeClient.AuthenticationV1().TokenReviews().Create(context.Background(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, nil, err
	}

	if !tokenReview.Status.Authenticated {
		return false, nil, nil
	}
	return true, &tokenReview.Status.User, nil
}

func (s *serviceProxy) managedClusterUserAuthenticatedAndInfo(token string) (bool, *authenticationv1.UserInfo, error) {
	tokenReview, err := s.managedClusterKubeClient.AuthenticationV1().TokenReviews().Create(context.Background(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token: token,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, nil, err
	}

	if !tokenReview.Status.Authenticated {
		return false, nil, nil
	}
	return true, &tokenReview.Status.User, nil
}

func (s *serviceProxy) getImpersonateToken() (string, error) {
	// Read the latest token from the mounted file
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// processAuthentication handles the authentication flow for both managed cluster and hub users
func (s *serviceProxy) processAuthentication(req *http.Request) error {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")

	// determine if the token is a managed cluster user
	managedClusterAuthenticated, _, err := s.managedClusterUserAuthenticatedAndInfo(token)
	if err != nil {
		klog.ErrorS(err, "managed cluster authentication failed")
		return fmt.Errorf("managed cluster authentication failed: %v", err)
	}

	if !managedClusterAuthenticated {
		// determine if the token is a hub user
		hubAuthenticated, hubUserInfo, err := s.hubUserAuthenticatedAndInfo(token)
		if err != nil {
			klog.ErrorS(err, "hub cluster authentication failed")
			return fmt.Errorf("authentication failed: managed cluster auth: not authenticated, hub cluster auth error: %v", err)
		}
		if !hubAuthenticated {
			klog.ErrorS(err, "authentication failed: token is neither valid for managed cluster nor hub cluster")
			return fmt.Errorf("authentication failed: token is neither valid for managed cluster nor hub cluster")
		}

		if err := s.processHubUser(req, hubUserInfo); err != nil {
			klog.ErrorS(err, "failed to process hub user")
			return fmt.Errorf("failed to process hub user: %v", err)
		}
	}

	return nil
}

// processHubUser handles the hub user specific operations including impersonation
func (s *serviceProxy) processHubUser(req *http.Request, hubUserInfo *authenticationv1.UserInfo) error {
	// set impersonate group header
	for _, group := range hubUserInfo.Groups {
		// Here using `Add` instead of `Set` to support multiple groups
		req.Header.Add("Impersonate-Group", group)
	}

	// check if the hub user is serviceaccount kind, if so, add "cluster:hub:" prefix to the username
	if strings.HasPrefix(hubUserInfo.Username, "system:serviceaccount:") {
		req.Header.Set("Impersonate-User", fmt.Sprintf("cluster:hub:%s", hubUserInfo.Username))
	} else {
		req.Header.Set("Impersonate-User", hubUserInfo.Username)
	}

	// replace the original token with cluster-proxy service-account token which has impersonate permission
	token, err := s.getImpersonateToken()
	if err != nil {
		return fmt.Errorf("failed to get impersonate token: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
