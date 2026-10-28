package main

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

// The start-deferral memo owns the branches that executePlannedStartsTraced
// and reconcileSessionBeadsTracedWithNamedDemand used to carry inline, so a nil
// memo or pass costs those two functions no cyclomatic complexity (#7170). The
// end-to-end behavior is pinned by session_start_deferral_test.go; these tests
// pin the seams themselves.

func TestStartDeferralMemoBeginPassTakesTheObservation(t *testing.T) {
	t.Run("a nil memo returns a nil pass", func(t *testing.T) {
		var memo *startDeferralMemo
		if pass := memo.beginPass(neverStartedSet("x")); pass != nil {
			t.Fatalf("nil memo opened a pass: %+v", pass)
		}
	})

	t.Run("a pass publishes the snapshot it was begun with", func(t *testing.T) {
		memo := newStartDeferralMemo()
		memo.beginPass(neverStartedSet("a", "b")).finish(context.Background())
		if got, want := seenDeferrals(memo), []string{"a", "b"}; !slices.Equal(got, want) {
			t.Fatalf("seen = %v, want %v", got, want)
		}
	})

	t.Run("a pass with no observation publishes nothing", func(t *testing.T) {
		memo := newStartDeferralMemo()
		commitSeenDeferralPass(memo, []string{"old", "other"}, "old")
		pass := memo.beginPass(nil)
		pass.noteGateDeferred("new")
		pass.finish(context.Background())
		if got, want := committedDeferrals(memo), []string{"old"}; !slices.Equal(got, want) {
			t.Fatalf("committed = %v, want %v untouched: a pass with no observation committed", got, want)
		}
		if got, want := seenDeferrals(memo), []string{"old", "other"}; !slices.Equal(got, want) {
			t.Fatalf("seen = %v, want %v untouched: a pass with no observation published", got, want)
		}
	})

	t.Run("an empty observation is an observation and publishes", func(t *testing.T) {
		memo := newStartDeferralMemo()
		commitSeenDeferralPass(memo, []string{"old"}, "old")
		memo.beginPass(neverStartedSet()).finish(context.Background())
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want the empty observation to replace it", got)
		}
		if got := seenDeferrals(memo); len(got) != 0 {
			t.Fatalf("seen = %v, want the empty observation to replace it", got)
		}
	})
}

func TestStartDeferralPassFinishReadsTheContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var noCtx context.Context

	for _, tc := range []struct {
		name      string
		ctx       context.Context
		wantTrack bool
	}{
		{"a live context commits", context.Background(), true},
		{"a nil context commits", noCtx, true},
		{"a canceled context commits nothing", canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memo := newStartDeferralMemo()
			commitDeferralPass(memo, "old")

			pass := memo.beginPass(neverStartedSet("new"))
			pass.noteGateDeferred("new")
			pass.finish(tc.ctx)

			got, seen := committedDeferrals(memo), seenDeferrals(memo)
			if tc.wantTrack {
				if len(got) != 1 || got[0] != "new" || len(seen) != 1 || seen[0] != "new" {
					t.Fatalf("committed = %v seen = %v, want [new] and [new]", got, seen)
				}
				return
			}
			if len(got) != 1 || got[0] != "old" || len(seen) != 1 || seen[0] != "old" {
				t.Fatalf("a canceled run changed the record: committed = %v seen = %v, want [old] untouched", got, seen)
			}
		})
	}

	t.Run("a nil pass finishes without panic", func(_ *testing.T) {
		var pass *startDeferralPass
		pass.finish(context.Background())
		pass.finish(canceled)
	})

	t.Run("a context canceled after the defer is read at finish", func(t *testing.T) {
		memo := newStartDeferralMemo()
		commitDeferralPass(memo, "old")
		ctx, cancelLater := context.WithCancel(context.Background())
		pass := memo.beginPass(neverStartedSet("new"))
		pass.noteGateDeferred("new")
		cancelLater()
		pass.finish(ctx)
		if got := committedDeferrals(memo); len(got) != 1 || got[0] != "old" {
			t.Fatalf("committed = %v, want [old]: the run was canceled before it finished", got)
		}
	})
}

func TestStartDeferralPassNoteStartOutcome(t *testing.T) {
	t.Run("a nil pass records nothing and does not panic", func(_ *testing.T) {
		var pass *startDeferralPass
		pass.noteStartOutcome("deferred_by_async_start_limit", "x")
	})

	t.Run("only the async start limit outcome records", func(t *testing.T) {
		for outcome, wantRecorded := range map[string]bool{
			"deferred_by_async_start_limit": true,
			"deferred_by_wake_budget":       false,
			"deferred_by_prepare_budget":    false,
			"deferred_by_endpoint_capacity": false,
			"context_canceled":              false,
			"started":                       false,
			"":                              false,
		} {
			memo := newStartDeferralMemo()
			pass := memo.beginPass(neverStartedSet())
			pass.noteStartOutcome(outcome, "id-1")
			pass.finish(context.Background())

			got := committedDeferrals(memo)
			if wantRecorded && (len(got) != 1 || got[0] != "id-1") {
				t.Errorf("outcome %q: committed = %v, want [id-1]", outcome, got)
			}
			if !wantRecorded && len(got) != 0 {
				t.Errorf("outcome %q recorded %v, want none", outcome, got)
			}
		}
	})

	t.Run("an empty session id is never recorded", func(t *testing.T) {
		memo := newStartDeferralMemo()
		pass := memo.beginPass(neverStartedSet())
		pass.noteStartOutcome("deferred_by_async_start_limit", "")
		pass.finish(context.Background())
		if got := committedDeferrals(memo); len(got) != 0 {
			t.Fatalf("committed = %v, want none", got)
		}
	})
}

func TestStartDeferralMemoHoldPendingCreate(t *testing.T) {
	passStart := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	claimed := passStart.Add(-5 * time.Minute)
	now := passStart.Add(12 * time.Minute)
	info := deferralInfo(claimed, claimed)
	info.ID = "held-1"
	clk := &clock.Fake{Time: now}

	heldMemo := func() *startDeferralMemo {
		memo := newStartDeferralMemo()
		commitSeenDeferralPass(memo, []string{info.ID, "not-deferred"}, info.ID)
		return memo
	}

	t.Run("a nil memo changes nothing and writes nothing", func(t *testing.T) {
		var memo *startDeferralMemo
		var stderr bytes.Buffer
		trace := newPoolDesiredStateTestTrace("worker")
		// A nil clock would panic on Now(): the nil memo must return first.
		expired, held := memo.holdPendingCreate(info, true, false, nil, &stderr, trace, "worker", "worker-1")
		if !expired || held {
			t.Fatalf("(expired, held) = (%v, %v), want the inputs back (true, false)", expired, held)
		}
		if stderr.Len() != 0 || len(trace.records) != 0 {
			t.Fatalf("a nil memo wrote stderr %q and %d trace records", stderr.String(), len(trace.records))
		}
	})

	t.Run("an already-unexpired row is left alone", func(t *testing.T) {
		var stderr bytes.Buffer
		trace := newPoolDesiredStateTestTrace("worker")
		expired, held := heldMemo().holdPendingCreate(info, false, true, clk, &stderr, trace, "worker", "worker-1")
		if expired || !held {
			t.Fatalf("(expired, held) = (%v, %v), want the inputs back (false, true)", expired, held)
		}
		if stderr.Len() != 0 || len(trace.records) != 0 {
			t.Fatalf("an unexpired row wrote stderr %q and %d trace records", stderr.String(), len(trace.records))
		}
	})

	t.Run("a row the memo does not hold changes nothing", func(t *testing.T) {
		var stderr bytes.Buffer
		trace := newPoolDesiredStateTestTrace("worker")
		other := info
		other.ID = "not-deferred"
		other.CreatedAt = passStart.Add(-30 * time.Minute) // in the last pass's snapshot: offered a start already
		other.PendingCreateStartedAt = ""
		expired, held := heldMemo().holdPendingCreate(other, true, false, clk, &stderr, trace, "worker", "worker-2")
		if !expired || held {
			t.Fatalf("(expired, held) = (%v, %v), want the inputs back (true, false)", expired, held)
		}
		if stderr.Len() != 0 || len(trace.records) != 0 {
			t.Fatalf("an unheld row wrote stderr %q and %d trace records", stderr.String(), len(trace.records))
		}
	})

	t.Run("a held row keeps its create and logs the hold exactly", func(t *testing.T) {
		var stderr bytes.Buffer
		trace := newPoolDesiredStateTestTrace("worker")
		expired, held := heldMemo().holdPendingCreate(info, true, false, clk, &stderr, trace, "worker", "worker-1")
		if expired || !held {
			t.Fatalf("(expired, held) = (%v, %v), want (false, true)", expired, held)
		}
		const wantLine = "session reconciler: keeping pending create worker-1 past its lease: its start is waiting on a start gate\n"
		if got := stderr.String(); got != wantLine {
			t.Fatalf("stderr = %q, want %q", got, wantLine)
		}
		if len(trace.records) != 1 {
			t.Fatalf("%d trace records, want 1: %+v", len(trace.records), trace.records)
		}
		r := trace.records[0]
		if r.SiteCode != TraceSiteReconcilerPendingCreate || r.ReasonCode != TraceReasonStartGateDeferred || r.OutcomeCode != TraceOutcomeDeferred {
			t.Errorf("record = %s/%s/%s, want %s/%s/%s", r.SiteCode, r.ReasonCode, r.OutcomeCode,
				TraceSiteReconcilerPendingCreate, TraceReasonStartGateDeferred, TraceOutcomeDeferred)
		}
		if r.Template != "worker" || r.SessionName != "worker-1" {
			t.Errorf("record template/session = %q/%q, want worker/worker-1", r.Template, r.SessionName)
		}
		if got := r.Fields["hold"]; got != "start_gate" {
			t.Errorf("fields[hold] = %v, want start_gate", got)
		}
		// 12 minutes after the pass began, 17 after the claim the lease runs from.
		if got, want := r.Fields["anchor_age"], "17m0s"; got != want {
			t.Errorf("fields[anchor_age] = %v, want %s", got, want)
		}
	})

	t.Run("a nil trace still logs the hold", func(t *testing.T) {
		var stderr bytes.Buffer
		expired, held := heldMemo().holdPendingCreate(info, true, false, clk, &stderr, nil, "worker", "worker-1")
		if expired || !held {
			t.Fatalf("(expired, held) = (%v, %v), want (false, true)", expired, held)
		}
		if stderr.Len() == 0 {
			t.Fatal("no stderr line for the held create")
		}
	})
}
