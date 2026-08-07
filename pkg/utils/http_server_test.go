package utils

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

func TestDrainConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  DrainConfig
		wantErr bool
	}{
		{name: "defaults", config: DrainConfig{Timeout: DefaultDrainTimeout, MinDuration: DefaultMinDrainDuration}},
		{name: "immediate", config: DrainConfig{}},
		{name: "negative timeout", config: DrainConfig{Timeout: -time.Second}, wantErr: true},
		{name: "negative minimum", config: DrainConfig{Timeout: time.Second, MinDuration: -time.Second}, wantErr: true},
		{name: "minimum exceeds timeout", config: DrainConfig{Timeout: time.Second, MinDuration: 2 * time.Second}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.config.Validate(); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestHealthProbeServerReadiness(t *testing.T) {
	server := NewHealthProbeServer("127.0.0.1:8000")
	assertProbeStatus(t, server, "/healthz", http.StatusOK)
	assertProbeStatus(t, server, "/readyz", http.StatusOK)

	server.SetReady(false)
	assertProbeStatus(t, server, "/readyz", http.StatusServiceUnavailable)
	assertProbeStatus(t, server, "/healthz", http.StatusOK)
}

func TestRunHTTPServersWaitsForMinimumDrainDuration(t *testing.T) {
	servers := newTestServers(t, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	runResult := runTestHTTPServers(ctx,
		DrainConfig{Timeout: time.Second, MinDuration: 100 * time.Millisecond}, servers, servers.serve)
	waitForPublicServer(t, http.DefaultClient, servers.url())

	startedDrain := time.Now()
	cancel()
	eventuallyProbeStatus(t, servers.health, "/readyz", http.StatusServiceUnavailable)

	connection, err := net.DialTimeout("tcp", servers.listener.Addr().String(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("public listener closed during minimum drain duration: %v", err)
	}
	_ = connection.Close()

	if err := <-runResult; err != nil {
		t.Fatalf("drain returned an error: %v", err)
	}
	if elapsed := time.Since(startedDrain); elapsed < 90*time.Millisecond {
		t.Fatalf("server stopped before minimum drain duration: %v", elapsed)
	}
	if connection, err := net.DialTimeout("tcp", servers.listener.Addr().String(), 50*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("public listener remained open after drain")
	}
}

func TestRunHTTPServersDrainsHTTP2Request(t *testing.T) {
	requestStarted := make(chan int, 1)
	releaseRequest := make(chan struct{})
	servers := newTestServers(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestStarted <- request.ProtoMajor
		<-releaseRequest
		_, _ = io.WriteString(writer, "drained")
	}))
	tlsConfig, transport := testHTTP2Config(t)
	servers.public.TLSConfig = tlsConfig
	t.Cleanup(transport.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := runTestHTTPServers(ctx,
		DrainConfig{Timeout: time.Second, MinDuration: 20 * time.Millisecond}, servers,
		func() error { return servers.public.ServeTLS(servers.listener, "", "") })
	responseResult := requestBody(&http.Client{Transport: transport, Timeout: time.Second},
		"https://"+servers.listener.Addr().String())

	if protocol := <-requestStarted; protocol != 2 {
		t.Fatalf("request protocol = HTTP/%d, want HTTP/2", protocol)
	}
	cancel()
	select {
	case err := <-runResult:
		t.Fatalf("server returned before active request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRequest)
	bodyResult := <-responseResult
	if bodyResult.err != nil {
		t.Fatalf("request failed during drain: %v", bodyResult.err)
	}
	if bodyResult.body != "drained" {
		t.Fatalf("response body = %q, want drained", bodyResult.body)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("drain returned an error: %v", err)
	}
}

func TestRunHTTPServersClosesIdleKeepAliveConnection(t *testing.T) {
	servers := newTestServers(t, http.NotFoundHandler())
	client := &http.Client{Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	runResult := runTestHTTPServers(ctx,
		DrainConfig{Timeout: time.Second, MinDuration: 20 * time.Millisecond}, servers, servers.serve)
	waitForPublicServer(t, client, servers.url())

	reused := false
	request, err := http.NewRequest(http.MethodGet, servers.url(), nil)
	if err != nil {
		t.Fatalf("create keep-alive request: %v", err)
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reused = info.Reused
		},
	}))
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("make keep-alive request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if !reused {
		t.Fatal("test connection was not reused before becoming idle")
	}

	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("drain returned an error: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("idle keep-alive connection delayed shutdown")
	}
}

func TestRunHTTPServersDoesNotWaitForHijackedConnection(t *testing.T) {
	hijacked := make(chan struct{})
	handlerDone := make(chan struct{})
	servers := newTestServers(t, hijackedConnectionHandler(hijacked, handlerDone))
	ctx, cancel := context.WithCancel(context.Background())
	runResult := runTestHTTPServers(ctx,
		DrainConfig{Timeout: time.Second, MinDuration: 20 * time.Millisecond}, servers, servers.serve)
	connection := openHijackedConnection(t, servers.listener.Addr().String())
	<-hijacked

	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("drain returned an error: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("server waited for hijacked connection")
	}
	select {
	case <-handlerDone:
		t.Fatal("hijacked connection closed before process exit")
	default:
	}

	if err := connection.Close(); err != nil {
		t.Fatalf("close hijacked client connection: %v", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("hijacked handler did not finish after client close")
	}
}

func TestRunHTTPServersBoundsActiveRequestDrain(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	servers := newTestServers(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		_, _ = io.WriteString(writer, "finished")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	runResult := runTestHTTPServers(ctx,
		DrainConfig{Timeout: 200 * time.Millisecond, MinDuration: 50 * time.Millisecond}, servers, servers.serve)
	responseResult := requestBody(&http.Client{Timeout: time.Second}, servers.url())
	<-requestStarted

	startedDrain := time.Now()
	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("planned timeout returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain timeout did not bound server termination")
	}
	elapsed := time.Since(startedDrain)
	if elapsed < 180*time.Millisecond {
		t.Fatalf("server stopped before drain deadline: %v", elapsed)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("minimum duration was added to the overall timeout: %v", elapsed)
	}

	close(releaseRequest)
	if result := <-responseResult; result.err != nil {
		t.Fatalf("active request failed after drain timeout: %v", result.err)
	}
}

func TestRunHTTPServersPropagatesHealthServerFailure(t *testing.T) {
	servers := newTestServers(t, http.NotFoundHandler())
	servers.health = NewHealthProbeServer("invalid address")

	err := <-runTestHTTPServers(context.Background(), DrainConfig{Timeout: time.Second}, servers, servers.serve)
	if err == nil || !strings.Contains(err.Error(), "health probe HTTP server failed") {
		t.Fatalf("expected health server failure, got %v", err)
	}
}

type testServers struct {
	public   *http.Server
	listener net.Listener
	health   *HealthProbeServer
}

func newTestServers(t *testing.T, handler http.Handler) *testServers {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for test proxy server: %v", err)
	}

	servers := &testServers{
		public:   &http.Server{Addr: listener.Addr().String(), Handler: handler},
		listener: listener,
		health:   NewHealthProbeServer("127.0.0.1:0"),
	}
	t.Cleanup(func() {
		_ = servers.public.Close()
		_ = servers.health.Close()
		_ = servers.listener.Close()
	})
	return servers
}

func (s *testServers) serve() error {
	return s.public.Serve(s.listener)
}

func (s *testServers) url() string {
	return "http://" + s.listener.Addr().String()
}

func runTestHTTPServers(
	ctx context.Context,
	config DrainConfig,
	servers *testServers,
	servePublic func() error,
) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- runHTTPServers(ctx, config, servers.public, servers.health, servePublic)
	}()
	return result
}

func waitForPublicServer(t *testing.T, client *http.Client, url string) {
	t.Helper()
	if result := <-requestBody(client, url); result.err != nil {
		t.Fatalf("wait for public server: %v", result.err)
	}
}

type bodyResult struct {
	body string
	err  error
}

func requestBody(client *http.Client, url string) <-chan bodyResult {
	result := make(chan bodyResult, 1)
	go func() {
		response, err := client.Get(url)
		if err != nil {
			result <- bodyResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		result <- bodyResult{body: string(body), err: err}
	}()
	return result
}

func assertProbeStatus(t *testing.T, server *HealthProbeServer, path string, status int) {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != status {
		t.Fatalf("%s status = %d, want %d", path, response.Code, status)
	}
}

func eventuallyProbeStatus(t *testing.T, server *HealthProbeServer, path string, status int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == status {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not reach status %d", path, status)
}

func testHTTP2Config(t *testing.T) (*tls.Config, *http.Transport) {
	t.Helper()
	testServer := httptest.NewUnstartedServer(http.NotFoundHandler())
	testServer.EnableHTTP2 = true
	testServer.StartTLS()
	tlsConfig := testServer.TLS.Clone()
	transport := testServer.Client().Transport.(*http.Transport).Clone()
	testServer.Close()
	return tlsConfig, transport
}

func hijackedConnectionHandler(hijacked, done chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer func() {
			_ = connection.Close()
			close(done)
		}()

		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = buffered.Flush()
		close(hijacked)
		_, _ = io.Copy(io.Discard, connection)
	})
}

func openHijackedConnection(t *testing.T, address string) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial proxy server: %v", err)
	}
	if _, err := io.WriteString(connection,
		"GET / HTTP/1.1\r\nHost: "+address+"\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"); err != nil {
		_ = connection.Close()
		t.Fatalf("write upgrade request: %v", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read upgrade response: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = connection.Close()
		t.Fatalf("unexpected upgrade response status: %d", response.StatusCode)
	}
	return connection
}
