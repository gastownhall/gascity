package coordclass_test

// This file is in the EXTERNAL test package on purpose. guard_test.go pins the
// contract strings coordclass mirrors against their canonical definitions, but
// it cannot reach this one: internal/storebinding imports coordclass, so an
// in-package test importing storebinding would be an import cycle. The external
// test package can import both, and it pins the arm BEHAVIORALLY — through
// Classify over the beads the real queue writes — which is a stronger pin than
// string equality, because it fails if either the spelling or the arm moves.

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// queueRecords drives the real queue through its three durable lifecycle states
// and returns every bead it left behind.
//
// The fixtures are neither hand-written nor built from a test-only encoder:
// they are what Enqueue, RecordFailure and Ack actually write, so a rename of
// the queue label moves the fixture and these tests fail rather than passing
// over a bead that no longer resembles the real one.
func queueRecords(t *testing.T) []beads.Bead {
	t.Helper()
	// SQLite rather than MemStore: the queue refuses a store without the
	// claim compare-and-swap, and the point of this fixture is that it is the
	// real queue over a real engine.
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			if err := closer.CloseStore(); err != nil {
				t.Logf("closing the queue store: %v", err)
			}
		}
	})
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeQueue: %v", err)
	}
	now := time.Now().UTC()
	item := func(id string) nudgequeue.Item {
		return nudgequeue.Item{ID: id, Agent: "alpha/polly", Source: "wait", CreatedAt: now}
	}
	// Pending: the live bucket.
	if err := queue.Enqueue(item("nudge-pending")); err != nil {
		t.Fatalf("enqueueing the pending nudge: %v", err)
	}
	// Dead: a permanent failure dead-letters on the first attempt.
	if err := queue.Enqueue(item("nudge-dead")); err != nil {
		t.Fatalf("enqueueing the dead nudge: %v", err)
	}
	if _, err := queue.RecordFailure([]string{"nudge-dead"}, storebinding.ErrNudgeSessionFenceMismatch, now); err != nil {
		t.Fatalf("dead-lettering a nudge: %v", err)
	}
	// Terminal: a delivered nudge retires into a CLOSED bead, which is the
	// state a classifier is likeliest to be dark on.
	if err := queue.Enqueue(item("nudge-terminal")); err != nil {
		t.Fatalf("enqueueing the terminal nudge: %v", err)
	}
	if err := queue.Ack([]string{"nudge-terminal"}, "delivered", "", ""); err != nil {
		t.Fatalf("acking a nudge: %v", err)
	}
	records, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing the queue's beads: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("the queue left %d bead(s), want 3 (pending, dead, terminal)", len(records))
	}
	return records
}

// TestQueueBeadClassifiesToItsOwningClass pins the gc:nudge-queue arm against
// the queue's own writes, in every durable state it stores.
func TestQueueBeadClassifiesToItsOwningClass(t *testing.T) {
	for _, record := range queueRecords(t) {
		if got := coordclass.Classify(record); got != coordclass.ClassNudges {
			t.Errorf("queue bead %q (status %q) classifies as %s; the nudges class is the one that physically holds it",
				record.ID, record.Status, got)
		}
	}
}

// TestQueueLabelIsNotTheShadowLabel states the reason the arm has to exist at
// all: the two families are different strings, and the nudge arm matches
// exactly. A future edit that made gc:nudge a PREFIX match would pass the test
// above while also swallowing any other gc:nudge* family invented later, so the
// disjointness is pinned in its own right.
func TestQueueLabelIsNotTheShadowLabel(t *testing.T) {
	const shadowLabel = "gc:nudge"
	for _, record := range queueRecords(t) {
		for _, label := range record.Labels {
			if label == shadowLabel {
				t.Fatalf("queue bead %q carries the shadow family label %q; the two families collapsed", record.ID, shadowLabel)
			}
		}
	}
	// A bead carrying neither family is still work: the arm widened by exactly
	// one label, not into a prefix.
	near := beads.Bead{Type: "chore", Labels: []string{"gc:nudger"}}
	if got := coordclass.Classify(near); got != coordclass.ClassWork {
		t.Errorf("a bead labeled gc:nudger classifies as %s; the nudge arms must stay exact", got)
	}
}
