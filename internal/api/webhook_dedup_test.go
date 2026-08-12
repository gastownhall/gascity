package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/webhookverify"
)

func TestWebhookDedupCache_SeenAndForget(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	k := webhookDedupKey("github", "d-1")
	if c.seen(k) {
		t.Fatal("first sighting must report unseen")
	}
	if !c.seen(k) {
		t.Fatal("second sighting must report a duplicate")
	}
	// forget releases the claim so a genuine retry can re-run.
	c.forget(k)
	if c.seen(k) {
		t.Fatal("after forget, the key must be unseen again")
	}
}

// A high-volume webhook that overflows the shared per-city cap must evict its
// OWN soonest-expiring entries, never a quieter co-resident hook's — otherwise a
// flood erodes another hook's replay window (schemes without a signed timestamp
// depend on that window).
func TestWebhookDedupCache_FloodEvictsOwnHookNotNeighbor(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	c.max = 4 // small cap so the flood overflows quickly

	quiet := webhookDedupKey("quiet", "only-one")
	if c.seen(quiet) {
		t.Fatal("first sight of the quiet hook must be unseen")
	}

	// Flood a noisy hook well past the cap.
	for i := 0; i < 50; i++ {
		c.seen(webhookDedupKey("noisy", fmt.Sprintf("d-%d", i)))
	}

	// The quiet hook's single entry must survive: seeing it again is a duplicate.
	if !c.seen(quiet) {
		t.Fatal("the quiet hook's replay entry was evicted by the noisy hook's flood")
	}
	// The cap is still honored.
	if len(c.entries) > c.max {
		t.Fatalf("cache holds %d entries, over cap %d", len(c.entries), c.max)
	}
}

// A hook that already holds most of the shared cap must keep ALL of its replay
// entries when an unrelated hook then floods the cache with unique deliveries.
// The overflow is charged to the hook doing the flooding, never to whichever
// hook happens to hold the most entries — otherwise the flooder silently erodes
// a quiet-but-busy neighbor's replay window. This is the ordering the earlier
// "flood evicts own hook" test misses: here the eventual victim becomes the
// busiest hook FIRST, then the neighbor floods.
func TestWebhookDedupCache_BusiestHookSurvivesNeighborFlood(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	c.max = 8

	// Hook A fills the cache to the cap, making it the busiest hook.
	aKeys := make([]string, c.max)
	for i := range aKeys {
		aKeys[i] = webhookDedupKey("hook-a", fmt.Sprintf("a-%d", i))
		if c.seen(aKeys[i]) {
			t.Fatalf("hook-a delivery %d: first sight must be unseen", i)
		}
	}

	// An unrelated hook now floods the shared cache with unique deliveries.
	for i := 0; i < 100; i++ {
		c.seen(webhookDedupKey("hook-b", fmt.Sprintf("b-%d", i)))
	}

	// Every one of hook A's original replay keys must still read as a duplicate:
	// the neighbor's flood must not have evicted any of A's entries.
	for i, k := range aKeys {
		if !c.seen(k) {
			t.Fatalf("hook-a replay key %d was evicted by hook-b's flood — a neighbor's traffic must not erode this hook's replay window", i)
		}
	}
	// The cap is soft by at most the flooder's single retained just-claimed key:
	// hook A holds the whole cap, so hook B's latest claim (which seen() must not
	// evict) sits one entry over. Never more than that here — the overshoot is
	// bounded by the live co-resident hook count, not unbounded.
	if len(c.entries) > c.max+1 {
		t.Fatalf("cache holds %d entries, over the soft cap %d", len(c.entries), c.max+1)
	}
}

// A hook's very first delivery must stay tracked even when a neighbor has already
// saturated the shared cap. The overflow policy charges eviction to the inserting
// hook, but it must never evict that hook's just-claimed key: seen() has already
// returned false and the handler will dispatch on that promise, so dropping the
// key would dispatch an untracked delivery and let its replay re-fire. Hook A
// fills the cap; hook B then sends one delivery (claimed, retained) whose
// duplicate must dedup. This is the cap-saturation ordering the neighbor-flood
// tests miss: there the victim already holds entries, here it holds none yet.
func TestWebhookDedupCache_FirstClaimRetainedUnderSaturatedCap(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	c.max = 8

	// Hook A saturates the shared cap.
	aKeys := make([]string, c.max)
	for i := range aKeys {
		aKeys[i] = webhookDedupKey("hook-a", fmt.Sprintf("a-%d", i))
		if c.seen(aKeys[i]) {
			t.Fatalf("hook-a delivery %d: first sight must be unseen", i)
		}
	}

	// Hook B's first delivery lands into an already-full cache.
	bKey := webhookDedupKey("hook-b", "b-only")
	if c.seen(bKey) {
		t.Fatal("hook-b's first delivery must be unseen")
	}
	// The just-claimed key must be retained: a duplicate of it dedups rather than
	// dispatching an untracked replay.
	if !c.seen(bKey) {
		t.Fatal("hook-b's first claim was evicted under cap saturation — its replay would dispatch untracked")
	}
	// Neighbor protection still holds: none of hook A's replay entries were dropped
	// to make room for hook B's claim.
	for i, k := range aKeys {
		if !c.seen(k) {
			t.Fatalf("hook-a replay key %d was evicted — a co-resident hook's first claim must not cost a neighbor its replay window", i)
		}
	}
	// The cap is soft by exactly the one retained fresh claim, never unbounded.
	if len(c.entries) > c.max+1 {
		t.Fatalf("cache holds %d entries, over the soft cap %d", len(c.entries), c.max+1)
	}
}

func TestWebhookDedupCache_Clear(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	k := webhookDedupKey("h", "1")
	if c.seen(k) {
		t.Fatal("first sight unseen")
	}
	c.clear()
	if c.seen(k) {
		t.Fatal("after clear, a previously-seen key must be unseen again")
	}
}

func TestWebhookDedupCache_TTLExpiry(t *testing.T) {
	c := newWebhookDedupCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	k := webhookDedupKey("h", "x")
	if c.seen(k) {
		t.Fatal("unseen on first sight")
	}
	if !c.seen(k) {
		t.Fatal("duplicate within the TTL")
	}
	now = now.Add(2 * time.Minute) // past the TTL
	if c.seen(k) {
		t.Fatal("an expired entry must be treated as unseen")
	}
}

func TestWebhookDedupCache_KeyNamespacing(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	if c.seen(webhookDedupKey("a", "1")) {
		t.Fatal("webhook a, id 1: unseen")
	}
	if c.seen(webhookDedupKey("b", "1")) {
		t.Fatal("webhook b sharing id 1 must not collide with webhook a")
	}
}

func TestWebhookDedupCache_EvictsOverCap(t *testing.T) {
	c := newWebhookDedupCache(time.Hour)
	c.max = 4
	for i := 0; i < 20; i++ {
		c.seen(webhookDedupKey("h", fmt.Sprintf("d-%d", i)))
	}
	if len(c.entries) > c.max {
		t.Fatalf("entries = %d, want <= cap %d", len(c.entries), c.max)
	}
}

// FIX 3/4: the dedup KEY comes from signature-covered content, never from the
// unsigned/coarse provider delivery id.
func TestWebhookDedupKeyFor_KeysOnSignedContent(t *testing.T) {
	bodyA := []byte(`{"n":1}`)
	bodyB := []byte(`{"n":2}`)

	// FIX 3 (github/generic-hmac): the delivery id is UNSIGNED. A replayed body
	// with a FRESH delivery id must map to the SAME key (so the replay dedups) —
	// keying is on the body hash, not the attacker-mutable header.
	ghA := webhookverify.VerifyResult{OK: true, DedupID: "delivery-A", DedupIDSigned: false}
	ghB := webhookverify.VerifyResult{OK: true, DedupID: "delivery-B", DedupIDSigned: false}
	if webhookDedupKeyFor("gh", ghA, bodyA) != webhookDedupKeyFor("gh", ghB, bodyA) {
		t.Error("github: same body with different (unsigned) delivery ids must share a key — else a fresh id replays the delivery")
	}
	// Distinct bodies must stay distinct even with the same delivery id.
	if webhookDedupKeyFor("gh", ghA, bodyA) == webhookDedupKeyFor("gh", ghA, bodyB) {
		t.Error("github: distinct bodies must have distinct keys")
	}

	// FIX 4 (slack): the id is signed but coarse (second-granular). Two DISTINCT
	// bodies sharing the same ts must NOT collide — key on the body hash.
	slA := webhookverify.VerifyResult{OK: true, DedupID: "1700000000", DedupIDSigned: false}
	slB := webhookverify.VerifyResult{OK: true, DedupID: "1700000000", DedupIDSigned: false}
	if webhookDedupKeyFor("slack", slA, bodyA) == webhookDedupKeyFor("slack", slB, bodyB) {
		t.Error("slack: distinct bodies in the same second must not collide on the dedup key")
	}

	// jwt-jwks: the jti is signed AND unique per delivery, so it IS the key.
	// Same jti dedups regardless of body; different jti does not.
	jtiX := webhookverify.VerifyResult{OK: true, DedupID: "jti-x", DedupIDSigned: true}
	jtiY := webhookverify.VerifyResult{OK: true, DedupID: "jti-y", DedupIDSigned: true}
	if webhookDedupKeyFor("jwt", jtiX, bodyA) != webhookDedupKeyFor("jwt", jtiX, bodyB) {
		t.Error("jwt: same signed jti must dedup even when the body differs")
	}
	if webhookDedupKeyFor("jwt", jtiX, bodyA) == webhookDedupKeyFor("jwt", jtiY, bodyA) {
		t.Error("jwt: different jti must produce different keys (both dispatch)")
	}
	// A jwt with no jti falls back to the body hash (not an empty-id collision).
	noJTI := webhookverify.VerifyResult{OK: true, DedupID: "", DedupIDSigned: true}
	if webhookDedupKeyFor("jwt", noJTI, bodyA) != webhookDedupKey("jwt", webhookBodyHash(bodyA)) {
		t.Error("jwt without a jti must fall back to the body hash")
	}
}

func TestWebhookBodyHash_IsDigestNotBody(t *testing.T) {
	h := webhookBodyHash([]byte("super-secret-body-content"))
	if strings.Contains(h, "super-secret-body-content") {
		t.Fatalf("body hash must not embed the body: %q", h)
	}
	if !strings.HasPrefix(h, "sha256:") {
		t.Fatalf("body hash = %q, want sha256: prefix", h)
	}
	// Same body → same key (dedup works); different body → different key.
	if webhookBodyHash([]byte("a")) == webhookBodyHash([]byte("b")) {
		t.Fatal("distinct bodies must hash to distinct keys")
	}
}
