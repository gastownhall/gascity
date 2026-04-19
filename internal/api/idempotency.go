package api

import (
	"net/http"
	"sync"
	"time"
)

// idempotencyCacheMaxEntries caps the live entry count. Once reached,
// reserve evicts expired entries first and then the entry with the
// soonest-expiring deadline. Without this cap a client posting unique
// Idempotency-Key values grows the cache unbounded between TTL
// cleanups (ttl/4). The ceiling mirrors responseCacheMaxEntries.
const idempotencyCacheMaxEntries = 1024

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
	pending    bool // true while the create is in-flight
	statusCode int
	value      any // typed response value, populated when complete() is called
	bodyHash   string
	expiresAt  time.Time
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
	c.enforceCapLocked()
	return cachedEntry{}, false
}

// enforceCapLocked evicts expired entries and, if still over cap,
// evicts the completed entry with the soonest expiry. Must be called
// with c.mu held.
//
// Pending entries are NEVER evicted. Evicting a pending reservation
// would let a concurrent retry with the same Idempotency-Key re-execute
// the create, defeating the whole purpose of the cache. If the cache
// fills with pending reservations (pathological client behavior), new
// reserves still proceed — the cap only constrains completed entries.
func (c *idempotencyCache) enforceCapLocked() {
	if len(c.entries) <= idempotencyCacheMaxEntries {
		return
	}
	now := time.Now()
	for k, v := range c.entries {
		if !v.pending && now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
	// If still over cap, evict the soonest-expiring NON-PENDING entry.
	// Loop bails out when there are no eligible victims left.
	for len(c.entries) > idempotencyCacheMaxEntries {
		var oldestKey string
		var oldestExpiry time.Time
		for k, v := range c.entries {
			if v.pending {
				continue
			}
			if oldestKey == "" || v.expiresAt.Before(oldestExpiry) {
				oldestKey = k
				oldestExpiry = v.expiresAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// complete fills in the response for a previously reserved key.
func (c *idempotencyCache) complete(key string, statusCode int, value any, bodyHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedEntry{
		pending:    false,
		statusCode: statusCode,
		value:      value,
		bodyHash:   bodyHash,
		expiresAt:  time.Now().Add(c.ttl),
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

// handleIdempotent replays or rejects a duplicate request for the scoped key.
// Returns true when the response has already been written.
func (c *idempotencyCache) handleIdempotent(w http.ResponseWriter, key, bodyHash string) bool {
	if key == "" {
		return false
	}
	existing, found := c.reserve(key, bodyHash)
	if !found {
		return false
	}
	if existing.bodyHash != bodyHash {
		writeError(w, http.StatusUnprocessableEntity, "idempotency_mismatch",
			"Idempotency-Key reused with different request body")
		return true
	}
	if existing.pending {
		writeError(w, http.StatusConflict, "in_flight",
			"request with this Idempotency-Key is already in progress")
		return true
	}
	statusCode := existing.statusCode
	if statusCode == 0 {
		statusCode = http.StatusCreated
	}
	writeJSON(w, statusCode, existing.value)
	return true
}

// storeResponse caches the typed response value for later replay.
// Callers may pass either `(key, hash, value)` or `(key, hash, status, value)`.
func (c *idempotencyCache) storeResponse(key, bodyHash string, statusOrValue any, maybeValue ...any) {
	if key == "" {
		return
	}
	statusCode := http.StatusCreated
	value := statusOrValue
	if len(maybeValue) > 0 {
		if code, ok := statusOrValue.(int); ok {
			statusCode = code
			value = maybeValue[0]
		}
	}
	c.complete(key, statusCode, value, bodyHash)
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
