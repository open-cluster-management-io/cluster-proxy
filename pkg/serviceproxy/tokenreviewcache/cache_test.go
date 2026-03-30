package tokenreviewcache

import (
	"fmt"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestCacheHitAndMiss(t *testing.T) {
	c := New(5 * time.Second)

	user := &authenticationv1.UserInfo{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// miss before set
	_, _, found := c.Get("token-abc")
	if found {
		t.Fatal("expected cache miss for unseen token")
	}

	// set and hit
	c.Set("token-abc", true, user)
	auth, info, found := c.Get("token-abc")
	if !found {
		t.Fatal("expected cache hit")
	}
	if !auth {
		t.Fatal("expected authenticated=true")
	}
	if info.Username != "test-user" {
		t.Fatalf("expected username test-user, got %s", info.Username)
	}
}

func TestCacheExpiration(t *testing.T) {
	c := New(50 * time.Millisecond)

	user := &authenticationv1.UserInfo{Username: "u"}
	c.Set("tok", true, user)

	// should hit immediately
	_, _, found := c.Get("tok")
	if !found {
		t.Fatal("expected cache hit before expiry")
	}

	// wait for expiry
	time.Sleep(100 * time.Millisecond)

	_, _, found = c.Get("tok")
	if found {
		t.Fatal("expected cache miss after expiry")
	}
}

func TestCacheUnauthenticated(t *testing.T) {
	c := New(5 * time.Second)

	c.Set("bad-token", false, &authenticationv1.UserInfo{})
	auth, _, found := c.Get("bad-token")
	if !found {
		t.Fatal("expected cache hit for unauthenticated token")
	}
	if auth {
		t.Fatal("expected authenticated=false for unauthenticated token")
	}
}

func TestCacheDoesNotStoreRawToken(t *testing.T) {
	c := New(5 * time.Second)

	c.Set("secret-token", true, &authenticationv1.UserInfo{Username: "u"})

	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.items {
		if key == "secret-token" {
			t.Fatal("cache stored raw token as key — must store hash only")
		}
	}
}

func TestCacheDeepCopiesUserInfo(t *testing.T) {
	c := New(5 * time.Second)

	user := &authenticationv1.UserInfo{
		Username: "original",
		Groups:   []string{"g1"},
	}

	c.Set("tok", true, user)

	// mutate the original
	user.Username = "mutated"
	user.Groups[0] = "mutated-group"

	_, info, _ := c.Get("tok")
	if info.Username != "original" {
		t.Fatalf("expected cached username 'original', got '%s'", info.Username)
	}
	if info.Groups[0] != "g1" {
		t.Fatalf("expected cached group 'g1', got '%s'", info.Groups[0])
	}
}

func TestLRUEviction(t *testing.T) {
	c := NewWithMaxSize(5*time.Second, 3)

	for i := 0; i < 3; i++ {
		c.Set(fmt.Sprintf("tok-%d", i), true, &authenticationv1.UserInfo{Username: fmt.Sprintf("u%d", i)})
	}
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", c.Len())
	}

	// adding a 4th should evict tok-0 (least recently used)
	c.Set("tok-3", true, &authenticationv1.UserInfo{Username: "u3"})
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", c.Len())
	}

	_, _, found := c.Get("tok-0")
	if found {
		t.Fatal("expected tok-0 to be evicted (LRU)")
	}

	// tok-1, tok-2, tok-3 should still be present
	for _, tok := range []string{"tok-1", "tok-2", "tok-3"} {
		_, _, found := c.Get(tok)
		if !found {
			t.Fatalf("expected %s to still be in cache", tok)
		}
	}
}

func TestLRUPromotionOnGet(t *testing.T) {
	c := NewWithMaxSize(5*time.Second, 3)

	c.Set("tok-0", true, &authenticationv1.UserInfo{Username: "u0"})
	c.Set("tok-1", true, &authenticationv1.UserInfo{Username: "u1"})
	c.Set("tok-2", true, &authenticationv1.UserInfo{Username: "u2"})

	// access tok-0, promoting it to most recently used
	c.Get("tok-0")

	// add tok-3 — should evict tok-1 (now the least recently used), not tok-0
	c.Set("tok-3", true, &authenticationv1.UserInfo{Username: "u3"})

	_, _, found := c.Get("tok-1")
	if found {
		t.Fatal("expected tok-1 to be evicted (LRU after tok-0 was promoted)")
	}

	_, _, found = c.Get("tok-0")
	if !found {
		t.Fatal("expected tok-0 to survive (was promoted by Get)")
	}
}

func TestUpdateExistingEntry(t *testing.T) {
	c := New(5 * time.Second)

	c.Set("tok", false, &authenticationv1.UserInfo{})

	auth, _, found := c.Get("tok")
	if !found || auth {
		t.Fatal("expected unauthenticated entry")
	}

	// update same token to authenticated
	c.Set("tok", true, &authenticationv1.UserInfo{Username: "now-valid"})

	auth, info, found := c.Get("tok")
	if !found || !auth {
		t.Fatal("expected authenticated entry after update")
	}
	if info.Username != "now-valid" {
		t.Fatalf("expected username 'now-valid', got '%s'", info.Username)
	}

	if c.Len() != 1 {
		t.Fatalf("expected 1 entry (update, not duplicate), got %d", c.Len())
	}
}
