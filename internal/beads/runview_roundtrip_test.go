package beads_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

type beadSeed struct {
	eventType string
	bead      beads.Bead
}

// TestRunViewRoundTripFromNotifyChange is the anchor guardrail from the run-view
// RCA: a run-shaped bead is emitted through the REAL producer
// (beads.CachingStore.notifyChange) to build real events.Events, those events
// are folded by runproj.Fold and projected by runproj.BuildRunSummary, and the
// summary must contain the seeded run. Producer and consumer were previously
// tested in separate hermetic bubbles, so an event-shape or metadata-migration
// drift between them could not register. This test spans the exact seam that
// broke: if the fold starves (wire-shape mismatch, silent drop), the summary
// comes back empty and this fails.
func TestRunViewRoundTripFromNotifyChange(t *testing.T) {
	created := time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)
	// A graph.v2 run root (gc.formula_contract=graph.v2 / gc.kind=run) plus a
	// child step bead carrying gc.step_id — the shape the run view groups on.
	root := beads.Bead{
		ID:        "gcg-run-root",
		Title:     "adopt PR #7",
		Status:    "in_progress",
		Type:      "molecule",
		CreatedAt: created,
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.KindMetadataKey:            "run",
			beadmeta.FormulaMetadataKey:         "mol-adopt-pr-v2",
		},
	}
	step := beads.Bead{
		ID:        "gcg-run-step",
		Title:     "review",
		Status:    "in_progress",
		Type:      "task",
		CreatedAt: created,
		ParentID:  "gcg-run-root",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "gcg-run-root",
			beadmeta.StepIDMetadataKey:     "gcg-run-step",
		},
	}

	evts := recordThroughNotifyChange(t,
		beadSeed{events.BeadCreated, root},
		beadSeed{events.BeadCreated, step},
	)

	folded := runproj.Fold(evts)
	if len(folded) != 2 {
		t.Fatalf("fold size = %d, want 2 (root + step); the fold starved", len(folded))
	}

	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(beadsInSeenOrder(evts, folded)))
	if summary.TotalActive == 0 {
		t.Fatalf("run summary has no active lanes — the projection starved.\nsummary=%+v", summary)
	}
	var lane *runproj.RunLane
	for i := range summary.Lanes {
		if summary.Lanes[i].ID == "gcg-run-root" {
			lane = &summary.Lanes[i]
		}
	}
	if lane == nil {
		t.Fatalf("seeded run gcg-run-root not present in summary lanes: %+v", summary.Lanes)
	}
	if lane.Formula.Status != "known" || lane.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("lane formula = %+v, want known/mol-adopt-pr-v2", lane.Formula)
	}
}

// recordThroughNotifyChange emits each seed through the real
// CachingStore.notifyChange producer and captures the events.Events the record
// site (cmd/gc/api_state.go onChange closure) would build from the
// (eventType, id, runID, sessionID, stepID, payload) callback.
func recordThroughNotifyChange(t *testing.T, seeds ...beadSeed) []events.Event {
	t.Helper()
	var out []events.Event
	seq := uint64(0)
	cs := beads.NewCachingStore(beads.NewMemStore(), func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage) {
		seq++
		out = append(out, events.Event{
			Seq:       seq,
			Type:      eventType,
			Actor:     "cache-reconcile",
			Subject:   beadID,
			RunID:     runID,
			SessionID: sessionID,
			StepID:    stepID,
			Payload:   payload,
		})
	})
	for _, s := range seeds {
		cs.NotifyChangeForTest(s.eventType, s.bead)
	}
	if len(out) != len(seeds) {
		t.Fatalf("emitted %d events, want %d (one per seed)", len(out), len(seeds))
	}
	return out
}

// beadsInSeenOrder returns the folded beads in the order their ids first appear
// in the event stream, mirroring the Projector's first-seen ordering that
// BuildRunSummary expects (a plain Fold map has random iteration order).
func beadsInSeenOrder(evts []events.Event, folded map[string]beads.Bead) []beads.Bead {
	var order []beads.Bead
	seen := map[string]bool{}
	for _, e := range evts {
		b, ok := folded[e.Subject]
		if !ok || seen[e.Subject] {
			continue
		}
		seen[e.Subject] = true
		order = append(order, b)
	}
	return order
}
