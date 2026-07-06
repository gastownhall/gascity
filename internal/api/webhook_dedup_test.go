package api

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
