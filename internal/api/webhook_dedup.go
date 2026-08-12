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
// entries first and then the soonest-expiring OTHER entry of the hook whose
// insertion overflowed the cap, so a flood only ever shrinks its own replay
// window. The just-claimed key is never evicted, so the cap is soft by at most
// one retained claim per co-resident hook — bounded, because hook names are
// configured and entries expire — rather than ever dropping a delivery seen just
// promised to track.
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
// treated as unseen and re-recorded. A false return guarantees key stays retained
// even when the shared cap is already saturated by other hooks, so the caller can
// dispatch knowing a replay of the same delivery will dedup rather than re-fire.
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
	c.enforceCapLocked(now, key)
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

// enforceCapLocked keeps the map near c.max: it drops expired entries first,
// then — while still over cap — evicts the soonest-expiring entry belonging to
// insertedHook OTHER THAN insertedKey, the hook whose just-recorded delivery
// pushed the map over the cap. Charging the overflow to the inserting hook means
// a high-volume webhook can only shrink ITS OWN replay window under pressure; a
// flood on one hook can never evict a quieter co-resident hook's entry — not even
// one that currently holds the most entries. The shared per-city cap must not let
// one hook erode another's replay protection (the schemes without a signed
// timestamp rely on this window).
//
// insertedKey itself is never evicted here: seen has already returned false and
// the webhook handler dispatches on that promise, so dropping the fresh key would
// dispatch an untracked delivery and let its replay re-fire. When the inserting
// hook holds only that fresh key while other hooks fill the cap, the map is left
// one entry over cap rather than breaking either the retention or the
// neighbor-protection invariant — a bounded soft overshoot (at most one retained
// claim per live co-resident hook; hook names are configured and entries expire),
// never unbounded growth. Must hold c.mu.
func (c *webhookDedupCache) enforceCapLocked(now time.Time, insertedKey string) {
	if len(c.entries) <= c.max {
		return
	}
	for k, exp := range c.entries {
		if now.After(exp) {
			delete(c.entries, k)
		}
	}
	insertedHook := webhookDedupHookOf(insertedKey)
	for len(c.entries) > c.max {
		if !c.evictFromHookLocked(insertedHook, insertedKey) {
			return
		}
	}
}

// evictFromHookLocked deletes the soonest-expiring entry belonging to hook,
// skipping protectKey so the just-claimed delivery is never the victim. It
// returns false when hook has no other entry left to evict, so the caller stops
// rather than spinning (leaving the map a bounded amount over cap). Must hold
// c.mu. Eviction runs only on a cap overflow, so the O(n) scan is bounded and
// rare.
func (c *webhookDedupCache) evictFromHookLocked(hook, protectKey string) bool {
	var victimKey string
	var victimExp time.Time
	for k, exp := range c.entries {
		if k == protectKey || webhookDedupHookOf(k) != hook {
			continue
		}
		if victimKey == "" || exp.Before(victimExp) {
			victimKey, victimExp = k, exp
		}
	}
	if victimKey == "" {
		return false
	}
	delete(c.entries, victimKey)
	return true
}

// webhookDedupHookOf returns the hook-name prefix of a dedup key (the segment
// before the NUL separator written by webhookDedupKey).
func webhookDedupHookOf(key string) string {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[:i]
	}
	return key
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
