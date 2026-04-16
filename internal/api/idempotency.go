package api

import (
	"net/http"
	"sync"
	"time"
)

// idempotencyCache stores responses keyed by Idempotency-Key header values.
// Used on create endpoints so clients can safely retry after network failures.
//
// The cache uses a two-phase protocol to prevent TOCTOU races:
//  1. reserve(key, hash) atomically inserts a pending entry if absent
//  2. complete(key, ...) fills in the response body once the create succeeds
//
// Concurrent requests with the same key: the first reserves, others see the
// pending entry and get a 409 Conflict response.
//
// Phase 3 Fix 3l: entries store the typed response value (not serialized
// bytes). The request-body hash (bodyHash) stays as its own hex string
// because it IS a hash, not a response. Huma re-serializes the typed value
// on each replay at the handler boundary.
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]cachedEntry
	ttl     time.Duration
}

type cachedEntry struct {
	pending   bool // true while the create is in-flight
	value     any  // typed response value, populated when complete() is called
	bodyHash  string
	expiresAt time.Time
}

func newIdempotencyCache(ttl time.Duration) *idempotencyCache {
	return &idempotencyCache{
		entries: make(map[string]cachedEntry),
		ttl:     ttl,
	}
}

// reserve atomically reserves a key for processing. Returns:
//   - (entry, true) if the key already exists (completed or pending)
//   - (zero, false) if the key was successfully reserved for this caller
func (c *idempotencyCache) reserve(key, bodyHash string) (cachedEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok {
		if time.Now().After(entry.expiresAt) {
			delete(c.entries, key)
			// Fall through to reserve.
		} else {
			return entry, true
		}
	}
	// Reserve the key with a pending entry.
	c.entries[key] = cachedEntry{
		pending:   true,
		bodyHash:  bodyHash,
		expiresAt: time.Now().Add(c.ttl),
	}
	// Lazy cleanup when cache grows large.
	if len(c.entries) > 1000 {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
	return cachedEntry{}, false
}

// complete fills in the response for a previously reserved key.
func (c *idempotencyCache) complete(key string, value any, bodyHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedEntry{
		pending:   false,
		value:     value,
		bodyHash:  bodyHash,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// unreserve removes a pending reservation on failure (so the key can be retried).
func (c *idempotencyCache) unreserve(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok && entry.pending {
		delete(c.entries, key)
	}
}

// storeResponse caches the typed response value for later replay.
// No serialization happens here; Huma re-serializes on each replay at the
// handler boundary.
func (c *idempotencyCache) storeResponse(key, bodyHash string, v any) {
	if key == "" {
		return
	}
	c.complete(key, v, bodyHash)
}

// replayAs is a generic helper: look up an existing entry and type-assert
// its cached value to T. Returns (zero, false) if absent, pending, or
// type-asserted fails.
func replayAs[T any](entry cachedEntry) (T, bool) {
	var zero T
	if entry.pending || entry.value == nil {
		return zero, false
	}
	t, ok := entry.value.(T)
	return t, ok
}

// scopedIdemKey returns an idempotency cache key namespaced by HTTP method
// and path, preventing cross-endpoint collisions when clients reuse the same
// Idempotency-Key value across different endpoints.
func scopedIdemKey(r *http.Request, key string) string {
	if key == "" {
		return ""
	}
	return r.Method + ":" + r.URL.Path + ":" + key
}

