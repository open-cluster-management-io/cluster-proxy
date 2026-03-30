package tokenreviewcache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
)

const defaultMaxSize = 1000

// entry holds a cached TokenReview result with an expiration time.
type entry struct {
	key       string
	user      *authenticationv1.UserInfo
	expiresAt time.Time
}

// Cache provides a thread-safe LRU+TTL cache for authenticated TokenReview results.
// Entries are evicted when they expire (TTL) or when the cache exceeds
// its maximum size (LRU — least recently used entries are evicted first).
// Token hashes (SHA-256) are used as keys to avoid storing raw tokens.
//
// Only authenticated results are cached. Unauthenticated results are never
// stored to prevent cache pollution attacks where an attacker floods the cache
// with invalid tokens to evict legitimate entries.
type Cache struct {
	mu      sync.Mutex
	items   map[string]*list.Element
	order   *list.List // front = most recently used
	ttl     time.Duration
	maxSize int
}

// New creates a new TokenReview cache with the given context, TTL, and default max size (1000).
func New(ctx context.Context, ttl time.Duration) *Cache {
	return NewWithMaxSize(ctx, ttl, defaultMaxSize)
}

// NewWithMaxSize creates a new TokenReview cache with the given context, TTL, and max size.
// The eviction goroutine is stopped when the context is canceled.
func NewWithMaxSize(ctx context.Context, ttl time.Duration, maxSize int) *Cache {
	c := &Cache{
		items:   make(map[string]*list.Element),
		order:   list.New(),
		ttl:     ttl,
		maxSize: maxSize,
	}
	go c.startEviction(ctx)
	return c
}

// Get looks up a cached TokenReview result by token.
// On cache hit, the entry is promoted to the front (most recently used).
// Returns (userInfo, found). Only authenticated tokens are cached, so
// a cache hit always means the token was authenticated.
func (c *Cache) Get(token string) (*authenticationv1.UserInfo, bool) {
	key := hashToken(token)

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}

	e := elem.Value.(*entry)
	if time.Now().After(e.expiresAt) {
		c.removeLocked(elem)
		return nil, false
	}

	// promote to front (most recently used)
	c.order.MoveToFront(elem)

	return e.user.DeepCopy(), true
}

// Set stores an authenticated TokenReview result in the cache.
// If the cache is full, the least recently used entry is evicted.
func (c *Cache) Set(token string, user *authenticationv1.UserInfo) {
	key := hashToken(token)

	c.mu.Lock()
	defer c.mu.Unlock()

	// update existing entry
	if elem, ok := c.items[key]; ok {
		e := elem.Value.(*entry)
		e.user = user.DeepCopy()
		e.expiresAt = time.Now().Add(c.ttl)
		c.order.MoveToFront(elem)
		return
	}

	// evict LRU if at capacity
	if c.order.Len() >= c.maxSize {
		c.removeLocked(c.order.Back())
	}

	e := &entry{
		key:       key,
		user:      user.DeepCopy(),
		expiresAt: time.Now().Add(c.ttl),
	}
	elem := c.order.PushFront(e)
	c.items[key] = elem
}

// removeLocked removes an element from both the list and map.
// Caller must hold c.mu.
func (c *Cache) removeLocked(elem *list.Element) {
	e := c.order.Remove(elem).(*entry)
	delete(c.items, e.key)
}

// startEviction periodically removes expired entries to prevent memory leaks.
// It stops when the context is canceled.
func (c *Cache) startEviction(ctx context.Context) {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.evict()
		}
	}
}

func (c *Cache) evict() {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Iterate all entries and remove expired ones.
	// LRU order != TTL order (access-time vs insert-time), so we cannot short-circuit.
	for elem := c.order.Back(); elem != nil; {
		e := elem.Value.(*entry)
		prev := elem.Prev()
		if now.After(e.expiresAt) {
			c.removeLocked(elem)
		}
		elem = prev
	}
}

// Len returns the number of entries in the cache.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// hashToken returns a hex-encoded SHA-256 hash of the token.
// We never store raw tokens in the cache.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
