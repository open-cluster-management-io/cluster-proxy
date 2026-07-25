package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	DefaultHTTPReadHeaderTimeout = 5 * time.Second
	DefaultHTTPShutdownTimeout   = 30 * time.Second
	httpForceCloseGracePeriod    = time.Second
)

// RunnableHTTPServer contains an HTTP server and its serve function.
type RunnableHTTPServer struct {
	Name   string
	Server *http.Server
	Serve  func() error
}

// NewHTTPServer creates an HTTP server for proxy traffic.
func NewHTTPServer(address string, tlsConfig *tls.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		TLSConfig:         tlsConfig,
		Handler:           handler,
		ReadHeaderTimeout: DefaultHTTPReadHeaderTimeout,
	}
}

// NewHealthProbeServer creates an HTTP server for health probes and config checks.
func NewHealthProbeServer(healthProbeBindAddress string, tlsConfig *tls.Config, customChecks ...healthz.Checker) *http.Server {
	mux := http.NewServeMux()

	checks := map[string]healthz.Checker{
		"healthz-ping": healthz.Ping,
	}

	for i, check := range customChecks {
		checks[fmt.Sprintf("custom-healthz-checker-%d", i)] = check
	}

	mux.Handle("/healthz", http.StripPrefix("/healthz", &healthz.Handler{Checks: checks}))
	return NewHTTPServer(healthProbeBindAddress, tlsConfig, mux)
}

type serverConnectionContextKey struct{}

type httpServerLifecycle struct {
	mu sync.Mutex

	inFlight   int
	hijacked   map[net.Conn]struct{}
	idle       chan struct{}
	cancelBase context.CancelFunc
}

func newHTTPServerLifecycle() *httpServerLifecycle {
	idle := make(chan struct{})
	close(idle)
	return &httpServerLifecycle{
		hijacked: make(map[net.Conn]struct{}),
		idle:     idle,
	}
}

func (l *httpServerLifecycle) startRequest() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inFlight == 0 {
		l.idle = make(chan struct{})
	}
	l.inFlight++
}

func (l *httpServerLifecycle) finishRequest(connection net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.inFlight--
	delete(l.hijacked, connection)
	if l.inFlight == 0 {
		close(l.idle)
	}
}

func (l *httpServerLifecycle) updateConnectionState(connection net.Conn, state http.ConnState) {
	// StateHijacked is terminal; finishRequest removes the connection.
	if state != http.StateHijacked {
		return
	}
	l.mu.Lock()
	l.hijacked[connection] = struct{}{}
	l.mu.Unlock()
}

func (l *httpServerLifecycle) wait(ctx context.Context) error {
	l.mu.Lock()
	idle := l.idle
	l.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *httpServerLifecycle) forceClose() []error {
	l.mu.Lock()
	connections := make([]net.Conn, 0, len(l.hijacked))
	for connection := range l.hijacked {
		connections = append(connections, connection)
	}
	l.mu.Unlock()

	l.cancelBase()

	var errs []error
	// Hijacked connections ignore context cancellation.
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errs
}

type lifecycleHandler struct {
	lifecycle *httpServerLifecycle
	handler   http.Handler
}

func (h *lifecycleHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	h.lifecycle.startRequest()
	connection, _ := request.Context().Value(serverConnectionContextKey{}).(net.Conn)
	defer h.lifecycle.finishRequest(connection)

	h.handler.ServeHTTP(w, request)
}

func prepareHTTPServer(server *http.Server) *httpServerLifecycle {
	lifecycle := newHTTPServerLifecycle()
	handler := server.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	server.Handler = &lifecycleHandler{lifecycle: lifecycle, handler: handler}

	// A shared base context lets forceClose cancel all active requests.
	forceCloseCtx, cancelBase := context.WithCancel(context.Background())
	lifecycle.cancelBase = cancelBase
	previousBaseContext := server.BaseContext
	server.BaseContext = func(listener net.Listener) context.Context {
		baseCtx := context.Background()
		if previousBaseContext != nil {
			baseCtx = previousBaseContext(listener)
		}
		requestBaseCtx, cancelRequestBase := context.WithCancel(baseCtx)
		context.AfterFunc(forceCloseCtx, cancelRequestBase)
		return requestBaseCtx
	}

	previousConnContext := server.ConnContext
	server.ConnContext = func(ctx context.Context, connection net.Conn) context.Context {
		if previousConnContext != nil {
			ctx = previousConnContext(ctx, connection)
		}
		return context.WithValue(ctx, serverConnectionContextKey{}, connection)
	}

	previousConnState := server.ConnState
	server.ConnState = func(connection net.Conn, state http.ConnState) {
		lifecycle.updateConnectionState(connection, state)
		if previousConnState != nil {
			previousConnState(connection, state)
		}
	}
	return lifecycle
}

type httpServerResult struct {
	name string
	err  error
}

// RunHTTPServers serves until the context is canceled or a server stops, then
// drains active and hijacked requests within shutdownTimeout.
func RunHTTPServers(ctx context.Context, shutdownTimeout time.Duration, servers ...RunnableHTTPServer) error {
	if err := validateHTTPServers(servers); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	lifecycles := make([]*httpServerLifecycle, 0, len(servers))
	for _, server := range servers {
		lifecycles = append(lifecycles, prepareHTTPServer(server.Server))
	}
	defer func() {
		for _, lifecycle := range lifecycles {
			lifecycle.cancelBase()
		}
	}()

	results := make(chan httpServerResult, len(servers))
	for _, server := range servers {
		go func() {
			results <- httpServerResult{name: server.Name, err: server.Serve()}
		}()
	}

	var completed []httpServerResult
	select {
	case <-ctx.Done():
	case result := <-results:
		completed = append(completed, result)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	shutdownResults := make(chan httpServerResult, len(servers))
	for _, server := range servers {
		go func() {
			shutdownResults <- httpServerResult{
				name: server.Name,
				err:  server.Server.Shutdown(shutdownCtx),
			}
		}()
	}

	var errs []error
	for range servers {
		result := <-shutdownResults
		if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("shut down %s HTTP server: %w", result.name, result.err))
		}
	}

	drainErr := waitForHTTPHandlers(shutdownCtx, lifecycles)
	if drainErr != nil {
		errs = append(errs, fmt.Errorf("drain HTTP handlers: %w", drainErr))
	}

	if len(errs) > 0 {
		for i, lifecycle := range lifecycles {
			for _, err := range lifecycle.forceClose() {
				errs = append(errs, fmt.Errorf("close hijacked connection for %s HTTP server: %w", servers[i].Name, err))
			}
		}
		for _, result := range closeHTTPServers(servers) {
			if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
				errs = append(errs, fmt.Errorf("force close %s HTTP server: %w", result.name, result.err))
			}
		}

		forceCtx, cancelForce := context.WithTimeout(context.Background(), httpForceCloseGracePeriod)
		if err := waitForHTTPHandlers(forceCtx, lifecycles); err != nil {
			errs = append(errs, fmt.Errorf("wait for forced HTTP handler close: %w", err))
		}
		completed = collectHTTPServerResults(forceCtx, results, completed, len(servers), &errs)
		cancelForce()
	} else {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), httpForceCloseGracePeriod)
		completed = collectHTTPServerResults(stopCtx, results, completed, len(servers), &errs)
		cancelStop()
	}

	for _, result := range completed {
		if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("%s HTTP server failed: %w", result.name, result.err))
		}
	}

	return errors.Join(errs...)
}

func validateHTTPServers(servers []RunnableHTTPServer) error {
	if len(servers) == 0 {
		return fmt.Errorf("at least one HTTP server is required")
	}
	for _, server := range servers {
		if server.Name == "" {
			return fmt.Errorf("HTTP server name is required")
		}
		if server.Server == nil {
			return fmt.Errorf("%s HTTP server is required", server.Name)
		}
		if server.Serve == nil {
			return fmt.Errorf("%s HTTP server serve function is required", server.Name)
		}
	}
	return nil
}

func waitForHTTPHandlers(ctx context.Context, lifecycles []*httpServerLifecycle) error {
	for _, lifecycle := range lifecycles {
		if err := lifecycle.wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

func closeHTTPServers(servers []RunnableHTTPServer) []httpServerResult {
	results := make(chan httpServerResult, len(servers))
	for _, server := range servers {
		go func() {
			results <- httpServerResult{name: server.Name, err: server.Server.Close()}
		}()
	}

	completed := make([]httpServerResult, 0, len(servers))
	for range servers {
		completed = append(completed, <-results)
	}
	return completed
}

func collectHTTPServerResults(
	ctx context.Context,
	results <-chan httpServerResult,
	completed []httpServerResult,
	total int,
	errs *[]error,
) []httpServerResult {
	for len(completed) < total {
		select {
		case result := <-results:
			completed = append(completed, result)
		case <-ctx.Done():
			*errs = append(*errs, fmt.Errorf("wait for HTTP servers to stop: %w", ctx.Err()))
			return completed
		}
	}
	return completed
}
