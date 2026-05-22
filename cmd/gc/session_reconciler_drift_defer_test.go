package main

import (
	"testing"
	"time"
)

// TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinHalfTTL verifies
// that a second deferral with the same drift key, taken well within the
// false-negative TTL window, does NOT re-stamp the deferred_at timestamp.
//
// On parent (pre-fix), recordSessionAttachedConfigDriftDeferral always writes
// now() into deferred_at, producing a bead.updated event every reconcile tick
// (~1.4s) on every attached session bead with persistent drift. This test
// fails on parent and passes after the fix.
func TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinHalfTTL(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(session, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after first: %v", err)
	}
	firstStamp := first.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]
	if firstStamp == "" {
		t.Fatal("first call must stamp deferred_at")
	}
	if first.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] != driftKey {
		t.Fatalf("first key = %q, want %q", first.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata], driftKey)
	}

	// Advance the clock well within TTL/2 (TTL is 30s; advance 5s).
	env.clk.Time = env.clk.Time.Add(5 * time.Second)

	if err := recordSessionAttachedConfigDriftDeferral(first, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("second record: %v", err)
	}
	second, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after second: %v", err)
	}
	secondStamp := second.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]
	if secondStamp != firstStamp {
		t.Fatalf("deferred_at must not be re-stamped within TTL/2; got %q want unchanged %q",
			secondStamp, firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_RewritesWhenKeyChanges verifies
// that a different drift key forces a rewrite even within the TTL window — the
// guard must only suppress writes for the same drift situation, not for
// genuinely new drift.
func TestRecordSessionAttachedConfigDriftDeferral_RewritesWhenKeyChanges(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")

	if err := recordSessionAttachedConfigDriftDeferral(session, env.store, env.clk, "key-A"); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after first: %v", err)
	}
	firstStamp := first.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]

	env.clk.Time = env.clk.Time.Add(5 * time.Second)

	if err := recordSessionAttachedConfigDriftDeferral(first, env.store, env.clk, "key-B"); err != nil {
		t.Fatalf("second record: %v", err)
	}
	second, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if second.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata] != "key-B" {
		t.Fatalf("key after key-change call = %q, want key-B",
			second.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata])
	}
	if second.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == firstStamp {
		t.Fatalf("deferred_at must be re-stamped on key change; got unchanged %q", firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_RewritesAfterHalfTTL verifies
// that once the existing stamp is older than TTL/2, the next call refreshes
// it. This keeps the 30s false-negative TTL semantically intact: the
// deferral cannot be allowed to age past TTL just because the same key
// keeps being observed.
func TestRecordSessionAttachedConfigDriftDeferral_RewritesAfterHalfTTL(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(session, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after first: %v", err)
	}
	firstStamp := first.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]

	// Advance past TTL/2 (TTL is 30s; advance 16s).
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftFalseNegativeLimit/2 + time.Second)

	if err := recordSessionAttachedConfigDriftDeferral(first, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("second record: %v", err)
	}
	second, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if second.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == firstStamp {
		t.Fatalf("deferred_at must be refreshed past TTL/2; got unchanged %q", firstStamp)
	}
}
