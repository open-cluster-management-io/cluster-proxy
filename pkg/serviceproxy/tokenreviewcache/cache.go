package tokenreviewcache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// entry holds a cached TokenReview result with an expiration time.
type entry struct {
	authenticated bool
	user          *authenticationv1.UserInfo
	expiresAt     time.Time
}

// Cache provides a thread-safe TTL cache for TokenReview results.
// It uses a token hash as the key to avoid storing raw tokens in memory.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*entry
	ttl     time.Duration
}

// New creates a new TokenReview cache with the given TTL for cached entries.
func New(ttl time.Duration) *Cache {
	c := &Cache{
		entries: make(map[string]*entry),
		ttl:     ttl,
	}
	go c.startEviction()
	return c
}

// Get looks up a cached TokenReview result by token.
// Returns (authenticated, userInfo, found).
func (c *Cache) Get(token string) (bool, *authenticationv1.UserInfo, bool) {
	key := hashToken(token)

	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || time.Now().After(e.expiresAt) {
		return false, nil, false
	}

	return e.authenticated, e.user, true
}

// Set stores a TokenReview result in the cache.
func (c *Cache) Set(token string, authenticated bool, user *authenticationv1.UserInfo) {
	key := hashToken(token)

	c.mu.Lock()
	c.entries[key] = &entry{
		authenticated: authenticated,
		user:          user.DeepCopy(),
		expiresAt:     time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// startEviction periodically removes expired entries to prevent memory leaks.
func (c *Cache) startEviction() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()

	for range ticker.C {
		c.evict()
	}
}

func (c *Cache) evict() {
	now := time.Now()

	c.mu.Lock()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// hashToken returns a hex-encoded SHA-256 hash of the token.
// We never store raw tokens in the cache.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
