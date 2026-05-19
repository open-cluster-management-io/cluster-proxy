package servicerelay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

func TestServiceRelayRestoresAuthorizationAndStripsInternalHeaders(t *testing.T) {
	var captured *http.Request
	relay := &ServiceRelay{
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Clone(req.Context())
			captured.Header = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     http.Header{"X-Backend": []string{"ok"}},
				Body:       io.NopCloser(strings.NewReader("backend-body")),
				Request:    req,
			}, nil
		}),
	}

	req := httptest.NewRequest("GET", "http://relay/ping?x=1", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "http")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "hello")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")
	req.Header.Set("Authorization", "Bearer managed-token")
	req.Header.Set(utils.HeaderClusterProxyAuthorization, "Bearer original-token")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, recorder.Code)
	}
	if recorder.Header().Get("X-Backend") != "ok" {
		t.Fatalf("expected backend header to be proxied")
	}
	if strings.TrimSpace(recorder.Body.String()) != "backend-body" {
		t.Fatalf("unexpected body %q", recorder.Body.String())
	}
	if captured == nil {
		t.Fatal("expected backend request to be captured")
	}
	if captured.URL.Scheme != "http" || captured.URL.Host != "hello.default.svc:8080" || captured.URL.Path != "/ping" {
		t.Fatalf("unexpected target URL %s", captured.URL.String())
	}
	if captured.Header.Get("Authorization") != "Bearer original-token" {
		t.Fatalf("expected original authorization to be restored, got %q", captured.Header.Get("Authorization"))
	}
	for _, header := range []string{
		utils.HeaderClusterProxyProto,
		utils.HeaderClusterProxyNamespace,
		utils.HeaderClusterProxyService,
		utils.HeaderClusterProxyPort,
		utils.HeaderClusterProxyAuthorization,
	} {
		if captured.Header.Get(header) != "" {
			t.Fatalf("expected header %s to be stripped, got %q", header, captured.Header.Get(header))
		}
	}
}

func TestServiceRelayRejectsKubeAPIServerTarget(t *testing.T) {
	relay := &ServiceRelay{}
	req := httptest.NewRequest("GET", "http://relay/healthz", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "https")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "kubernetes")
	req.Header.Set(utils.HeaderClusterProxyPort, "443")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
