package userserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	fakeaddon "open-cluster-management.io/api/client/addon/clientset/versioned/fake"

	"open-cluster-management.io/cluster-proxy/pkg/constant"
)

func TestClusterTransportPool(t *testing.T) {
	var created atomic.Int64
	pool := newClusterTransportPool(func(string) *http.Transport {
		created.Add(1)
		return &http.Transport{}
	})

	if _, ok := pool.getOrCreate("cluster1"); ok {
		t.Fatal("an unknown cluster must not get a transport")
	}

	pool.allow("cluster1")
	first, ok := pool.getOrCreate("cluster1")
	if !ok {
		t.Fatal("an allowed cluster must get a transport")
	}
	second, ok := pool.getOrCreate("cluster1")
	if !ok || second != first {
		t.Fatal("the same cluster must reuse its transport")
	}

	pool.allow("cluster2")
	other, ok := pool.getOrCreate("cluster2")
	if !ok || other == first {
		t.Fatal("different clusters must use different transports")
	}
	if got := created.Load(); got != 2 {
		t.Fatalf("created %d transports, want 2", got)
	}

	pool.remove("cluster1")
	if _, ok := pool.getOrCreate("cluster1"); ok {
		t.Fatal("a removed cluster must not get a transport")
	}

	pool.allow("cluster1")
	readded, ok := pool.getOrCreate("cluster1")
	if !ok || readded == first {
		t.Fatal("a re-added cluster must get a fresh transport")
	}
}

func TestClusterTransportPoolConcurrentGetOrCreate(t *testing.T) {
	tests := []struct {
		name      string
		warmCache bool
	}{
		{name: "cold cache"},
		{name: "warm cache", warmCache: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var created atomic.Int64
			pool := newClusterTransportPool(func(string) *http.Transport {
				created.Add(1)
				return &http.Transport{}
			})
			pool.allow("cluster1")
			if test.warmCache {
				pool.getOrCreate("cluster1")
			}

			const goroutines = 64
			results := make([]*http.Transport, goroutines)
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					results[index], _ = pool.getOrCreate("cluster1")
				}(i)
			}
			wg.Wait()

			for i, result := range results {
				if result != results[0] {
					t.Fatalf("goroutine %d received a different transport", i)
				}
			}
			if got := created.Load(); got != 1 {
				t.Fatalf("created %d transports, want 1", got)
			}
		})
	}
}

func TestClusterTransportPoolRemoveWinsFinalRace(t *testing.T) {
	pool := newClusterTransportPool(func(string) *http.Transport { return &http.Transport{} })
	pool.allow("cluster1")

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			pool.allow("cluster1")
			_, _ = pool.getOrCreate("cluster1")
		}()
		go func() {
			defer wg.Done()
			pool.remove("cluster1")
		}()
	}
	wg.Wait()

	pool.remove("cluster1")
	if _, ok := pool.getOrCreate("cluster1"); ok {
		t.Fatal("the final remove must prevent a later transport creation")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if _, ok := pool.transports["cluster1"]; ok {
		t.Fatal("the final remove left a cached transport")
	}
}

func TestClusterTransportPoolClosesIdleConnections(t *testing.T) {
	tests := map[string]func(*clusterTransportPool){
		"remove": func(pool *clusterTransportPool) { pool.remove("cluster1") },
		"close":  func(pool *clusterTransportPool) { pool.closeAll() },
	}

	for name, closePool := range tests {
		t.Run(name, func(t *testing.T) {
			idle := make(chan struct{})
			closed := make(chan struct{})
			var idleOnce sync.Once
			var closedOnce sync.Once
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				switch state {
				case http.StateIdle:
					idleOnce.Do(func() { close(idle) })
				case http.StateClosed:
					closedOnce.Do(func() { close(closed) })
				}
			}
			backend.Start()
			t.Cleanup(backend.Close)

			pool := newClusterTransportPool(func(string) *http.Transport { return &http.Transport{} })
			pool.allow("cluster1")
			transport, _ := pool.getOrCreate("cluster1")
			response, err := transport.RoundTrip(mustRequest(t, backend.URL))
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			waitForSignal(t, idle, "backend connection to become idle")

			closePool(pool)
			waitForSignal(t, closed, "idle backend connection to close")
			if pool.isAllowed("cluster1") {
				t.Fatal("closed pool still allows cluster1")
			}
		})
	}
}

func TestStartManagedClusterAddonWatcher(t *testing.T) {
	cluster1Addon := newManagedClusterAddon("cluster1", constant.AddonName)
	unrelatedAddon := newManagedClusterAddon("cluster2", "other-addon")
	client := fakeaddon.NewSimpleClientset(cluster1Addon, unrelatedAddon)
	pool := newClusterTransportPool(func(string) *http.Transport { return &http.Transport{} })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := startManagedClusterAddonWatcher(ctx, client, pool); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	if !pool.isAllowed("cluster1") {
		t.Fatal("initial informer list did not allow cluster1")
	}
	if pool.isAllowed("cluster2") {
		t.Fatal("an unrelated add-on must not allow cluster2")
	}

	cluster3Addon := newManagedClusterAddon("cluster3", constant.AddonName)
	if _, err := client.AddonV1beta1().ManagedClusterAddOns("cluster3").Create(
		ctx, cluster3Addon, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create add-on: %v", err)
	}
	waitForCondition(t, func() bool { return pool.isAllowed("cluster3") }, "cluster3 to become allowed")

	pool.getOrCreate("cluster1")
	if err := client.AddonV1beta1().ManagedClusterAddOns("cluster1").Delete(
		ctx, constant.AddonName, metav1.DeleteOptions{},
	); err != nil {
		t.Fatalf("delete add-on: %v", err)
	}
	waitForCondition(t, func() bool { return !pool.isAllowed("cluster1") }, "cluster1 to become disallowed")
	pool.mu.Lock()
	_, cached := pool.transports["cluster1"]
	pool.mu.Unlock()
	if cached {
		t.Fatal("add-on deletion did not evict the cluster1 transport")
	}
}

func TestDeletedClusterProxyAddonTombstone(t *testing.T) {
	want := newManagedClusterAddon("cluster1", constant.AddonName)
	got, ok := deletedClusterProxyAddon(cache.DeletedFinalStateUnknown{Key: "cluster1/cluster-proxy", Obj: want})
	if !ok || got != want {
		t.Fatal("valid tombstone was not decoded")
	}
}

func newManagedClusterAddon(namespace, name string) *addonv1beta1.ManagedClusterAddOn {
	return &addonv1beta1.ManagedClusterAddOn{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return request
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
