package userserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"k8s.io/client-go/kubernetes"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	addonclient "open-cluster-management.io/api/client/addon/clientset/versioned"
	addoninformers "open-cluster-management.io/api/client/addon/informers/externalversions"
	addonlisterv1beta1 "open-cluster-management.io/api/client/addon/listers/addon/v1beta1"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"

	"open-cluster-management.io/cluster-proxy/pkg/constant"
	clusterproxyutil "open-cluster-management.io/cluster-proxy/pkg/util"
	"open-cluster-management.io/cluster-proxy/pkg/utils"

	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
	ctrl "sigs.k8s.io/controller-runtime"
)

func NewUserServerCommand() *cobra.Command {
	userServer := newUserServer()

	cmd := &cobra.Command{
		Use:   "user-server",
		Short: "user-server",
		Long:  `A http proxy server running on the hub cluster, receives http requests from users and forwards to the ANP proxy-server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return userServer.Run(cmd.Context())
		},
	}

	userServer.AddFlags(cmd)
	return cmd
}

var (
	serviceProxyRootCA *x509.CertPool
)

type userServer struct {
	getTunnel       func(context.Context) (konnectivity.Tunnel, error)
	proxyServerHost string
	proxyServerPort int

	proxyCACertPath, proxyCertPath, proxyKeyPath string

	serverCert, serverKey string
	serverPort            int

	serviceProxyCACertPath string
	agentInstallNamespace  string
	drain                  utils.DrainConfig

	addonLister addonlisterv1beta1.ManagedClusterAddOnLister

	// exposedServicesConfigMap is the name of the ConfigMap (in the pod's own
	// namespace) that lists permitted service proxy targets. Defaults to the
	// well-known constant ExposedServicesConfigMapName.
	exposedServicesConfigMap string
	// serviceAllowlist is populated at startup from the ConfigMap and kept
	// up to date by an informer.
	serviceAllowlist *ServiceAllowlist

	maxConnsPerHost     int
	maxIdleConnsPerHost int
	idleConnTimeout     time.Duration

	// transports caches one http.Transport per managed cluster name.
	// Each transport maintains a connection pool to the cluster's service-proxy,
	// allowing tunnel reuse across sequential HTTP requests and eliminating the
	// tunnel-per-request churn that saturates the ANP proxy-server under load.
	// Key: cluster name (string), Value: *http.Transport
	transports sync.Map
}

func (k *userServer) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&k.proxyServerHost, "host", k.proxyServerHost, "The host of the ANP proxy-server")
	flags.IntVar(&k.proxyServerPort, "port", k.proxyServerPort, "The port of the ANP proxy-server")

	flags.StringVar(&k.proxyCACertPath, "proxy-ca-cert", k.proxyCACertPath, "The path to the CA certificate of the ANP proxy-server")
	flags.StringVar(&k.proxyCertPath, "proxy-cert", k.proxyCertPath, "The path to the certificate of the ANP proxy-server")
	flags.StringVar(&k.proxyKeyPath, "proxy-key", k.proxyKeyPath, "The path to the key of the ANP proxy-server")

	flags.StringVar(&k.serverCert, "server-cert", k.serverCert, "Secure communication with this cert")
	flags.StringVar(&k.serverKey, "server-key", k.serverKey, "Secure communication with this key")
	flags.IntVar(&k.serverPort, "server-port", k.serverPort, "handle user request using this port")

	flags.StringVar(&k.serviceProxyCACertPath, "service-proxy-ca-cert", k.serviceProxyCACertPath, "The path to the CA certificate of the service proxy server")

	flags.StringVar(&k.agentInstallNamespace, "agent-install-namespace", k.agentInstallNamespace, "The namespace of the agent install")
	k.drain.AddFlags(flags)

	flags.StringVar(&k.exposedServicesConfigMap, "exposed-services-configmap", constant.ExposedServicesConfigMapName,
		"Name of the ConfigMap (in the pod's namespace) that lists which services are reachable via the service proxy path")
}

func (k *userServer) Validate() error {
	if err := k.drain.Validate(); err != nil {
		return err
	}

	if k.serverCert == "" {
		return fmt.Errorf("the server-cert is required")
	}

	if k.serverKey == "" {
		return fmt.Errorf("the server-key is required")
	}

	if k.serverPort == 0 {
		return fmt.Errorf("the server-port is required")
	}

	if k.serviceProxyCACertPath == "" {
		return fmt.Errorf("the serviceproxy-ca-cert is required")
	}

	return nil
}

func newUserServer() *userServer {
	return &userServer{
		maxIdleConnsPerHost: 10,
		maxConnsPerHost:     10,
		idleConnTimeout:     90 * time.Second,
	}
}

func (k *userServer) init(ctx context.Context, kubeClient kubernetes.Interface, podNamespace string) error {
	proxyTLSCfg, err := util.GetClientTLSConfig(k.proxyCACertPath, k.proxyCertPath, k.proxyKeyPath, k.proxyServerHost, nil)
	if err != nil {
		return err
	}

	// prepare ca for service proxy server
	serviceProxyRootCA, err = certutil.NewPool(k.serviceProxyCACertPath)
	if err != nil {
		return fmt.Errorf("failed to load service proxy ca cert: %w", err)
	}

	k.getTunnel = func(tunnelCtx context.Context) (konnectivity.Tunnel, error) {
		// instantiate a gprc proxy dialer
		tunnel, err := konnectivity.CreateSingleUseGrpcTunnelWithContext(
			ctx,
			tunnelCtx,
			net.JoinHostPort(k.proxyServerHost, strconv.Itoa(k.proxyServerPort)),
			grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time: time.Minute * 10,
			}),
		)
		if err != nil {
			return nil, err
		}
		return tunnel, nil
	}

	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get Kubernetes config for add-on informer: %w", err)
	}
	addonClient, err := addonclient.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("failed to create add-on client: %w", err)
	}
	addonInformerFactory := addoninformers.NewSharedInformerFactory(addonClient, 30*time.Minute)
	k.addonLister = addonInformerFactory.Addon().V1beta1().ManagedClusterAddOns().Lister()
	addonInformerFactory.Start(ctx.Done())

	// Start the service allowlist watcher. The watcher enforces default-deny:
	// only services listed in the ConfigMap are reachable via the service proxy
	// path. Kube-apiserver proxy requests are not subject to this check.
	k.serviceAllowlist, err = startServiceAllowlistWatcher(ctx, kubeClient, podNamespace, k.exposedServicesConfigMap)
	if err != nil {
		return fmt.Errorf("failed to start service allowlist watcher: %w", err)
	}
	klog.Infof("service allowlist active: %d entries loaded from ConfigMap %s/%s",
		k.serviceAllowlist.Len(), podNamespace, k.exposedServicesConfigMap)

	klog.Infof("transport pool config: maxConnsPerHost=%d maxIdleConnsPerHost=%d idleConnTimeout=%v",
		k.maxConnsPerHost, k.maxIdleConnsPerHost, k.idleConnTimeout)

	return nil
}

// getOrCreateTransport returns the cached *http.Transport for clusterName,
// creating and storing one if none exists yet.
//
// A single transport is shared across all requests to the same cluster.
// At high request rates, multiple connections may exist simultaneously, but
// each is reused by subsequent requests rather than torn down after a single use.
//
// Without this caching, every HTTP request creates a new gRPC tunnel (full
// TCP+TLS+HTTP/2 negotiation) regardless of whether an existing tunnel is idle.
// Under load this saturates the ANP proxy-server with concurrent short-lived
// Proxy() handlers, causing lock contention on shared state and channel
// backpressure errors.
func (k *userServer) getOrCreateTransport(clusterName string) *http.Transport {
	if t, ok := k.transports.Load(clusterName); ok {
		return t.(*http.Transport)
	}

	transport := &http.Transport{
		MaxConnsPerHost:     k.maxConnsPerHost,
		MaxIdleConns:        k.maxIdleConnsPerHost, // same as MaxIdleConnsPerHost since this transport serves a single host (one managed cluster)
		MaxIdleConnsPerHost: k.maxIdleConnsPerHost,
		IdleConnTimeout:     k.idleConnTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs:    serviceProxyRootCA,
			MinVersion: tls.VersionTLS12,
		},
		// golang http pkg automatically upgrade http connection to http2 connection, but http2 can not upgrade to SPDY which used in "kubectl exec".
		// set ForceAttemptHTTP2 = false to prevent auto http2 upgration
		ForceAttemptHTTP2:     false,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			klog.V(4).Infof("creating tunnel for cluster %s (transport pool miss)", clusterName)
			tunnel, err := k.getTunnel(ctx)
			if err != nil {
				return nil, err
			}
			return tunnel.DialContext(ctx, network, addr)
		},
	}

	// LoadOrStore handles the race where two goroutines concurrently create
	// a transport for the same cluster -- the loser's transport is discarded
	// (it has no active connections so there is no resource leak).
	actual, _ := k.transports.LoadOrStore(clusterName, transport)
	return actual.(*http.Transport)
}

func (k *userServer) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
	if klog.V(4).Enabled() {
		dump, err := httputil.DumpRequest(req, true)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		klog.V(4).Infof("request:\n%s", string(dump))
	}

	var tsc utils.TargetServiceConfig
	var err error

	switch utils.GetProxyType(req.RequestURI) {
	case utils.ProxyTypeService:
		tsc, err = utils.GetTargetServiceConfig(req.RequestURI)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		if !k.serviceAllowlist.IsAllowed(tsc) {
			klog.V(4).Infof("service proxy request denied: %s/%s is not in the exposed services allowlist",
				tsc.Namespace, tsc.Service)
			http.Error(wr,
				fmt.Sprintf("service %s/%s is not in the exposed services allowlist", tsc.Namespace, tsc.Service),
				http.StatusForbidden)
			return
		}
	case utils.ProxyTypeKubeAPIServer:
		tsc, err = utils.GetTargetServiceConfigForKubeAPIServer(req.RequestURI)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
	}

	targetURL, err := url.Parse(serviceProxyURL(tsc.Cluster))
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = k.getOrCreateTransport(tsc.Cluster)

	proxy.ErrorHandler = func(rw http.ResponseWriter, r *http.Request, e error) {
		http.Error(rw, fmt.Sprintf("proxy to anp-proxy-server failed because %v", e), http.StatusBadGateway)
		klog.Errorf("proxy to anp-proxy-server failed because %v", e)
	}

	klog.V(4).Infof("request scheme:%s; rawQuery:%s; path:%s", req.URL.Scheme, req.URL.RawQuery, req.URL.Path)

	proxy.ServeHTTP(wr, utils.UpdateRequest(tsc, req))
}

func (k *userServer) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var err error

	klog.Info("begin to run user server")

	if err = k.Validate(); err != nil {
		return err
	}

	podNamespace := os.Getenv("POD_NAMESPACE")
	if len(podNamespace) == 0 {
		return fmt.Errorf("pod namespace is empty, please set the POD_NAMESPACE environment variable")
	}

	// Create the kube client once and share it between init (service allowlist
	// watcher) and the TLS ConfigMap watcher so we only open one connection.
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get Kubernetes config: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("failed to create kube client: %w", err)
	}

	if err = k.init(runCtx, kubeClient, podNamespace); err != nil {
		return err
	}

	sdkTLSConfig, err := sdktls.StartTLSConfigMapWatcher(runCtx, kubeClient, podNamespace, func() {
		klog.Info("TLS ConfigMap changed, shutting down gracefully for restart")
		cancel()
	})
	if err != nil {
		return fmt.Errorf("failed to start TLS ConfigMap watcher: %w", err)
	}
	klog.Infof("TLS config loaded: minVersion=%s, ciphersuites=%s", sdktls.VersionToString(sdkTLSConfig.MinVersion),
		sdktls.CipherSuitesToString(sdkTLSConfig.CipherSuites))

	cc, err := addonutils.NewConfigChecker("user-server", k.proxyCACertPath, k.proxyCertPath, k.proxyKeyPath, k.serverCert, k.serverKey, k.serviceProxyCACertPath)
	if err != nil {
		return fmt.Errorf("failed to create config checker: %w", err)
	}

	tlsConfig := &tls.Config{
		MinVersion:   sdkTLSConfig.MinVersion,
		CipherSuites: sdkTLSConfig.CipherSuites,
	}

	healthServer := utils.NewHealthProbeServer(":8000", cc.Check)
	publicServer := utils.NewProxyHTTPServer(fmt.Sprintf(":%d", k.serverPort), tlsConfig, k)

	klog.Infof("starting user HTTPS server on %d and health server on 8000", k.serverPort)
	return utils.RunHTTPServers(
		runCtx,
		k.drain,
		publicServer,
		k.serverCert,
		k.serverKey,
		healthServer,
	)
}

// serviceProxyURL is used to generate the URL of the service proxy server.
func serviceProxyURL(clusterName string) string {
	serviceProxyHost := clusterproxyutil.GenerateServiceProxyHost(clusterName)
	return fmt.Sprintf("https://%s:%d", serviceProxyHost, constant.ServiceProxyPort)
}
