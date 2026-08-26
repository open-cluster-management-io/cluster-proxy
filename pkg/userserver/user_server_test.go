package userserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	konnectivity "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

// fakeConn is a minimal net.Conn that satisfies the interface for tests.
type fakeConn struct{}

func (f *fakeConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (f *fakeConn) Write(b []byte) (int, error)        { return len(b), nil }
func (f *fakeConn) Close() error                       { return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

var _ net.Conn = (*fakeConn)(nil)

// fakeTunnel implements konnectivity.Tunnel and counts DialContext calls.
type fakeTunnel struct {
	dialCount int64
	done      chan struct{}
}

func newFakeTunnel() *fakeTunnel {
	t := &fakeTunnel{done: make(chan struct{})}
	close(t.done)
	return t
}

func (f *fakeTunnel) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	atomic.AddInt64(&f.dialCount, 1)
	return &fakeConn{}, nil
}

func (f *fakeTunnel) Done() <-chan struct{} { return f.done }

var _ konnectivity.Tunnel = (*fakeTunnel)(nil)

// newTestUserServer creates a userServer via newUserServer() (so defaults are
// properly initialised) with a custom getTunnel for testing.
func newTestUserServer(getTunnel func(context.Context) (konnectivity.Tunnel, error)) *userServer {
	s := newUserServer()
	s.getTunnel = getTunnel
	return s
}

// --- getOrCreateTransport tests ---

func TestGetOrCreateTransport_ReturnsSameTransportForSameCluster(t *testing.T) {
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		return newFakeTunnel(), nil
	})

	t1 := s.getOrCreateTransport("cluster1")
	t2 := s.getOrCreateTransport("cluster1")

	if t1 != t2 {
		t.Fatal("expected same *http.Transport for same cluster, got different pointers")
	}
}

func TestGetOrCreateTransport_ReturnsDifferentTransportForDifferentClusters(t *testing.T) {
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		return newFakeTunnel(), nil
	})

	ta := s.getOrCreateTransport("cluster-a")
	tb := s.getOrCreateTransport("cluster-b")

	if ta == tb {
		t.Fatal("expected different *http.Transport for different clusters, got same pointer")
	}
}

func TestGetOrCreateTransport_Settings(t *testing.T) {
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		return newFakeTunnel(), nil
	})

	transport := s.getOrCreateTransport("cluster1")

	// MaxConnsPerHost should equal the resolved maxConnsPerHost on the struct.
	if transport.MaxConnsPerHost != s.maxConnsPerHost {
		t.Errorf("MaxConnsPerHost: got %d, want %d",
			transport.MaxConnsPerHost, s.maxConnsPerHost)
	}
	// MaxIdleConns should equal MaxIdleConnsPerHost (single-host transport).
	if transport.MaxIdleConns != s.maxIdleConnsPerHost {
		t.Errorf("MaxIdleConns: got %d, want %d (should match MaxIdleConnsPerHost)",
			transport.MaxIdleConns, s.maxIdleConnsPerHost)
	}
	if transport.MaxIdleConnsPerHost != s.maxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost: got %d, want %d",
			transport.MaxIdleConnsPerHost, s.maxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != s.idleConnTimeout {
		t.Errorf("IdleConnTimeout: got %v, want %v",
			transport.IdleConnTimeout, s.idleConnTimeout)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must not be nil")
	}

	// Verify hardcoded defaults.
	if s.maxConnsPerHost != 10 {
		t.Errorf("default maxConnsPerHost: got %d, want 10", s.maxConnsPerHost)
	}
	if s.maxIdleConnsPerHost != 10 {
		t.Errorf("default maxIdleConnsPerHost: got %d, want 10", s.maxIdleConnsPerHost)
	}
	if s.idleConnTimeout != 90*time.Second {
		t.Errorf("default idleConnTimeout: got %v, want 90s", s.idleConnTimeout)
	}
}

func TestGetOrCreateTransport_ConcurrentAccessReturnsSameTransport(t *testing.T) {
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		return newFakeTunnel(), nil
	})

	const goroutines = 50
	results := make([]*http.Transport, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = s.getOrCreateTransport("cluster1")
		}()
	}
	wg.Wait()

	first := results[0]
	for i, tr := range results {
		if tr != first {
			t.Errorf("goroutine %d got a different transport pointer", i)
		}
	}
}

func TestGetOrCreateTransport_DialContextCreatesNewTunnelOnPoolMiss(t *testing.T) {
	var tunnelCount int64
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		atomic.AddInt64(&tunnelCount, 1)
		return newFakeTunnel(), nil
	})

	transport := s.getOrCreateTransport("cluster1")

	// First pool miss: expect one tunnel.
	conn, err := transport.DialContext(context.Background(), "tcp", "cluster1.proxy:7443")
	if err != nil {
		t.Fatalf("first DialContext: %v", err)
	}
	conn.Close()

	if got := atomic.LoadInt64(&tunnelCount); got != 1 {
		t.Errorf("after first pool miss: got %d tunnels, want 1", got)
	}

	// Second pool miss: expect a second tunnel (each tunnel is single-use).
	conn2, err := transport.DialContext(context.Background(), "tcp", "cluster1.proxy:7443")
	if err != nil {
		t.Fatalf("second DialContext: %v", err)
	}
	conn2.Close()

	if got := atomic.LoadInt64(&tunnelCount); got != 2 {
		t.Errorf("after second pool miss: got %d tunnels, want 2", got)
	}
}

func TestGetOrCreateTransport_TunnelNotCreatedOnCacheLookup(t *testing.T) {
	// Verifies that merely looking up the cached transport does not call
	// getTunnel -- tunnels are created lazily on DialContext (pool miss).
	var tunnelCount int64
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		atomic.AddInt64(&tunnelCount, 1)
		return newFakeTunnel(), nil
	})

	// Two cache lookups -- no DialContext calls yet.
	_ = s.getOrCreateTransport("cluster1")
	_ = s.getOrCreateTransport("cluster1")

	if got := atomic.LoadInt64(&tunnelCount); got != 0 {
		t.Errorf("getTunnel must not be called during cache lookup, got %d calls", got)
	}
}

func TestServeHTTP_DifferentClustersGetDifferentTransports(t *testing.T) {
	s := newTestUserServer(func(_ context.Context) (konnectivity.Tunnel, error) {
		return newFakeTunnel(), nil
	})

	ta := s.getOrCreateTransport("cluster-a")
	tb := s.getOrCreateTransport("cluster-b")

	if ta == tb {
		t.Error("different clusters must have different cached transports")
	}
	if s.getOrCreateTransport("cluster-a") != ta {
		t.Error("second lookup for cluster-a must return the same transport")
	}
	if s.getOrCreateTransport("cluster-b") != tb {
		t.Error("second lookup for cluster-b must return the same transport")
	}
}
