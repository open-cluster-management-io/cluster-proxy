package serviceproxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

type recordingRoundTripper struct {
	mu         sync.Mutex
	roundTrips int
	closeCalls int
}

func (t *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.roundTrips++
	t.mu.Unlock()

	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader("ok")),
		ContentLength: 2,
		Request:       req,
	}, nil
}

func (t *recordingRoundTripper) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeCalls++
}

func (t *recordingRoundTripper) counts() (roundTrips, closeCalls int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.roundTrips, t.closeCalls
}

func TestNewProxyTransportPreservesConfiguration(t *testing.T) {
	rootCAs := x509.NewCertPool()
	s := &serviceProxy{
		rootCAs:               rootCAs,
		maxIdleConns:          17,
		idleConnTimeout:       23 * time.Second,
		tLSHandshakeTimeout:   29 * time.Second,
		expectContinueTimeout: 31 * time.Second,
	}

	transport := s.newProxyTransport()

	if transport.DialContext == nil {
		t.Fatal("DialContext is not configured")
	}
	if transport.MaxIdleConns != s.maxIdleConns {
		t.Fatalf("unexpected MaxIdleConns: got %d, want %d", transport.MaxIdleConns, s.maxIdleConns)
	}
	if transport.IdleConnTimeout != s.idleConnTimeout {
		t.Fatalf("unexpected IdleConnTimeout: got %v, want %v", transport.IdleConnTimeout, s.idleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != s.tLSHandshakeTimeout {
		t.Fatalf("unexpected TLSHandshakeTimeout: got %v, want %v", transport.TLSHandshakeTimeout, s.tLSHandshakeTimeout)
	}
	if transport.ExpectContinueTimeout != s.expectContinueTimeout {
		t.Fatalf(
			"unexpected ExpectContinueTimeout: got %v, want %v",
			transport.ExpectContinueTimeout,
			s.expectContinueTimeout,
		)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is not configured")
	}
	if transport.TLSClientConfig.RootCAs != rootCAs {
		t.Fatal("transport does not use the configured root CAs")
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("unexpected minimum TLS version: %d", transport.TLSClientConfig.MinVersion)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must remain disabled for SPDY upgrades")
	}
}

func TestServeHTTPReusesSharedTransport(t *testing.T) {
	transport := &recordingRoundTripper{}
	s := &serviceProxy{proxyTransport: transport}

	sendRequest := func(t *testing.T) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://service-proxy.example/healthz", nil)
		req.Header.Set(utils.HeaderClusterProxyProto, "http")
		req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
		req.Header.Set(utils.HeaderClusterProxyService, "backend")
		req.Header.Set(utils.HeaderClusterProxyPort, "8080")
		recorder := httptest.NewRecorder()

		s.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Errorf("unexpected status: got %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		if recorder.Body.String() != "ok" {
			t.Errorf("unexpected response body: %q", recorder.Body.String())
		}
	}

	const sequentialRequests = 3
	for i := 0; i < sequentialRequests; i++ {
		sendRequest(t)
	}

	const concurrentRequests = 20
	var wg sync.WaitGroup
	wg.Add(concurrentRequests)
	for i := 0; i < concurrentRequests; i++ {
		go func() {
			defer wg.Done()
			sendRequest(t)
		}()
	}
	wg.Wait()

	roundTrips, _ := transport.counts()
	if want := sequentialRequests + concurrentRequests; roundTrips != want {
		t.Fatalf("shared transport handled %d requests, want %d", roundTrips, want)
	}
}

func TestServeHTTPRequiresInitializedTransportForForwarding(t *testing.T) {
	s := &serviceProxy{}
	req := httptest.NewRequest(http.MethodGet, "https://service-proxy.example/healthz", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "http")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "backend")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")
	recorder := httptest.NewRecorder()

	s.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status: got %d, want 500: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCloseIdleConnectionsUsesSharedTransport(t *testing.T) {
	transport := &recordingRoundTripper{}
	s := &serviceProxy{proxyTransport: transport}

	s.closeIdleConnections()

	_, closeCalls := transport.counts()
	if closeCalls != 1 {
		t.Fatalf("CloseIdleConnections called %d times, want 1", closeCalls)
	}

	(&serviceProxy{}).closeIdleConnections()
}

var _ closeIdleRoundTripper = (*recordingRoundTripper)(nil)
