package main

import (
	"testing"
	"time"
)

// TestCompletionsLaneBacksOffAfterWarmFailure: a chunk that could not warm its
// idempotency record converged nothing, and re-polling it at the chunk cadence
// re-issues the journal read every poll interval for as long as the journal
// stays unreadable — the 30s full-history hammering of ga-ftgyl. A warm failure
// must back the sweep off without forgetting that it is owed.
func TestCompletionsLaneBacksOffAfterWarmFailure(t *testing.T) {
	lane := newCompletionsLane()
	now := time.Now()

	if _, due := lane.sweepDue(now); !due {
		t.Fatal("a fresh lane must be due for its startup sweep")
	}

	lane.noteSweepFailure(now)
	if reason, due := lane.sweepDue(now.Add(time.Second)); due {
		t.Fatalf("due (%s) immediately after a warm failure; the backoff is not holding", reason)
	}
	if _, due := lane.sweepDue(now.Add(completionsWarmFailureBackoff + time.Second)); !due {
		t.Fatal("not due after the backoff elapsed; the failure forgot the sweep is owed")
	}

	// A forced sweep survives the backoff: the debt is kept, only the retry is
	// paced. This lane has never completed a sweep, so the debt is owed as
	// STARTUP, not cursor-gap: startup precedes forced until the first traversal
	// finishes (so a pre-first-sweep gap cannot suppress VisitStamped), and a
	// startup sweep subsumes the gap by visiting stamped roots too.
	lane.force()
	lane.noteSweepFailure(now)
	if _, due := lane.sweepDue(now.Add(time.Second)); due {
		t.Fatal("forced sweep not paced by the failure backoff")
	}
	if reason, due := lane.sweepDue(now.Add(completionsWarmFailureBackoff + time.Second)); !due || reason != backstopReasonStartup {
		t.Fatalf("after the backoff, due=%t reason=%q; want the forced sweep still owed as %q", due, reason, backstopReasonStartup)
	}
}
