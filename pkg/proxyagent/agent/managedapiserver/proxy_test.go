package managedapiserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestTargetAddress(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		expected string
	}{
		{
			name:     "https default port",
			host:     "https://managed.example.com",
			expected: "managed.example.com:443",
		},
		{
			name:     "https explicit port",
			host:     "https://managed.example.com:6443",
			expected: "managed.example.com:6443",
		},
		{
			name:     "http default port",
			host:     "http://managed.example.com",
			expected: "managed.example.com:80",
		},
		{
			name:     "ipv6 default port",
			host:     "https://[::1]",
			expected: "[::1]:443",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual, err := targetAddress(c.host)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if actual != c.expected {
				t.Fatalf("expected %q, got %q", c.expected, actual)
			}
		})
	}

	if _, err := targetAddress("ftp://managed.example.com"); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}

func TestProxyRelaysRawTCPBytes(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen upstream: %v", err)
	}
	defer upstream.Close()

	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("relay:" + string(buf[:n])))
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate listen address: %v", err)
	}
	listenAddress := listener.Addr().String()
	_ = listener.Close()

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	if err := os.WriteFile(kubeconfigPath, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://%s
contexts:
- name: managed
  context:
    cluster: managed
    user: cluster-proxy
current-context: managed
users:
- name: cluster-proxy
  user:
    token: token
`, upstream.Addr().String())), 0600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Proxy{
			ManagedKubeconfig:      kubeconfigPath,
			Listen:                 listenAddress,
			DialTimeout:            time.Second,
			HealthProbeBindAddress: "127.0.0.1:0",
		}).Run(ctx)
	}()

	conn, err := dialEventually(ctx, listenAddress)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("tls-client-hello")); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read proxy response: %v", err)
	}
	if string(buf[:n]) != "relay:tls-client-hello" {
		t.Fatalf("unexpected proxy response %q", string(buf[:n]))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("unexpected proxy error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for proxy shutdown")
	}
}

func dialEventually(ctx context.Context, address string) (net.Conn, error) {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}
