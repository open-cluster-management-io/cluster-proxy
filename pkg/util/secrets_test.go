package util

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"

	certutil "k8s.io/client-go/util/cert"
)

func TestBuildTLSConfigCABundleValidation(t *testing.T) {
	certPEM, keyPEM := newTestCertPEM(t)
	nonCertificateBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a private key")})
	malformedCertificateBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a DER-encoded certificate")})

	testcases := []struct {
		name    string
		caData  []byte
		wantErr bool
	}{
		{
			name:   "valid bundle",
			caData: certPEM,
		},
		{
			name:   "non-certificate block is ignored",
			caData: slices.Concat(certPEM, nonCertificateBlock),
		},
		{
			name:    "malformed certificate block is rejected",
			caData:  slices.Concat(certPEM, malformedCertificateBlock),
			wantErr: true,
		},
		{
			name:   "truncated PEM block is skipped",
			caData: slices.Concat(certPEM, []byte("-----BEGIN CERTIFICATE-----\nMIIBkTCCATeg\n")),
		},
		{
			name:    "bundle without certificates is rejected",
			caData:  []byte("no certificates here"),
			wantErr: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg, err := buildTLSConfig(tc.caData, certPEM, keyPEM, "proxy.test", nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tlsCfg.RootCAs == nil {
				t.Fatal("expected RootCAs to be set")
			}
		})
	}
}

func newTestCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: "test-ca"}, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
