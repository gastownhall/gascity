package main

import (
	"testing"
	"time"
)

// TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinRefreshInterval
// verifies that a second deferral with the same drift key, taken well within
// the refresh interval, does NOT re-stamp the deferred_at timestamp.
//
// On parent (pre-fix), recordSessionAttachedConfigDriftDeferral rewrote
// deferred_at once the existing stamp was older than the 30s validity window /
// 2 = 15s. Because the patrol interval (30s default) exceeds that, it re-stamped
// on essentially every reconcile tick, producing a bead.updated event — and a
// Dolt commit — on every attached session bead with persistent drift. This test
// fails on parent and passes after the fix.
func TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinRefreshInterval(t *testing.T) {
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

	// Advance the clock well within the refresh interval (2m; advance 5s).
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
		t.Fatalf("deferred_at must not be re-stamped within the refresh interval; got %q want unchanged %q",
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

// TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteAcrossManyTicks
// verifies the churn fix: with the default 30s patrol interval, the deferral
// is re-observed every tick, but record() must NOT rewrite the stamp on each
// one. The refresh interval is decoupled from (and much larger than) the
// patrol interval, so a long run of same-key ticks produces no metadata write
// (hence no Dolt commit) until the refresh interval elapses.
//
// This is the regression guard for the per-tick re-stamp churn: on the buggy
// version (rewrite once the stamp is older than the 30s validity limit / 2 =
// 15s), every 30s tick rewrote the stamp. It fails on that version and passes
// after the fix.
func TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteAcrossManyTicks(t *testing.T) {
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

	// Simulate many reconciler ticks at the default 30s patrol interval, all
	// well within the refresh interval. None of them may rewrite the stamp.
	const patrol = 30 * time.Second
	cur := first
	for elapsed := patrol; elapsed < sessionAttachedConfigDriftRefreshInterval; elapsed += patrol {
		env.clk.Time = env.clk.Time.Add(patrol)
		if err := recordSessionAttachedConfigDriftDeferral(cur, env.store, env.clk, driftKey); err != nil {
			t.Fatalf("record at +%s: %v", elapsed, err)
		}
		cur, err = env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("get at +%s: %v", elapsed, err)
		}
		if got := cur.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]; got != firstStamp {
			t.Fatalf("deferred_at re-stamped at +%s (got %q, want unchanged %q) — per-tick churn not eliminated",
				elapsed, got, firstStamp)
		}
	}

	// Once the existing stamp is older than the refresh interval, the next call
	// refreshes it so the deferral cannot age out of the false-negative window.
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval + time.Second)
	if err := recordSessionAttachedConfigDriftDeferral(cur, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("refresh record: %v", err)
	}
	refreshed, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after refresh: %v", err)
	}
	if refreshed.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == firstStamp {
		t.Fatalf("deferred_at must be refreshed past the refresh interval; got unchanged %q", firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_RefreshKeepsWithinValidityWindow
// exercises the actual read path (recentlyDeferredSessionAttachedConfigDrift)
// to prove the decoupling preserves the safety contract: a stamp that record()
// left un-refreshed because it was younger than the refresh interval must still
// be treated as valid by the reader. This catches regressions the constant-only
// invariant check would miss (wrong metadata key, reader still on the old 30s
// limit, off-by-one at the boundary).
func TestRecordSessionAttachedConfigDriftDeferral_RefreshKeepsWithinValidityWindow(t *testing.T) {
	// Structural invariant: the reader's validity window must exceed the
	// writer's refresh interval, or a just-skipped refresh could read as lapsed.
	if sessionAttachedConfigDriftRefreshInterval >= sessionAttachedConfigDriftFalseNegativeLimit {
		t.Fatalf("refresh interval (%s) must be < false-negative limit (%s) or the deferral can lapse between refreshes",
			sessionAttachedConfigDriftRefreshInterval, sessionAttachedConfigDriftFalseNegativeLimit)
	}

	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(session, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("record: %v", err)
	}
	stamped, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Just below the refresh interval: record() skips the rewrite. The reader
	// must still consider the deferral valid (this is the whole point of the
	// decoupling — a skipped refresh cannot create a false-negative gap).
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval - time.Second)
	if err := recordSessionAttachedConfigDriftDeferral(stamped, env.store, env.clk, driftKey); err != nil {
		t.Fatalf("record near refresh boundary: %v", err)
	}
	afterSkip, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	if !recentlyDeferredSessionAttachedConfigDrift(afterSkip, env.clk, driftKey) {
		t.Fatal("deferral must read as valid just below the refresh interval (un-refreshed stamp)")
	}

	// Just below the validity limit (and a refresh has NOT happened since the
	// original stamp): the reader must still treat it as valid — proving the
	// window the reader uses really is the 5m limit, not the old 30s value.
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftFalseNegativeLimit - sessionAttachedConfigDriftRefreshInterval - time.Second)
	stillValid, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get near validity boundary: %v", err)
	}
	if !recentlyDeferredSessionAttachedConfigDrift(stillValid, env.clk, driftKey) {
		t.Fatal("deferral must read as valid just below the false-negative limit")
	}

	// Past the validity limit with no refresh: the reader must now treat it as
	// lapsed (so genuine post-detach drift can proceed).
	env.clk.Time = env.clk.Time.Add(2 * time.Second)
	lapsed, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get past validity: %v", err)
	}
	if recentlyDeferredSessionAttachedConfigDrift(lapsed, env.clk, driftKey) {
		t.Fatal("deferral must read as lapsed past the false-negative limit")
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_SurvivesSkippedRefreshThenFlicker
// guards the COMPOSED safety invariant that actually keeps an attached session
// from being drained mid-conversation:
//
//	sessionAttachedConfigDriftRefreshInterval + patrol < sessionAttachedConfigDriftFalseNegativeLimit
//
// The worst case the validity window must absorb is: record() SKIPS a rewrite
// because the stamp is just under the refresh interval, and then exactly one
// patrol tick later attachment detection flickers to "not attached". The reader
// at that moment sees stamp age = refreshInterval + patrol and must STILL read
// the deferral as valid, or a still-attached session is drained/restarted in the
// middle of a live conversation.
//
// patrol_interval is operator-configurable (cfg.Daemon.PatrolIntervalDuration,
// default 30s) with no upper clamp, so this is checked across a range of patrol
// values. The structural assertion catches a future constant change that shrinks
// the headroom; the behavioral assertion proves the reader actually honors it.
func TestRecordSessionAttachedConfigDriftDeferral_SurvivesSkippedRefreshThenFlicker(t *testing.T) {
	const driftKey = "old-hash:new-hash"
	// Worst-case stamp age the reader must tolerate is (refreshInterval + patrol):
	// a rewrite skipped at age just below refreshInterval, then one patrol tick.
	patrols := []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 2 * time.Minute}
	for _, patrol := range patrols {
		worst := sessionAttachedConfigDriftRefreshInterval + patrol
		// Structural guard: the composed invariant, not just refresh < validity.
		if worst >= sessionAttachedConfigDriftFalseNegativeLimit {
			t.Fatalf("patrol=%s: refreshInterval(%s)+patrol(%s)=%s must be < falseNegativeLimit(%s); "+
				"a skipped refresh followed by an attachment flicker would drain a still-attached session",
				patrol, sessionAttachedConfigDriftRefreshInterval, patrol, worst,
				sessionAttachedConfigDriftFalseNegativeLimit)
		}

		// Behavioral guard: drive the real reader at exactly the worst-case age.
		env := newReconcilerTestEnv()
		session := env.createSessionBead("worker", "worker")
		if err := recordSessionAttachedConfigDriftDeferral(session, env.store, env.clk, driftKey); err != nil {
			t.Fatalf("patrol=%s: record: %v", patrol, err)
		}
		stamped, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("patrol=%s: get: %v", patrol, err)
		}
		stamp0 := stamped.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]

		// Tick at age just under the refresh interval: record() must SKIP (stamp unchanged).
		env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval - time.Second)
		if err := recordSessionAttachedConfigDriftDeferral(stamped, env.store, env.clk, driftKey); err != nil {
			t.Fatalf("patrol=%s: record near refresh boundary: %v", patrol, err)
		}
		afterSkip, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("patrol=%s: get after skip: %v", patrol, err)
		}
		if afterSkip.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] != stamp0 {
			t.Fatalf("patrol=%s: stamp must be unchanged just under the refresh interval", patrol)
		}

		// One patrol tick later attachment flickers detached: total age is now
		// (refreshInterval - 1s) + patrol < worst. The reader must still hold.
		env.clk.Time = env.clk.Time.Add(patrol)
		if !recentlyDeferredSessionAttachedConfigDrift(afterSkip, env.clk, driftKey) {
			t.Fatalf("patrol=%s: deferral must read as valid at age refreshInterval+patrol "+
				"(skipped refresh then flicker) or a still-attached session is drained", patrol)
		}
	}
}
