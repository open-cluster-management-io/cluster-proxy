package tokenreviewcache

import (
	"context"
	"fmt"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestCacheHitAndMiss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, 5*time.Second)

	user := &authenticationv1.UserInfo{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// miss before set
	_, found := c.Get("token-abc")
	if found {
		t.Fatal("expected cache miss for unseen token")
	}

	// set and hit
	c.Set("token-abc", user)
	info, found := c.Get("token-abc")
	if !found {
		t.Fatal("expected cache hit")
	}
	if info.Username != "test-user" {
		t.Fatalf("expected username test-user, got %s", info.Username)
	}
}

func TestCacheExpiration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, 50*time.Millisecond)

	user := &authenticationv1.UserInfo{Username: "u"}
	c.Set("tok", user)

	// should hit immediately
	_, found := c.Get("tok")
	if !found {
		t.Fatal("expected cache hit before expiry")
	}

	// wait for expiry
	time.Sleep(100 * time.Millisecond)

	_, found = c.Get("tok")
	if found {
		t.Fatal("expected cache miss after expiry")
	}
}

func TestCacheDoesNotStoreRawToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, 5*time.Second)

	c.Set("secret-token", &authenticationv1.UserInfo{Username: "u"})

	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.items {
		if key == "secret-token" {
			t.Fatal("cache stored raw token as key — must store hash only")
		}
	}
}

func TestCacheDeepCopiesOnSetAndGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, 5*time.Second)

	user := &authenticationv1.UserInfo{
		Username: "original",
		Groups:   []string{"g1"},
	}

	c.Set("tok", user)

	// mutate the original — should not affect cache
	user.Username = "mutated"
	user.Groups[0] = "mutated-group"

	info, _ := c.Get("tok")
	if info.Username != "original" {
		t.Fatalf("expected cached username 'original', got '%s'", info.Username)
	}
	if info.Groups[0] != "g1" {
		t.Fatalf("expected cached group 'g1', got '%s'", info.Groups[0])
	}

	// mutate the returned value — should not affect cache
	info.Username = "mutated-again"
	info2, _ := c.Get("tok")
	if info2.Username != "original" {
		t.Fatalf("expected cached username 'original' after mutating Get result, got '%s'", info2.Username)
	}
}

func TestLRUEviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewWithMaxSize(ctx, 5*time.Second, 3)

	for i := 0; i < 3; i++ {
		c.Set(fmt.Sprintf("tok-%d", i), &authenticationv1.UserInfo{Username: fmt.Sprintf("u%d", i)})
	}
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", c.Len())
	}

	// adding a 4th should evict tok-0 (least recently used)
	c.Set("tok-3", &authenticationv1.UserInfo{Username: "u3"})
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", c.Len())
	}

	_, found := c.Get("tok-0")
	if found {
		t.Fatal("expected tok-0 to be evicted (LRU)")
	}

	// tok-1, tok-2, tok-3 should still be present
	for _, tok := range []string{"tok-1", "tok-2", "tok-3"} {
		_, found := c.Get(tok)
		if !found {
			t.Fatalf("expected %s to still be in cache", tok)
		}
	}
}

func TestLRUPromotionOnGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewWithMaxSize(ctx, 5*time.Second, 3)

	c.Set("tok-0", &authenticationv1.UserInfo{Username: "u0"})
	c.Set("tok-1", &authenticationv1.UserInfo{Username: "u1"})
	c.Set("tok-2", &authenticationv1.UserInfo{Username: "u2"})

	// access tok-0, promoting it to most recently used
	c.Get("tok-0")

	// add tok-3 — should evict tok-1 (now the least recently used), not tok-0
	c.Set("tok-3", &authenticationv1.UserInfo{Username: "u3"})

	_, found := c.Get("tok-1")
	if found {
		t.Fatal("expected tok-1 to be evicted (LRU after tok-0 was promoted)")
	}

	_, found = c.Get("tok-0")
	if !found {
		t.Fatal("expected tok-0 to survive (was promoted by Get)")
	}
}

func TestUpdateExistingEntry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(ctx, 5*time.Second)

	c.Set("tok", &authenticationv1.UserInfo{Username: "v1"})

	info, found := c.Get("tok")
	if !found {
		t.Fatal("expected cache hit")
	}
	if info.Username != "v1" {
		t.Fatalf("expected username 'v1', got '%s'", info.Username)
	}

	// update same token
	c.Set("tok", &authenticationv1.UserInfo{Username: "v2"})

	info, found = c.Get("tok")
	if !found {
		t.Fatal("expected cache hit after update")
	}
	if info.Username != "v2" {
		t.Fatalf("expected username 'v2', got '%s'", info.Username)
	}

	if c.Len() != 1 {
		t.Fatalf("expected 1 entry (update, not duplicate), got %d", c.Len())
	}
}

func TestEvictionGoroutineStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := New(ctx, 50*time.Millisecond)

	c.Set("tok", &authenticationv1.UserInfo{Username: "u"})

	cancel()

	// give goroutine time to exit
	time.Sleep(100 * time.Millisecond)

	// cache should still be usable (just no background eviction)
	_, found := c.Get("tok")
	// entry may or may not be expired by now, we just verify no panic
	_ = found
}
