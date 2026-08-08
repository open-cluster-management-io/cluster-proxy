package serviceproxy

import "testing"

func TestNewServiceProxy_DefaultValues(t *testing.T) {
	s := newServiceProxy()

	if s.tokenReviewCacheTTL != defaultTokenReviewCacheTTL {
		t.Fatalf("expected default TTL %v, got %v", defaultTokenReviewCacheTTL, s.tokenReviewCacheTTL)
	}
	if s.kubeClientQPS != defaultKubeClientQPS {
		t.Fatalf("expected default QPS %v, got %v", defaultKubeClientQPS, s.kubeClientQPS)
	}
	if s.kubeClientBurst != defaultKubeClientBurst {
		t.Fatalf("expected default burst %v, got %v", defaultKubeClientBurst, s.kubeClientBurst)
	}
}
