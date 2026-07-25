package utils

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewHTTPServerConfiguresHeaderTimeoutWithoutBreakingStreaming(t *testing.T) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	server := NewHTTPServer("127.0.0.1:8443", tlsConfig, handler)

	if server.Addr != "127.0.0.1:8443" {
		t.Fatalf("unexpected address: %q", server.Addr)
	}
	if server.TLSConfig != tlsConfig {
		t.Fatal("TLS config was not preserved")
	}
	if server.Handler == nil {
		t.Fatal("handler was not configured")
	}
	if server.ReadHeaderTimeout != DefaultHTTPReadHeaderTimeout {
		t.Fatalf("unexpected ReadHeaderTimeout: %v", server.ReadHeaderTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout must remain unset for streaming requests, got %v", server.WriteTimeout)
	}

	healthServer := NewHealthProbeServer("127.0.0.1:8000", tlsConfig)
	if healthServer.ReadHeaderTimeout != DefaultHTTPReadHeaderTimeout {
		t.Fatalf("health server has unexpected ReadHeaderTimeout: %v", healthServer.ReadHeaderTimeout)
	}
	if healthServer.WriteTimeout != 0 {
		t.Fatalf("health server has unexpected WriteTimeout: %v", healthServer.WriteTimeout)
	}
}

func TestPrepareHTTPServerPreservesAndCancelsBaseContext(t *testing.T) {
	type contextKey struct{}

	t.Run("preserves parent values and cancellation", func(t *testing.T) {
		parentCtx, cancelParent := context.WithCancel(
			context.WithValue(context.Background(), contextKey{}, "parent-value"),
		)
		server := NewHTTPServer("", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		server.BaseContext = func(net.Listener) context.Context {
			return parentCtx
		}
		lifecycle := prepareHTTPServer(server)
		t.Cleanup(lifecycle.cancelBase)

		baseCtx := server.BaseContext(nil)
		if got := baseCtx.Value(contextKey{}); got != "parent-value" {
			t.Fatalf("base context value was not preserved: got %v", got)
		}

		cancelParent()
		select {
		case <-baseCtx.Done():
		case <-time.After(time.Second):
			t.Fatal("parent cancellation did not reach the wrapped base context")
		}
	})

	t.Run("propagates lifecycle cancellation", func(t *testing.T) {
		server := NewHTTPServer("", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		lifecycle := prepareHTTPServer(server)
		baseCtx := server.BaseContext(nil)

		lifecycle.cancelBase()
		select {
		case <-baseCtx.Done():
		case <-time.After(time.Second):
			t.Fatal("lifecycle cancellation did not reach the wrapped base context")
		}
	})
}

func TestRunHTTPServersDrainsActiveRequestOnCancellation(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := NewHTTPServer("", nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		_, _ = io.WriteString(w, "drained")
	}))
	listener := newTestListener(t)
	serveStarted := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- RunHTTPServers(ctx, time.Second, RunnableHTTPServer{
			Name:   "public",
			Server: server,
			Serve: func() error {
				close(serveStarted)
				return server.Serve(listener)
			},
		})
	}()
	<-serveStarted

	client := &http.Client{Timeout: time.Second}
	responseResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			responseResult <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && string(body) != "drained" {
			err = errors.New("response was not drained")
		}
		responseResult <- err
	}()
	<-requestStarted

	cancel()
	select {
	case err := <-runResult:
		t.Fatalf("server returned before the active request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRequest)
	if err := <-responseResult; err != nil {
		t.Fatalf("active request failed during graceful shutdown: %v", err)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("graceful shutdown returned an error: %v", err)
	}
}

func TestRunHTTPServersPropagatesFailureAndStopsPeer(t *testing.T) {
	listener := newTestListener(t)
	peer := NewHTTPServer("", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	peerStarted := make(chan struct{})
	wantErr := errors.New("health listener failed")

	err := RunHTTPServers(context.Background(), time.Second,
		RunnableHTTPServer{
			Name:   "public",
			Server: peer,
			Serve: func() error {
				close(peerStarted)
				return peer.Serve(listener)
			},
		},
		RunnableHTTPServer{
			Name:   "health probe",
			Server: &http.Server{},
			Serve: func() error {
				<-peerStarted
				return wantErr
			},
		},
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("server failure was not propagated: %v", err)
	}
	connection, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	if dialErr == nil {
		connection.Close()
		t.Fatal("peer listener remained open after another server failed")
	}
}

func TestRunHTTPServersForcesCloseAfterShutdownTimeout(t *testing.T) {
	requestStarted := make(chan struct{})

	server := NewHTTPServer("", nil, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
	}))
	listener := newTestListener(t)
	serveStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- RunHTTPServers(ctx, 20*time.Millisecond, RunnableHTTPServer{
			Name:   "public",
			Server: server,
			Serve: func() error {
				close(serveStarted)
				return server.Serve(listener)
			},
		})
	}()
	<-serveStarted

	client := &http.Client{Timeout: time.Second}
	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String())
		if response != nil {
			response.Body.Close()
		}
		requestResult <- err
	}()
	<-requestStarted
	cancel()

	select {
	case err := <-runResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected shutdown deadline error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown timeout did not bound server termination")
	}
	<-requestResult
}

func TestRunHTTPServersWaitsForHijackedRequestToCloseNaturally(t *testing.T) {
	hijacked := make(chan struct{})
	handlerEnded := make(chan struct{})
	handlerErr := make(chan error, 1)
	server := NewHTTPServer("", nil, upgradedConnectionHandler(hijacked, handlerEnded, handlerErr))
	observedHijack := make(chan struct{})
	var observedHijackOnce sync.Once
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			observedHijackOnce.Do(func() {
				close(observedHijack)
			})
		}
	}
	listener := newTestListener(t)
	serveStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- RunHTTPServers(ctx, time.Second, RunnableHTTPServer{
			Name:   "public",
			Server: server,
			Serve: func() error {
				close(serveStarted)
				return server.Serve(listener)
			},
		})
	}()
	<-serveStarted

	connection := openUpgradedConnection(t, listener.Addr().String())
	<-hijacked
	<-observedHijack
	cancel()

	select {
	case err := <-runResult:
		t.Fatalf("server returned before hijacked request closed naturally: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := connection.Close(); err != nil {
		t.Fatalf("close upgraded client connection: %v", err)
	}
	<-handlerEnded
	select {
	case err := <-handlerErr:
		t.Fatalf("upgraded handler failed: %v", err)
	default:
	}
	if err := <-runResult; err != nil {
		t.Fatalf("natural upgraded connection drain returned an error: %v", err)
	}
}

func TestRunHTTPServersForceClosesHijackedRequestAtDeadline(t *testing.T) {
	hijacked := make(chan struct{})
	handlerEnded := make(chan struct{})
	handlerErr := make(chan error, 1)
	server := NewHTTPServer("", nil, upgradedConnectionHandler(hijacked, handlerEnded, handlerErr))
	listener := newTestListener(t)
	serveStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- RunHTTPServers(ctx, 30*time.Millisecond, RunnableHTTPServer{
			Name:   "public",
			Server: server,
			Serve: func() error {
				close(serveStarted)
				return server.Serve(listener)
			},
		})
	}()
	<-serveStarted

	connection := openUpgradedConnection(t, listener.Addr().String())
	defer connection.Close()
	<-hijacked
	cancel()

	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set upgraded connection read deadline: %v", err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("upgraded client connection remained open after shutdown deadline")
	}

	select {
	case <-handlerEnded:
	case <-time.After(time.Second):
		t.Fatal("hijacked handler did not end after its connection was force-closed")
	}
	select {
	case err := <-handlerErr:
		t.Fatalf("upgraded handler failed: %v", err)
	default:
	}
	select {
	case err := <-runResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected shutdown deadline error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("force-closing hijacked request exceeded cleanup grace")
	}
}

func upgradedConnectionHandler(
	hijacked chan<- struct{},
	ended chan<- struct{},
	handlerErr chan<- error,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(ended)

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			handlerErr <- errors.New("response writer does not implement http.Hijacker")
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			handlerErr <- fmt.Errorf("hijack connection: %w", err)
			return
		}
		defer connection.Close()

		if _, err := buffered.WriteString(
			"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n",
		); err != nil {
			handlerErr <- fmt.Errorf("write upgrade response: %w", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			handlerErr <- fmt.Errorf("flush upgrade response: %w", err)
			return
		}
		close(hijacked)

		_, _ = io.Copy(io.Discard, connection)
	})
}

func openUpgradedConnection(t *testing.T, address string) net.Conn {
	t.Helper()

	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial upgrade server: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		connection.Close()
		t.Fatalf("set upgrade handshake deadline: %v", err)
	}
	if _, err := fmt.Fprintf(
		connection,
		"GET /upgrade HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n",
		address,
	); err != nil {
		connection.Close()
		t.Fatalf("write upgrade request: %v", err)
	}

	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		connection.Close()
		t.Fatalf("read upgrade status: %v", err)
	}
	if !strings.Contains(status, "101 Switching Protocols") {
		connection.Close()
		t.Fatalf("unexpected upgrade status: %q", status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			connection.Close()
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		connection.Close()
		t.Fatalf("clear upgrade handshake deadline: %v", err)
	}
	return connection
}

func newTestListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return listener
}
