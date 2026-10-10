package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// closeBackstopFacts reads the harness sink and returns (compute, raw model)
// counts. The model count is the RAW appended count, not the idempotency-deduped
// one, so a re-sweep that double-records fails the assertion instead of being
// collapsed at read time.
func closeBackstopFacts(t *testing.T, h liveSweepHarness) (compute []usage.Fact, model int) {
	t.Helper()
	facts, warnings, err := usage.ReadFacts(h.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	for _, f := range facts {
		if f.Kind == usage.KindCompute {
			compute = append(compute, f)
		}
	}
	return compute, rawSinkModelFactCount(t, h.sinkPath)
}

// TestEmitDueComputeFactsSettlesSessionClosedFromActive is the #6672
// regression. A session closed straight from active (the drain-ack close at
// finalizeDrainAckStoppedSession, and every other close arm) leaves the open
// snapshot in the same tick it closes, so the open-set scan never saw it in a
// compute-terminal state: its compute fact was never emitted, and model usage
// that landed after the last live sweep (the <=liveModelSweepMinInterval tail)
// was never billed. The next tick must settle both from the closed bead, end the
// interval at closed_at rather than at the tick's now, and stay idempotent.
func TestEmitDueComputeFactsSettlesSessionClosedFromActive(t *testing.T) {
	for _, reason := range []string{"drained", "stale-session"} {
		t.Run(reason, func(t *testing.T) {
			workDir := t.TempDir()
			codexRoot := t.TempDir()
			start := liveSweepStart()
			writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
				{150, 100, 50},
			})
			h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))

			// Tick 1: the session is awake; the live lane bills the first invocation
			// and leaves the interval open.
			h.tick()
			if compute, model := closeBackstopFacts(t, h); len(compute) != 0 || model != 1 {
				t.Fatalf("tick 1: compute=%d model=%d, want 0/1", len(compute), model)
			}

			// A second invocation lands inside the live-sweep floor, and the session is
			// then closed straight from active, before any live sweep could see it.
			writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
				{150, 100, 50},
				{450, 200, 100},
			})
			closedAt := start.Add(30 * time.Minute)
			if !closeBead(h.store, h.beadID, reason, closedAt, io.Discard) {
				t.Fatalf("closeBead(%s) did not close the session", reason)
			}

			// Tick 2: the closed session is gone from the open snapshot.
			h.cr.emitDueComputeFacts(context.Background(), nil, false)
			compute, model := closeBackstopFacts(t, h)
			if len(compute) != 1 {
				t.Fatalf("tick 2 compute facts = %d, want 1 for the closed interval", len(compute))
			}
			if model != 2 {
				t.Fatalf("tick 2 model facts = %d, want 2: the post-live-sweep tail must bill at close", model)
			}
			if got, want := compute[0].WallSeconds, closedAt.Sub(start).Seconds(); got != want {
				t.Fatalf("compute WallSeconds = %v, want %v (interval ends at closed_at, not the tick)", got, want)
			}
			if compute[0].SessionID != h.beadID || compute[0].RunID != "run-L" {
				t.Fatalf("compute fact attribution = %+v, want session %s run run-L", compute[0], h.beadID)
			}
			b, err := h.store.Get(h.beadID)
			if err != nil {
				t.Fatal(err)
			}
			if b.Metadata[usageComputeEmittedAtKey] != h.meta["awake_started_at"] ||
				b.Metadata[usageModelSweptAtKey] != h.meta["awake_started_at"] {
				t.Fatalf("closed interval not committed: emitted=%q swept=%q", b.Metadata[usageComputeEmittedAtKey], b.Metadata[usageModelSweptAtKey])
			}
			if _, held := h.cr.liveSweepMemos.Load(h.beadID); held {
				t.Fatal("live-sweep memo for a closed session must be released")
			}

			// Tick 3 (and a forced backstop scan): nothing new.
			h.cr.emitDueComputeFacts(context.Background(), nil, false)
			h.cr.closedUsageScanAt = time.Time{}
			h.cr.emitDueComputeFacts(context.Background(), nil, false)
			if compute, model := closeBackstopFacts(t, h); len(compute) != 1 || model != 2 {
				t.Fatalf("idempotency: compute=%d model=%d after re-ticks, want 1/2", len(compute), model)
			}
		})
	}
}

// TestEmitDueComputeFactsClosedBackstopScan covers closes this controller never
// watched happen: a crash between the close and the next tick, or a close
// written by another process. The periodic scan recovers a session closed
// within closedUsageBackstopWindow, ignores one closed before it, and does not
// run on the boot pass.
func TestEmitDueComputeFactsClosedBackstopScan(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, ""))
	recent := h.beadID
	if !closeBead(h.store, recent, "orphaned", start.Add(10*time.Minute), io.Discard) {
		t.Fatal("close recent")
	}
	oldStart := time.Now().UTC().Add(-30 * time.Hour)
	oldInfo := h.addSession(t, liveCodexSessionMeta(oldStart, workDir, ""))
	if !closeBead(h.store, oldInfo.ID, "orphaned", oldStart.Add(time.Hour), io.Discard) {
		t.Fatal("close old")
	}

	h.cr.emitDueComputeFacts(context.Background(), nil, true)
	if compute, _ := closeBackstopFacts(t, h); len(compute) != 0 {
		t.Fatalf("boot pass emitted %d compute facts, want 0 (scan is steady-state only)", len(compute))
	}

	h.cr.emitDueComputeFacts(context.Background(), nil, false)
	compute, _ := closeBackstopFacts(t, h)
	if len(compute) != 1 || compute[0].SessionID != recent {
		t.Fatalf("backstop compute facts = %+v, want exactly one for %s", compute, recent)
	}
	if got, want := compute[0].WallSeconds, (10 * time.Minute).Seconds(); got != want {
		t.Fatalf("backstop WallSeconds = %v, want %v", got, want)
	}
	old, err := h.store.Get(oldInfo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("a session closed outside the backstop window must not be accounted")
	}
}

// TestEmitDueComputeFactsFailedCreateCloseEmitsNothing: a failed-create session
// never confirmed a start, so it has no awake interval to account.
func TestEmitDueComputeFactsFailedCreateCloseEmitsNothing(t *testing.T) {
	codexRoot := t.TempDir()
	meta := liveCodexSessionMeta(liveSweepStart(), t.TempDir(), "")
	delete(meta, "awake_started_at")
	meta["state"] = "creating"
	h := newLiveSweepHarness(t, codexRoot, meta)
	h.tick()
	if !closeBead(h.store, h.beadID, string(session.StateFailedCreate), time.Now().UTC(), io.Discard) {
		t.Fatal("close failed-create")
	}
	h.cr.emitDueComputeFacts(context.Background(), nil, false)
	if compute, model := closeBackstopFacts(t, h); len(compute) != 0 || model != 0 {
		t.Fatalf("failed-create close emitted compute=%d model=%d, want 0/0", len(compute), model)
	}
	b, err := h.store.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[usageComputeEmittedAtKey] != "" || b.Metadata[usageModelSweptAtKey] != "" {
		t.Fatalf("failed-create close stamped usage markers: %+v", b.Metadata)
	}
}
