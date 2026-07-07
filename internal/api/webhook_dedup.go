package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/webhookverify"
)

// defaultWebhookDedupTTL bounds how long a delivery id is remembered. Providers
// retry redeliveries with backoff over minutes (GitHub/Plane) so the window must
// comfortably cover a retry storm; 30 minutes mirrors the idempotencyCache TTL.
const defaultWebhookDedupTTL = 30 * time.Minute

// webhookDedupCacheMaxEntries caps live entries so a flood of unique delivery ids
// cannot grow the map unbounded between TTL sweeps. Over cap, seen evicts expired
// entries first and then the soonest-expiring, mirroring idempotencyCache.
const webhookDedupCacheMaxEntries = 8192

// webhookDedupCache is the E8 delivery-idempotency store: a bounded, TTL'd set of
// (webhook, delivery-id) keys. It deliberately mirrors idempotencyCache's
// bounded-TTL eviction rather than reusing it, because a webhook duplicate needs
// a single atomic "have I already accepted this delivery?" check-and-record —
// not the reserve/complete HTTP-response-replay protocol idempotencyCache serves.
// It replaces the per-pack dedup caches (discord receipts, github reserve_request,
// slack publishDedupCache) with one shared receiver-side store.
type webhookDedupCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // key -> expiry
	ttl     time.Duration
	max     int
	// now is an injectable clock for tests; nil uses time.Now.
	now func() time.Time
}

func newWebhookDedupCache(ttl time.Duration) *webhookDedupCache {
	if ttl <= 0 {
		ttl = defaultWebhookDedupTTL
	}
	return &webhookDedupCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
		max:     webhookDedupCacheMaxEntries,
	}
}

func (c *webhookDedupCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// seen atomically reports whether key was already recorded within the TTL. On a
// first sighting it records key (claiming the delivery) and returns false; on a
// live duplicate it returns true without extending the entry. An expired entry is
// treated as unseen and re-recorded.
func (c *webhookDedupCache) seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if exp, ok := c.entries[key]; ok {
		if now.Before(exp) {
			return true
		}
		delete(c.entries, key) // expired; fall through and re-record
	}
	c.entries[key] = now.Add(c.ttl)
	c.enforceCapLocked(now)
	return false
}

// forget drops a previously claimed key so a genuine processing failure (dispatch
// error, sink refusal) can be retried by the sender — the delivery was never
// actually acted on. Mirrors idempotencyCache.unreserve.
func (c *webhookDedupCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// clear drops every entry. Test seam so a suite can reset dedup state between cases.
func (c *webhookDedupCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]time.Time)
}

// enforceCapLocked keeps the map under c.max: expired entries first, then the
// soonest-expiring, until at or below the cap. Must hold c.mu.
func (c *webhookDedupCache) enforceCapLocked(now time.Time) {
	if len(c.entries) <= c.max {
		return
	}
	for k, exp := range c.entries {
		if now.After(exp) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) > c.max {
		var oldestKey string
		var oldest time.Time
		for k, exp := range c.entries {
			if oldestKey == "" || exp.Before(oldest) {
				oldestKey = k
				oldest = exp
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// webhookDedupKey namespaces a delivery id under its webhook so two webhooks that
// share a delivery-id value (e.g. both counting from 1) never collide.
func webhookDedupKey(hook, dedupID string) string {
	return hook + "\x00" + dedupID
}

// webhookDedupKeyFor derives the (webhook, delivery) dedup key from content the
// delivery's signature COVERS. A per-delivery-unique, signature-covered id (the
// jwt-jwks jti, flagged DedupIDSigned) is used directly; every other scheme keys
// on the body hash — the body is signed under every scheme, so it is tamper-proof
// and unique per delivery.
//
// The provider's surfaced DedupID (github's X-GitHub-Delivery, slack's timestamp)
// is deliberately NOT part of the key when it is unsigned or coarse:
//   - github/generic-hmac: the delivery header is UNSIGNED, so an attacker could
//     replay a captured valid (body, signature) under a fresh delivery id to mint
//     a fresh key and re-fire the order. Keying on the body hash defeats that —
//     a legit retry re-sends the byte-identical body, so it still dedups.
//   - slack: the timestamp is signed but only second-granular, so two DISTINCT
//     deliveries in the same wall-clock second would collide and one would be
//     silently dropped. The body hash keeps distinct deliveries distinct.
func webhookDedupKeyFor(hook string, vres webhookverify.VerifyResult, body []byte) string {
	id := webhookBodyHash(body)
	if vres.DedupIDSigned {
		if signed := strings.TrimSpace(vres.DedupID); signed != "" {
			id = signed
		}
	}
	return webhookDedupKey(hook, id)
}

// webhookBodyHash is the dedup-id fallback for schemes that surface no delivery
// id: a one-way SHA-256 of the raw body. It is a digest, never the body, so it is
// safe to use as a key and to place on an event.
func webhookBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
