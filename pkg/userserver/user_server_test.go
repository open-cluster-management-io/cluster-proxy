package userserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"

	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

const testClusterName = "cluster1"

type dialingTunnel struct {
	address string
	done    chan struct{}
}

func newDialingTunnel(address string) *dialingTunnel {
	done := make(chan struct{})
	close(done)
	return &dialingTunnel{address: address, done: done}
}

func (t *dialingTunnel) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, t.address)
}

func (t *dialingTunnel) Done() <-chan struct{} {
	return t.done
}

var _ konnectivity.Tunnel = (*dialingTunnel)(nil)

func TestCachedTransportSettings(t *testing.T) {
	server := newUserServer()
	server.transportPool.allow(testClusterName)
	transport, ok := server.transportPool.getOrCreate(testClusterName)
	if !ok {
		t.Fatal("expected transport for allowed cluster")
	}

	if transport.MaxConnsPerHost != 10 {
		t.Errorf("MaxConnsPerHost = %d, want 10", transport.MaxConnsPerHost)
	}
	if transport.MaxIdleConns != 10 {
		t.Errorf("MaxIdleConns = %d, want 10", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10", transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %s, want 90s", transport.IdleConnTimeout)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("cached transport must require TLS 1.2 or newer")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must be enabled by default")
	}

	server.enableHTTP2 = false
	disabled := server.newCachedTransport("cluster2")
	if disabled.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must be disabled when --enable-http2=false")
	}
	if server.newUpgradeTransport(newDialingTunnel("127.0.0.1:1")).ForceAttemptHTTP2 {
		t.Fatal("upgrade transport must always use HTTP/1.1")
	}
}

func TestServeHTTPRejectsUnroutableRequestsWithoutCreatingTransport(t *testing.T) {
	tests := map[string]struct {
		path           string
		allowedCluster bool
		allowedService bool
		wantStatus     int
	}{
		"unknown cluster Kubernetes API": {
			path:       kubeAPIPath("unknown"),
			wantStatus: http.StatusNotFound,
		},
		"unknown cluster disallowed service": {
			path:       serviceProxyPath("unknown"),
			wantStatus: http.StatusNotFound,
		},
		"unknown cluster allowed service": {
			path:           serviceProxyPath("unknown"),
			allowedService: true,
			wantStatus:     http.StatusNotFound,
		},
		"known cluster disallowed service": {
			path:           serviceProxyPath(testClusterName),
			allowedCluster: true,
			wantStatus:     http.StatusForbidden,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var tunnelCount atomic.Int64
			server := newTestUserServer(nil, func(context.Context) (konnectivity.Tunnel, error) {
				tunnelCount.Add(1)
				return nil, fmt.Errorf("unexpected tunnel")
			})
			if test.allowedCluster {
				server.transportPool.allow(testClusterName)
			}
			if test.allowedService {
				server.serviceAllowlist.update([]ExposedService{{
					Namespace: "default",
					Service:   "example",
					Port:      "80",
					Protocol:  "http",
				}})
			}

			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if got := tunnelCount.Load(); got != 0 {
				t.Fatalf("getTunnel called %d times, want 0", got)
			}
			server.transportPool.mu.RLock()
			cachedTransports := len(server.transportPool.transports)
			server.transportPool.mu.RUnlock()
			if cachedTransports != 0 {
				t.Fatalf("created %d cached transports, want 0", cachedTransports)
			}
		})
	}
}

func TestServeHTTPReusesOneHTTP2Tunnel(t *testing.T) {
	protocols := make(chan string, 2)
	backend, roots := newTLSBackend(t, testClusterName, true, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		protocols <- request.Proto
		_, _ = io.WriteString(w, "ok")
	}))
	server, tunnelCount := newProxyToBackend(t, roots, backend.Listener.Addr().String(), true)
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	for range 2 {
		response := getResponse(t, proxy.Client(), proxy.URL+kubeAPIPath(testClusterName))
		if response != "ok" {
			t.Fatalf("response body = %q, want %q", response, "ok")
		}
	}

	for range 2 {
		if protocol := <-protocols; protocol != "HTTP/2.0" {
			t.Fatalf("backend protocol = %q, want HTTP/2.0", protocol)
		}
	}
	if got := tunnelCount.Load(); got != 1 {
		t.Fatalf("created %d tunnels for two sequential requests, want 1", got)
	}
}

func TestServeHTTPMultiplexesConcurrentRequestsOverOneHTTP2Tunnel(t *testing.T) {
	var active atomic.Int64
	var maximum atomic.Int64
	backend, roots := newTLSBackend(t, testClusterName, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	server, tunnelCount := newProxyToBackend(t, roots, backend.Listener.Addr().String(), true)
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	getResponse(t, proxy.Client(), proxy.URL+kubeAPIPath(testClusterName))

	const requests = 64
	errors := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := proxy.Client().Get(proxy.URL + kubeAPIPath(testClusterName))
			if err != nil {
				errors <- err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				errors <- err
				return
			}
			if response.StatusCode != http.StatusOK || string(body) != "ok" {
				errors <- fmt.Errorf("status=%d body=%q", response.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent request: %v", err)
	}
	if maximum.Load() < 2 {
		t.Fatal("backend requests did not overlap, so multiplexing was not exercised")
	}
	if got := tunnelCount.Load(); got != 1 {
		t.Fatalf("created %d tunnels after warmup and %d concurrent requests, want 1", got, requests)
	}
}

func TestServeHTTPReusesHTTP1TunnelWhenHTTP2Disabled(t *testing.T) {
	protocols := make(chan string, 2)
	backend, roots := newTLSBackend(t, testClusterName, true, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		protocols <- request.Proto
		_, _ = io.WriteString(w, "ok")
	}))
	server, tunnelCount := newProxyToBackend(t, roots, backend.Listener.Addr().String(), false)
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	for range 2 {
		getResponse(t, proxy.Client(), proxy.URL+kubeAPIPath(testClusterName))
	}

	for range 2 {
		if protocol := <-protocols; protocol != "HTTP/1.1" {
			t.Fatalf("backend protocol = %q, want HTTP/1.1", protocol)
		}
	}
	if got := tunnelCount.Load(); got != 1 {
		t.Fatalf("created %d HTTP/1.1 tunnels for two sequential requests, want 1", got)
	}
}

func TestServeHTTPStreamsWithoutGlobalFlushInterval(t *testing.T) {
	backendFlushed := make(chan struct{})
	releaseBackend := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBackend) }) }
	t.Cleanup(release)

	backend, roots := newTLSBackend(t, testClusterName, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first event\n")
		w.(http.Flusher).Flush()
		close(backendFlushed)
		<-releaseBackend
		_, _ = io.WriteString(w, "second event\n")
	}))
	server, _ := newProxyToBackend(t, roots, backend.Listener.Addr().String(), true)
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	line := make(chan string, 1)
	errResult := make(chan error, 1)
	go func() {
		response, err := proxy.Client().Get(proxy.URL + kubeAPIPath(testClusterName))
		if err != nil {
			errResult <- err
			return
		}
		defer response.Body.Close()
		firstLine, err := bufio.NewReader(response.Body).ReadString('\n')
		if err != nil {
			errResult <- err
			return
		}
		line <- firstLine
	}()

	waitForSignal(t, backendFlushed, "backend to flush its first event")
	select {
	case got := <-line:
		if got != "first event\n" {
			t.Fatalf("first streamed line = %q", got)
		}
	case err := <-errResult:
		t.Fatalf("read stream: %v", err)
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("first event was not delivered while the backend response remained open")
	}
	release()
}

func TestServeHTTPUpgradeUsesDedicatedHTTP1Tunnel(t *testing.T) {
	backendProtocol := make(chan string, 1)
	backend, roots := newTLSBackend(t, testClusterName, true, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		backendProtocol <- request.Proto
		connection, readWriter, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprintf(readWriter, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		if err := readWriter.Flush(); err != nil {
			t.Errorf("backend flush upgrade: %v", err)
			return
		}
		message, err := readWriter.ReadString('\n')
		if err != nil {
			t.Errorf("backend read upgraded connection: %v", err)
			return
		}
		_, _ = fmt.Fprintf(readWriter, "echo: %s", message)
		_ = readWriter.Flush()
	}))
	server, tunnelCount := newProxyToBackend(t, roots, backend.Listener.Addr().String(), true)
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	address := strings.TrimPrefix(proxy.URL, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial user-server: %v", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, _ = fmt.Fprintf(connection,
		"GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n",
		kubeAPIPath(testClusterName), address,
	)
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade status: %v", err)
	}
	if status != "HTTP/1.1 101 Switching Protocols\r\n" {
		t.Fatalf("upgrade status = %q", status)
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade header: %v", err)
		}
		if header == "\r\n" {
			break
		}
	}
	_, _ = io.WriteString(connection, "ping\n")
	echo, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgraded response: %v", err)
	}
	if echo != "echo: ping\n" {
		t.Fatalf("upgraded response = %q", echo)
	}

	if protocol := <-backendProtocol; protocol != "HTTP/1.1" {
		t.Fatalf("upgrade backend protocol = %q, want HTTP/1.1", protocol)
	}
	if got := tunnelCount.Load(); got != 1 {
		t.Fatalf("upgrade created %d tunnels, want 1", got)
	}
	server.transportPool.mu.Lock()
	cached := len(server.transportPool.transports)
	server.transportPool.mu.Unlock()
	if cached != 0 {
		t.Fatalf("upgrade populated the shared transport cache with %d entries", cached)
	}
}

func newTestUserServer(
	roots *x509.CertPool,
	getTunnel func(context.Context) (konnectivity.Tunnel, error),
) *userServer {
	server := newUserServer()
	server.serviceProxyRootCA = roots
	server.serviceAllowlist = &ServiceAllowlist{}
	server.getTunnel = getTunnel
	return server
}

func newProxyToBackend(
	t *testing.T,
	roots *x509.CertPool,
	backendAddress string,
	enableHTTP2 bool,
) (*userServer, *atomic.Int64) {
	t.Helper()
	tunnelCount := &atomic.Int64{}
	server := newTestUserServer(roots, func(context.Context) (konnectivity.Tunnel, error) {
		tunnelCount.Add(1)
		return newDialingTunnel(backendAddress), nil
	})
	server.enableHTTP2 = enableHTTP2
	server.transportPool.allow(testClusterName)
	return server, tunnelCount
}

func newTLSBackend(
	t *testing.T,
	clusterName string,
	enableHTTP2 bool,
	handler http.Handler,
) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	target, err := url.Parse(serviceProxyURL(clusterName))
	if err != nil {
		t.Fatalf("parse service proxy URL: %v", err)
	}
	certificatePEM, keyPEM, err := certutil.GenerateSelfSignedCertKey(target.Hostname(), nil, nil)
	if err != nil {
		t.Fatalf("generate backend certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatalf("parse backend certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("add backend CA certificate")
	}

	backend := httptest.NewUnstartedServer(handler)
	backend.EnableHTTP2 = enableHTTP2
	backend.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	return backend, roots
}

func getResponse(t *testing.T, client *http.Client, requestURL string) string {
	t.Helper()
	response, err := client.Get(requestURL)
	if err != nil {
		t.Fatalf("GET %s: %v", requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", response.StatusCode, http.StatusOK, body)
	}
	return string(body)
}

func kubeAPIPath(clusterName string) string {
	return "/" + clusterName + "/api/v1/namespaces/default/configmaps"
}

func serviceProxyPath(clusterName string) string {
	return "/" + clusterName + "/api/v1/namespaces/default/services/http:example:80/proxy-service/"
}
