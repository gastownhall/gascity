package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// lastStartFailedAt is the advisory stamp a start failed in prepare or
// pre_start leaves on its row (ga-z4joq0).
const lastStartFailedAt = "last_start_failed_at"

// TestWakeFairnessTimeRanksAFailedStartNoEarlierThanItsFailure pins the
// wake-fairness key's failure term: a session whose start last failed ranks
// no earlier than the failure, so its cleared last_woke_at does not send it
// back to the front of a contended budget ahead of sessions that have waited
// longer since their last attempt.
func TestWakeFairnessTimeRanksAFailedStartNoEarlierThanItsFailure(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name string
		info session.Info
		want time.Time
	}{
		{"failure after the sleep", session.Info{SleptAt: at(-2 * time.Hour), LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"failure before the last wake", session.Info{LastWokeAt: at(-time.Minute), LastStartFailedAt: at(-2 * time.Hour)}, now.Add(-time.Minute)},
		{"failure after the creation", session.Info{CreatedAt: now.Add(-3 * time.Hour), LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"failure with no other time", session.Info{LastStartFailedAt: at(-time.Minute)}, now.Add(-time.Minute)},
		{"unparseable failure", session.Info{SleptAt: at(-2 * time.Hour), LastStartFailedAt: "garbage"}, now.Add(-2 * time.Hour)},
		{"no failure", session.Info{SleptAt: at(-2 * time.Hour)}, now.Add(-2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wakeFairnessTime(startCandidate{info: tc.info}); !got.Equal(tc.want) {
				t.Fatalf("wakeFairnessTime = %s, want %s", got, tc.want)
			}
		})
	}

	for _, standing := range []bool{false, true} {
		failed := startCandidate{standing: standing, info: session.Info{ID: "failed", SleptAt: at(-2 * time.Hour), LastStartFailedAt: at(-time.Minute)}}
		waiting := startCandidate{standing: standing, info: session.Info{ID: "waiting", LastWokeAt: at(-30 * time.Minute)}}
		ready := []startCandidate{failed, waiting}
		sortCandidatesByWakeFairness(ready)
		if ready[0].info.ID != "waiting" {
			t.Fatalf("standing=%v: order %s, %s; want the session waiting since before the failure first", standing, ready[0].info.ID, ready[1].info.ID)
		}
	}
}

// requireStartFailedStamp fails unless row cleared last_woke_at and stamped
// last_start_failed_at at now.
func requireStartFailedStamp(t *testing.T, row beads.Bead, now time.Time) {
	t.Helper()
	if got := row.Metadata["last_woke_at"]; got != "" {
		t.Errorf("last_woke_at = %q, want cleared", got)
	}
	if got, want := row.Metadata[lastStartFailedAt], now.UTC().Format(time.RFC3339); got != want {
		t.Errorf("%s = %q, want the failure time %q", lastStartFailedAt, got, want)
	}
}

// TestLegacyStartFailureStampsLastStartFailedAt is AC-6 of ga-z4joq0 in the
// legacy reconciler: each arm that ends a start failed in prepare or
// pre_start clears last_woke_at and stamps last_start_failed_at, and the
// failed session then ranks behind a same-class session that has waited
// since before the failure.
func TestLegacyStartFailureStampsLastStartFailedAt(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	t.Run("non-pending failure restores PreWake", func(t *testing.T) {
		e := newCapacityEnv(t, false, "s")
		c := asleepSession(t, e, map[string]string{"slept_at": e.clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)})
		e.sp.StartErrors["s"] = boom
		if woken := e.start(ctx, c); woken != 0 {
			t.Fatalf("woken = %d, want the failed start not counted", woken)
		}
		got := e.bead(t, c.info.ID)
		if got.Metadata["state"] != "asleep" {
			t.Fatalf("state = %q, want PreWake restored", got.Metadata["state"])
		}
		requireStartFailedStamp(t, got, e.clk.Now())

		waiting := e.session(t, "w", "", map[string]string{
			"session_name": "w", "template": "w", "state": "asleep",
			"last_woke_at": e.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		})
		ready := []startCandidate{e.refreshed(c), waiting}
		sortCandidatesByWakeFairness(ready)
		if ready[0].info.ID != waiting.info.ID {
			t.Fatalf("the failed session sorts ahead of one waiting since before its failure")
		}
	})

	t.Run("configured pending create keeps its row", func(t *testing.T) {
		e := newCapacityEnv(t, false, "p")
		c := e.pendingCreate(t, "p", "", nil)
		c.configured = true
		e.sp.StartErrors["p"] = boom
		e.start(ctx, c)
		got := e.bead(t, c.info.ID)
		if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("status %q claim %q, want the configured row kept with its claim", got.Status, got.Metadata["pending_create_claim"])
		}
		requireStartFailedStamp(t, got, e.clk.Now())
	})

	t.Run("prepare failure", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker")
		e.store = &failSetMetadataStore{MemStore: beads.NewMemStore(), failKey: "session_key"}
		c := e.pendingCreate(t, "worker", "", map[string]string{"state": "creating"})
		c.tp.ResolvedProvider = &config.ResolvedProvider{SessionIDFlag: "--session-id"}
		if woken := e.start(ctx, c); woken != 0 {
			t.Fatalf("woken = %d, want 0 when prepare fails after PreWake", woken)
		}
		requireStartFailedStamp(t, e.bead(t, c.info.ID), e.clk.Now())
		if slices.ContainsFunc(e.sp.Calls, func(c runtime.Call) bool { return c.Method == "Start" }) {
			t.Fatalf("provider calls %+v, want no Start after a failed prepare", e.sp.Calls)
		}
	})
}

// TestAbandonStampsLastStartFailedAt is AC-6 of ga-z4joq0 in v2: the
// abandon's prepare and configured-pending class and its any-other class
// (otherFailurePatch) write last_start_failed_at at the pass's time in the
// write that clears or restores last_woke_at.
func TestAbandonStampsLastStartFailedAt(t *testing.T) {
	k := startKit(t, intentStart, "creating")
	boom := errors.New("boom")
	woke := gatherNow.Format(time.RFC3339)
	creating := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4", "last_woke_at", woke, "wake_attempts", "2")
	undo := preWakeUndo{
		token: "tok-4", written: session.MetadataPatch{"state": "creating", "last_woke_at": woke},
		restore: session.MetadataPatch{"state": "asleep", "last_woke_at": ""},
	}
	other := startFailureRead{startFailure: startFailure{cause: causeStartError, err: boom, undo: undo, display: "worker"}}
	prepare := other
	prepare.cause = causePrepare
	named := other
	named.pending, named.configured = true, true
	relaunch := other
	relaunch.relaunched = true
	killed := poolRow("gc-1", "worker", 1, "asleep", "generation", "4", "instance_token", "tok-4", "sleep_reason", "killed")
	for _, c := range []struct {
		name       string
		row        beads.Bead
		f          startFailureRead
		clearsWoke bool
	}{
		{"prepare", creating, prepare, true},
		{"configured named pending, not died", creating, named, true},
		{"any other failure", creating, other, true},
		{"any other failure of a relaunch", creating, relaunch, false},
		{"any other failure of a row killed mid-start", killed, other, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			step := abandonStep(abandonView(k, c.row, txRuntime{Class: rtAbsent}, c.f))
			if got := step.Write[lastStartFailedAt]; got != woke {
				t.Fatalf("write %v, want %s = %q", step.Write, lastStartFailedAt, woke)
			}
			if v, ok := step.Write["last_woke_at"]; c.clearsWoke && (!ok || v != "") {
				t.Fatalf("write %v, want last_woke_at cleared in the same write", step.Write)
			}
		})
	}
}
