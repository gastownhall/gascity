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
