package servicerelay

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"open-cluster-management.io/cluster-proxy/pkg/constant"
	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

type ServiceRelay struct {
	Listen                 string
	AdditionalServiceCA    string
	HealthProbeBindAddress string
	rootCAs                *x509.CertPool
	transport              http.RoundTripper
}

func NewCommand() *cobra.Command {
	relay := &ServiceRelay{
		Listen:                 fmt.Sprintf(":%d", constant.ServiceRelayPort),
		HealthProbeBindAddress: ":8000",
	}

	cmd := &cobra.Command{
		Use:   "service-relay",
		Short: "Relay hosted service-proxy requests to managed cluster Services",
		RunE: func(cmd *cobra.Command, args []string) error {
			return relay.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&relay.Listen, "listen", relay.Listen, "The HTTP listen address")
	flags.StringVar(&relay.AdditionalServiceCA, "additional-service-ca", relay.AdditionalServiceCA, "The path to the additional CA certificate for services")
	flags.StringVar(&relay.HealthProbeBindAddress, "health-probe-bind-address", relay.HealthProbeBindAddress, "The address the health probe and metrics endpoint binds to")

	return cmd
}

func (s *ServiceRelay) Run(ctx context.Context) error {
	if s.Listen == "" {
		return fmt.Errorf("listen address is required")
	}

	s.rootCAs, _ = x509.SystemCertPool()
	if s.rootCAs == nil {
		s.rootCAs = x509.NewCertPool()
	}

	if s.AdditionalServiceCA != "" {
		caData, err := os.ReadFile(s.AdditionalServiceCA)
		if err != nil {
			if os.IsNotExist(err) {
				klog.Infof("additional-service-ca file not found: %s", s.AdditionalServiceCA)
			} else {
				return err
			}
		} else if ok := s.rootCAs.AppendCertsFromPEM(caData); !ok {
			return fmt.Errorf("failed to parse additional service CA %s", s.AdditionalServiceCA)
		}
	}

	if s.transport == nil {
		s.transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig: &tls.Config{
				RootCAs:    s.rootCAs,
				MinVersion: tls.VersionTLS12,
			},
			ForceAttemptHTTP2: false,
		}
	}

	go func() {
		if err := utils.ServeHealthProbes(s.HealthProbeBindAddress, nil); err != nil {
			klog.Fatal(err)
		}
	}()

	server := &http.Server{
		Addr:              s.Listen,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	klog.Infof("service relay listening on %s", s.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return ctx.Err()
}

func (s *ServiceRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	target, err := utils.GetTargetServiceURLFromRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		addonmetrics.ObserveServiceRelayRequest("unknown", "error")
		return
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		http.Error(w, fmt.Sprintf("unsupported target scheme %q", target.Scheme), http.StatusBadRequest)
		addonmetrics.ObserveServiceRelayRequest(target.Scheme, "error")
		return
	}
	if target.Host == "kubernetes.default.svc" {
		http.Error(w, "service relay does not proxy kube-apiserver requests", http.StatusBadRequest)
		addonmetrics.ObserveServiceRelayRequest(target.Scheme, "error")
		return
	}

	restoreAuthorizationHeader(req)
	removeClusterProxyHeaders(req)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = s.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		klog.Errorf("service relay proxy error: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	}

	recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
	proxy.ServeHTTP(recorder, req)
	addonmetrics.ObserveServiceRelayRequest(target.Scheme, resultFromStatus(recorder.statusCode))
}

func restoreAuthorizationHeader(req *http.Request) {
	authorization := req.Header.Get(utils.HeaderClusterProxyAuthorization)
	req.Header.Del("Authorization")
	req.Header.Del(utils.HeaderClusterProxyAuthorization)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
}

func removeClusterProxyHeaders(req *http.Request) {
	for _, header := range []string{
		utils.HeaderClusterProxyProto,
		utils.HeaderClusterProxyNamespace,
		utils.HeaderClusterProxyService,
		utils.HeaderClusterProxyPort,
	} {
		req.Header.Del(header)
	}
}

func resultFromStatus(statusCode int) string {
	if statusCode >= http.StatusBadRequest {
		return "error"
	}
	return "success"
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}
