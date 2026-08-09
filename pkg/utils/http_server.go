package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	DefaultDrainTimeout     = 30 * time.Second
	DefaultMinDrainDuration = 10 * time.Second
	proxyReadHeaderTimeout  = 32 * time.Second
)

// NewProxyHTTPServer creates an HTTP server for proxy traffic.
func NewProxyHTTPServer(address string, tlsConfig *tls.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		TLSConfig:         tlsConfig,
		Handler:           handler,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
	}
}

// DrainConfig controls how long active HTTP requests may remain during shutdown.
// MinDuration is part of, rather than additional to, Timeout.
type DrainConfig struct {
	Timeout     time.Duration
	MinDuration time.Duration
}

func (c DrainConfig) Validate() error {
	if c.Timeout < 0 {
		return fmt.Errorf("drain-timeout must not be negative")
	}
	if c.MinDuration < 0 {
		return fmt.Errorf("min-drain-duration must not be negative")
	}
	if c.MinDuration > c.Timeout {
		return fmt.Errorf("min-drain-duration must not exceed drain-timeout")
	}
	return nil
}

// AddFlags registers the drain flags shared by the proxy server commands.
func (c *DrainConfig) AddFlags(flags *pflag.FlagSet) {
	flags.DurationVar(&c.Timeout, "drain-timeout", DefaultDrainTimeout,
		"Maximum time to drain active HTTP requests during shutdown.")
	flags.DurationVar(&c.MinDuration, "min-drain-duration", DefaultMinDrainDuration,
		"Minimum drain duration allowing time for endpoint deprogramming.")
}

type HealthProbeServer struct {
	*http.Server
	ready atomic.Bool
}

// NewHealthProbeServer creates separate liveness and readiness endpoints.
// Readiness only represents whether the proxy is accepting new traffic; the
// existing config checks remain on the liveness endpoint.
func NewHealthProbeServer(healthProbeBindAddress string, customChecks ...healthz.Checker) *HealthProbeServer {
	healthServer := &HealthProbeServer{}
	healthServer.ready.Store(true)

	mux := http.NewServeMux()
	checks := map[string]healthz.Checker{
		"healthz-ping": healthz.Ping,
	}
	for i, check := range customChecks {
		checks[fmt.Sprintf("custom-healthz-checker-%d", i)] = check
	}

	mux.Handle("/healthz", http.StripPrefix("/healthz", &healthz.Handler{Checks: checks}))
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !healthServer.ready.Load() {
			http.Error(writer, "draining", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	healthServer.Server = &http.Server{
		Addr:              healthProbeBindAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return healthServer
}

func (s *HealthProbeServer) SetReady(ready bool) {
	s.ready.Store(ready)
}

type httpServerResult struct {
	health bool
	err    error
}

// RunHTTPServers serves until the context is canceled or either server stops.
// During planned shutdown it first removes the proxy from readiness, waits for
// endpoint deprogramming, and then drains requests managed by net/http.
func RunHTTPServers(
	ctx context.Context,
	drainConfig DrainConfig,
	public *http.Server,
	certFile, keyFile string,
	health *HealthProbeServer,
) error {
	if ctx.Err() != nil {
		health.SetReady(false)
		return nil
	}

	return runHTTPServers(ctx, drainConfig, public, health, func() error {
		return public.ListenAndServeTLS(certFile, keyFile)
	})
}

func runHTTPServers(
	ctx context.Context,
	drainConfig DrainConfig,
	public *http.Server,
	health *HealthProbeServer,
	servePublic func() error,
) error {
	serverResults := make(chan httpServerResult, 2)
	go func() {
		serverResults <- httpServerResult{err: servePublic()}
	}()
	go func() {
		serverResults <- httpServerResult{health: true, err: health.ListenAndServe()}
	}()

	select {
	case <-ctx.Done():
	case result := <-serverResults:
		return unexpectedServerError(result)
	}

	health.SetReady(false)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainConfig.Timeout)
	defer cancelDrain()

	if drainConfig.MinDuration > 0 {
		timer := time.NewTimer(drainConfig.MinDuration)
		defer timer.Stop()
		select {
		case <-timer.C:
		case result := <-serverResults:
			return unexpectedServerError(result)
		}
	}

	err := public.Shutdown(drainCtx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		klog.Warningf("HTTP request drain timeout %s expired; exiting with active requests", drainConfig.Timeout)
		return nil
	default:
		return fmt.Errorf("drain public proxy HTTP requests: %w", err)
	}
}

func unexpectedServerError(result httpServerResult) error {
	if result.err == nil {
		return fmt.Errorf("HTTP server stopped unexpectedly")
	}
	if result.health {
		return fmt.Errorf("health probe HTTP server failed: %w", result.err)
	}
	return fmt.Errorf("public proxy HTTP server failed: %w", result.err)
}
