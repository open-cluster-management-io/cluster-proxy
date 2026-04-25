package controllers

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

func newTestConfig(replicas int32, additionalArgs ...string) *proxyv1alpha1.ManagedProxyConfiguration {
	return &proxyv1alpha1.ManagedProxyConfiguration{
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Replicas:       replicas,
				AdditionalArgs: additionalArgs,
			},
		},
	}
}

var baseArgs = []string{
	"--server-count=3",
	"--proxy-strategies=destHost",
	"--server-ca-cert=/etc/server-ca-pki/ca.crt",
	"--server-cert=/etc/server-pki/tls.crt",
	"--server-key=/etc/server-pki/tls.key",
	"--cluster-ca-cert=/etc/server-ca-pki/ca.crt",
	"--cluster-cert=/etc/agent-pki/tls.crt",
	"--cluster-key=/etc/agent-pki/tls.key",
}

func TestProxyServerArgs_NilTLSConfig(t *testing.T) {
	args := proxyServerArgs(newTestConfig(3), nil)
	assert.Equal(t, baseArgs, args)
}

func TestProxyServerArgs_EmptyCipherSuites(t *testing.T) {
	args := proxyServerArgs(newTestConfig(3), &sdktls.TLSConfig{})
	assert.Equal(t, baseArgs, args)
}

func TestProxyServerArgs_WithCipherSuites(t *testing.T) {
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
	}
	args := proxyServerArgs(newTestConfig(3), tlsConfig)

	expected := append(append([]string{}, baseArgs...),
		"--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites),
	)
	assert.Equal(t, expected, args)
}

func TestProxyServerArgs_WithAdditionalArgs(t *testing.T) {
	config := newTestConfig(3, "--extra-flag=value")
	args := proxyServerArgs(config, nil)

	expected := append(append([]string{}, baseArgs...), "--extra-flag=value")
	assert.Equal(t, expected, args)
}

func TestProxyServerArgs_WithAdditionalArgsAndCipherSuites(t *testing.T) {
	config := newTestConfig(3, "--extra-flag=value")
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	args := proxyServerArgs(config, tlsConfig)

	expected := append(append([]string{}, baseArgs...),
		"--extra-flag=value",
		"--cipher-suites="+sdktls.CipherSuitesToString(tlsConfig.CipherSuites),
	)
	assert.Equal(t, expected, args)
}

func TestTLSConfigHash_Nil(t *testing.T) {
	assert.Equal(t, "", tlsConfigHash(nil))
}

func TestTLSConfigHash_Deterministic(t *testing.T) {
	tlsConfig := &sdktls.TLSConfig{
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		MinVersion: tls.VersionTLS12,
	}
	hash1 := tlsConfigHash(tlsConfig)
	hash2 := tlsConfigHash(tlsConfig)
	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 16)
}

func TestTLSConfigHash_DiffersOnChange(t *testing.T) {
	config1 := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	config2 := &sdktls.TLSConfig{
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
	}
	assert.NotEqual(t, tlsConfigHash(config1), tlsConfigHash(config2))
}

func TestTLSConfigHash_EmptyConfig(t *testing.T) {
	hash := tlsConfigHash(&sdktls.TLSConfig{})
	assert.NotEmpty(t, hash)
	assert.Len(t, hash, 16)
}
