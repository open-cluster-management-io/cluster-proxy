package tokenreviewcache

import (
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

	c.mu.RLock()
	defer c.mu.RUnlock()

	for key := range c.entries {
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
