package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestFilterRunBeads pins the projection-boundary filter policy: engineering
// types, formula-v1 step beads and gc.kind=run roots are kept, while gc:-labeled
// control beads and non-run bookkeeping types (message/session) are dropped. This
// is the analog of the frontend runBeadFilter (summary.ts) that the live tailer
// applies before the pure summary/detail builders.
func TestFilterRunBeads(t *testing.T) {
	in := []beads.Bead{
		{ID: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
		{ID: "task", Type: "task"},
		{ID: "bug", Type: "bug"},
		{ID: "step", Type: "step", ParentID: "root"},
		{ID: "ctl", Type: "task", Labels: []string{"gc:control"}},
		{ID: "msg", Type: "message", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
		{ID: "sess", Type: "session"},
		{ID: "run-labeled", Type: "molecule", Labels: []string{"gc:workflow"}, Metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
	}

	got := FilterRunBeads(in)

	gotIDs := map[string]bool{}
	for _, b := range got {
		gotIDs[b.ID] = true
	}
	want := map[string]bool{"root": true, "task": true, "bug": true, "step": true}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("FilterRunBeads dropped %q; it should be kept", id)
		}
	}
	for _, id := range []string{"ctl", "msg", "sess", "run-labeled"} {
		if gotIDs[id] {
			t.Errorf("FilterRunBeads kept %q; a gc:-labeled or non-run bead must be dropped", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("FilterRunBeads returned %d beads, want %d (%v)", len(got), len(want), gotIDs)
	}
}

// TestFilterRunBeadsKeepsFormulaV1Steps pins the run projection of a formula-v1
// molecule. Instantiation types each untyped or "task"-typed non-root v1 step
// "step" (so Ready skips formula scaffolding, #1039) and links it to the
// molecule root by ParentID alone: a plain v1 step carries no gc.root_bead_id.
// Dropping the step type at the filter left such a run with a root and no
// steps, so it read pending even while its steps were claimed and closed.
func TestFilterRunBeadsKeepsFormulaV1Steps(t *testing.T) {
	root := beads.Bead{ID: "gc-1", Title: "v1 run", Type: "molecule", Status: "open"}
	claimed := beads.Bead{
		ID: "gc-2", Title: "Load context", Type: "step", Status: "in_progress", ParentID: root.ID,
		Metadata: beads.StringMap{beadmeta.StepRefMetadataKey: "mol-v1.load"},
	}
	queued := beads.Bead{
		ID: "gc-3", Title: "Do the work", Type: "step", Status: "open", ParentID: root.ID,
		Metadata: beads.StringMap{beadmeta.StepRefMetadataKey: "mol-v1.work"},
	}
	// A step under a parent that is not a run: the filter keeps it, and the
	// projection must still not make a run of it or of its parent.
	epic := beads.Bead{ID: "gc-4", Title: "plain epic", Type: "epic", Status: "open"}
	stray := beads.Bead{ID: "gc-5", Title: "stray step", Type: "step", Status: "in_progress", ParentID: epic.ID}

	filtered := FilterRunBeads([]beads.Bead{root, claimed, queued, epic, stray})
	keptIDs := map[string]bool{}
	for _, b := range filtered {
		keptIDs[b.ID] = true
	}
	for _, id := range []string{claimed.ID, queued.ID, stray.ID} {
		if !keptIDs[id] {
			t.Fatalf("FilterRunBeads dropped formula-v1 step %q; kept %v", id, keptIDs)
		}
	}

	summary, lanes := BuildRunSummaryWithAllLanes(filtered)
	if len(lanes) != 1 || lanes[0].ID != root.ID {
		laneIDs := make([]string, 0, len(lanes))
		for _, lane := range lanes {
			laneIDs = append(laneIDs, lane.ID)
		}
		t.Fatalf("run lanes = %v, want only the molecule root %q (no phantom run for a step outside a run)", laneIDs, root.ID)
	}
	if summary.TotalActive != 1 {
		t.Fatalf("summary.TotalActive = %d, want 1", summary.TotalActive)
	}

	memberIDs := map[string]bool{}
	for _, b := range RunMembers(filtered, root.ID) {
		memberIDs[b.ID] = true
	}
	for _, id := range []string{root.ID, claimed.ID, queued.ID} {
		if !memberIDs[id] {
			t.Fatalf("RunMembers(%q) = %v, missing %q", root.ID, memberIDs, id)
		}
	}
	if memberIDs[stray.ID] {
		t.Fatalf("RunMembers(%q) = %v, includes the step of another parent %q", root.ID, memberIDs, stray.ID)
	}

	got := CountCanonicalRunStatuses(filtered, lanes)
	want := CanonicalRunStatusCounts{Active: 1}
	if got != want {
		t.Fatalf("CountCanonicalRunStatuses() = %+v, want %+v (a claimed v1 step makes its run active)", got, want)
	}
}
