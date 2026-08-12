package serviceproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"

	certutil "k8s.io/client-go/util/cert"
)

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

func TestLoadRootCAs(t *testing.T) {
	caPEM := newTestCAPEM(t)
	additionalCAPEM := newTestCAPEM(t)
	malformedBundle := slices.Concat(caPEM, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: []byte("not a DER-encoded certificate"),
	}))

	tests := []struct {
		name                 string
		rootCA               []byte
		additionalCA         []byte
		additionalCAMissing  bool
		additionalCAIsDir    bool
		wantErr              bool
		wantAdditionalLoaded bool
	}{
		{
			name:   "root CA only",
			rootCA: caPEM,
		},
		{
			name:                "missing additional CA file is tolerated",
			rootCA:              caPEM,
			additionalCAMissing: true,
		},
		{
			name:                 "additional CA is loaded",
			rootCA:               caPEM,
			additionalCA:         additionalCAPEM,
			wantAdditionalLoaded: true,
		},
		{
			name:              "unreadable additional CA file is rejected",
			rootCA:            caPEM,
			additionalCAIsDir: true,
			wantErr:           true,
		},
		{
			name:    "malformed root CA is rejected",
			rootCA:  malformedBundle,
			wantErr: true,
		},
		{
			name:         "malformed additional CA is rejected",
			rootCA:       caPEM,
			additionalCA: malformedBundle,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rootCAFile := filepath.Join(dir, "ca.crt")
			if err := os.WriteFile(rootCAFile, tt.rootCA, 0600); err != nil {
				t.Fatal(err)
			}
			additionalCAFile := ""
			switch {
			case tt.additionalCAMissing:
				additionalCAFile = filepath.Join(dir, "missing.crt")
			case tt.additionalCAIsDir:
				additionalCAFile = dir
			case tt.additionalCA != nil:
				additionalCAFile = filepath.Join(dir, "additional.crt")
				if err := os.WriteFile(additionalCAFile, tt.additionalCA, 0600); err != nil {
					t.Fatal(err)
				}
			}

			rootCAs, additionalCALoaded, err := loadRootCAs(rootCAFile, additionalCAFile)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantPool, err := certutil.NewPoolFromBytes(slices.Concat(tt.rootCA, tt.additionalCA))
			if err != nil {
				t.Fatal(err)
			}
			if !rootCAs.Equal(wantPool) {
				t.Fatal("root CA pool does not contain the expected certificates")
			}
			if additionalCALoaded != tt.wantAdditionalLoaded {
				t.Fatalf("expected additionalCALoaded=%v, got %v", tt.wantAdditionalLoaded, additionalCALoaded)
			}
		})
	}
}

func newTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: "test-ca"}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
}
