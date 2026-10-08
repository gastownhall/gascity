package main

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The drain-begin effect's tests (CONTRACT v5 D1, D2, F4, C8.9).

// admittedBegin seeds gc row (drainRow) on a require-stamped store with a
// live runtime on a fence leaf, decides it with entry, and returns the pass
// that admitted the begin, the begin, the store and the leaf.
func admittedBegin(t *testing.T, entry func(*selectionEntry)) (*effectPass, intent, beads.Store, *fenceLeaf) {
	t.Helper()
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("last_woke_at", rowAt(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "s-gc-1", nil)
	k := rowKey{Leg: rowLeg, ID: b.ID}
	w := &World{
		Now: gatherNow, Env: &reconcileEnv{SP: leaf}, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)),
		LegStores: map[string]beads.Store{rowLeg: store},
	}
	e := &selectionEntry{Key: k, Liveness: livenessAlive}
	entry(e)
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: e}}}
	it, _ := decideRow(w, a, k)
	return newEffectPass(w, a), it, store, leaf
}

func runBegin(p *effectPass, it intent) settlement {
	return effectRegistry[it.Kind](p, it)(context.Background())
}

// TestDrainBeginIdleNeedsFreshDetachAndNoPending (C8.9, R26). Kills an idle
// or no-wake-reason begin that skips the fresh attach or pending read, or
// writes while either holds.
func TestDrainBeginIdleNeedsFreshDetachAndNoPending(t *testing.T) {
	for _, reason := range []string{reasonIdleSleep, ""} {
		sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reason }
		for _, c := range []struct {
			name  string
			setup func(*fenceLeaf)
			cause string
		}{
			{"attached", func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }, fenceAttached},
			{"pending", func(l *fenceLeaf) {
				l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
			}, fencePending},
			{"detached, nothing pending", func(*fenceLeaf) {}, ""},
		} {
			p, it, store, leaf := admittedBegin(t, sleep)
			if it.Kind != intentDrainBeginFresh {
				t.Fatalf("%q: %+v, want the fresh begin", reason, it)
			}
			c.setup(leaf)
			s := runBegin(p, it)
			got, _ := store.Get(it.Key.ID)
			switch {
			case c.cause != "" && (s.Outcome != settledRefused || s.Cause != c.cause || got.Metadata[drainIntentReasonKey] != ""):
				t.Fatalf("%q %s: %+v %v, want refused %q and no request", reason, c.name, s, got.Metadata, c.cause)
			case c.cause == "" && (s.Outcome != settledLanded || got.Metadata[drainIntentReasonKey] == ""):
				t.Fatalf("%q %s: %+v %v, want the request landed", reason, c.name, s, got.Metadata)
			}
		}
	}
}

// TestDrainBeginOrphanedProceedsWhileAttached (D2). Kills a fresh-leg probe
// on the undesired reasons: an orphaned begin is a plain row write and lands
// on an attached runtime (the signal and the stop verb fence it later).
func TestDrainBeginOrphanedProceedsWhileAttached(t *testing.T) {
	p, it, store, leaf := admittedBegin(t, undesired(drainOrphaned))
	leaf.SetAttached("s-gc-1", true)
	if it.Kind != intentDrainBegin {
		t.Fatalf("%+v, want the plain begin", it)
	}
	if s := runBegin(p, it); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if got, _ := store.Get(it.Key.ID); got.Metadata[drainIntentReasonKey] != drainOrphaned {
		t.Fatalf("row %v, want the orphaned request", got.Metadata)
	}
}

// TestDrainRequestWritesNewKeysOnly (v5 R1, I7). Kills BeginDrainPatch's
// state=draining, which legacy skips as an unknown state after a rollback:
// the begin changes the three controller keys and nothing else, and the
// next pass reads it back as the requested drain.
func TestDrainRequestWritesNewKeysOnly(t *testing.T) {
	p, it, store, _ := admittedBegin(t, undesired(drainSuspended))
	before, _ := store.Get(it.Key.ID)
	if s := runBegin(p, it); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	after, _ := store.Get(it.Key.ID)
	changed := maps.Clone(after.Metadata)
	maps.DeleteFunc(changed, func(k, v string) bool { return before.Metadata[k] == v })
	want := map[string]string{drainIntentReasonKey: drainSuspended, drainIntentAtKey: gatherNow.Format(time.RFC3339), drainIntentIncarnationKey: "3"}
	if !maps.Equal(changed, want) {
		t.Fatalf("the begin changed %v, want only %v", changed, want)
	}
	row := readCensus(t, gatherNow, censusLegs(rowLeg, store)).Rows[it.Key]
	if req, ok := activeStop(row); !ok || req.Phase != stopRequested || req.Reason != drainSuspended {
		t.Fatalf("activeStop after the begin = %+v, %v; want the requested drain", req, ok)
	}
}
