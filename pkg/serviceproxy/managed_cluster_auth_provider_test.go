package serviceproxy

import (
	"net/http"
	"testing"

	"k8s.io/apiserver/pkg/authentication/user"
)

func TestManagedClusterAuthProviderApplyIdentityIsNoop(t *testing.T) {
	provider := &managedClusterAuthProvider{}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	if err := provider.ApplyIdentity(t.Context(), req, &user.DefaultInfo{Name: "alice"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
