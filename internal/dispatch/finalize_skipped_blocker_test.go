package dispatch

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// finalizeBlockersFixture builds, in store, an open workflow root and its
// workflow-finalize control, with one closed blocker per entry of outcomes
// (each stamped with that gc.outcome) wired to the finalizer in creation
// order. rootMeta is merged into the root's base metadata (cancel marks,
// quarantine keys, ...).
func finalizeBlockersFixture(t *testing.T, store beads.Store, rootMeta map[string]string, outcomes []string) (beads.Bead, beads.Bead) {
	t.Helper()
	meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}
	maps.Copy(meta, rootMeta)
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: meta,
	})
	var blockerIDs []string
	for i, outcome := range outcomes {
		blocker := mustCreateWorkflowBead(t, store, beads.Bead{
			Title: fmt.Sprintf("step %d", i+1), Type: "task", Status: "closed",
			Metadata: map[string]string{"gc.outcome": outcome},
		})
		blockerIDs = append(blockerIDs, blocker.ID)
	}
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow", Type: "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": root.ID},
	})
	for _, id := range blockerIDs {
		mustDepAdd(t, store, finalizer.ID, id, "blocks")
	}
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return root, finalizer
}

// The negative side of the marker: a root whose every blocker passed closes
// with no gc.skipped_blockers key at all — not an empty value — so watchers
// can key on presence.
func TestProcessWorkflowFinalizeAllPassedWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, finalizer := finalizeBlockersFixture(t, store, nil, []string{"pass", "pass", "pass"})

	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = %s/%q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root gc.skipped_blockers = %q, want the key absent (no blocker was skipped)", got)
	}
}

// A skipped blocker only marks a PASS. Every route that settles the workflow
// any other way — a failed sibling blocker, a control-quarantined root, a
// cancel requested on the root — keeps today's outcome and writes no
// gc.skipped_blockers key.
func TestProcessWorkflowFinalizeNonPassWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	t.Run("failed blocker", func(t *testing.T) {
		t.Parallel()

		store := beads.NewMemStore()
		root, finalizer := finalizeBlockersFixture(t, store, nil, []string{"skipped", "fail"})

		result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
		if err != nil {
			t.Fatalf("ProcessControl(workflow-finalize): %v", err)
		}
		if !result.Processed || result.Action != "workflow-fail" {
			t.Fatalf("result = %+v, want processed workflow-fail", result)
		}
		rootAfter, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("get root: %v", err)
		}
		if rootAfter.Metadata["gc.outcome"] != "fail" {
			t.Fatalf("root gc.outcome = %q, want fail", rootAfter.Metadata["gc.outcome"])
		}
		if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
			t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a failed workflow", got)
		}
	})

	t.Run("control-quarantined root", func(t *testing.T) {
		t.Parallel()

		store := beads.NewMemStore()
		root, finalizer := quarantinedWorkflowFixture(t, store, "closed", controlQuarantinedRootMetadata())
		skipped := mustCreateWorkflowBead(t, store, beads.Bead{
			Title: "skipped", Type: "task", Status: "closed",
			Metadata: map[string]string{"gc.outcome": "skipped"},
		})
		mustDepAdd(t, store, finalizer.ID, skipped.ID, "blocks")

		result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
		if err != nil {
			t.Fatalf("ProcessControl(workflow-finalize): %v", err)
		}
		if !result.Processed || result.Action != "workflow-fail" {
			t.Fatalf("result = %+v, want processed workflow-fail (a quarantined root is a failed workflow)", result)
		}
		rootAfter, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("get root: %v", err)
		}
		if rootAfter.Metadata["gc.outcome"] != "fail" {
			t.Fatalf("root gc.outcome = %q, want fail", rootAfter.Metadata["gc.outcome"])
		}
		if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
			t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a quarantined root", got)
		}
	})

	t.Run("cancel requested", func(t *testing.T) {
		t.Parallel()

		store := beads.NewMemStore()
		root, finalizer := finalizeBlockersFixture(t, store,
			map[string]string{"gc.cancel_requested": "true"},
			[]string{"pass", "skipped"})

		result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
		if err != nil {
			t.Fatalf("ProcessControl(workflow-finalize): %v", err)
		}
		if !result.Processed || result.Action != "workflow-canceled" {
			t.Fatalf("result = %+v, want processed workflow-canceled", result)
		}
		rootAfter, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("get root: %v", err)
		}
		if rootAfter.Metadata["gc.outcome"] != "canceled" {
			t.Fatalf("root gc.outcome = %q, want canceled", rootAfter.Metadata["gc.outcome"])
		}
		if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
			t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a canceled workflow", got)
		}
	})
}

// The marker rides the root close's existing metadata write: a pass with
// skipped blockers costs no more store calls than a pass without them (the
// blocker scan already read every blocker), matching the pinned
// workflow-finalize budgets in call_budget_test.go.
func TestProcessWorkflowFinalizeSkipMarkerCostsNoStoreCall(t *testing.T) {
	t.Parallel()

	run := func(outcomes []string) (int, int) {
		counting := &callCountingStore{Store: beads.NewMemStore()}
		_, finalizer := finalizeBlockersFixture(t, counting, nil, outcomes)
		counting.reset()
		result, err := ProcessControl(beadstest.Graph(counting), finalizer, ProcessOptions{Work: beadstest.Work(counting)})
		if err != nil {
			t.Fatalf("ProcessControl(outcomes %v): %v", outcomes, err)
		}
		if !result.Processed || result.Action != "workflow-pass" {
			t.Fatalf("outcomes %v: result = %+v, want processed workflow-pass", outcomes, result)
		}
		return counting.reads, counting.writes
	}

	markedReads, markedWrites := run([]string{"skipped", "pass", "skipped"})
	cleanReads, cleanWrites := run([]string{"pass", "pass", "pass"})
	if markedReads != cleanReads || markedWrites != cleanWrites {
		t.Fatalf("workflow-finalize with skipped blockers made %d reads / %d writes, all-passed made %d / %d; the marker must ride the existing close write",
			markedReads, markedWrites, cleanReads, cleanWrites)
	}
}

// The close-metadata helper is the marker's only gate: pass with skips adds
// the key, everything else is the one-key outcome map setOutcomeAndClose
// writes.
func TestWorkflowRootCloseMetadata(t *testing.T) {
	t.Parallel()

	oneKey := func(outcome string) map[string]string {
		return map[string]string{beadmeta.OutcomeMetadataKey: outcome}
	}
	for _, tc := range []struct {
		name    string
		outcome string
		skipped []string
		want    map[string]string
	}{
		{name: "pass with no blockers", outcome: beadmeta.OutcomePass, want: oneKey(beadmeta.OutcomePass)},
		{name: "pass with empty list", outcome: beadmeta.OutcomePass, skipped: []string{}, want: oneKey(beadmeta.OutcomePass)},
		{
			// The scan sorts before handing the list down; the helper joins
			// what it is given.
			name:    "pass with skipped blockers",
			outcome: beadmeta.OutcomePass,
			skipped: []string{"bd-a", "bd-b"},
			want: map[string]string{
				beadmeta.OutcomeMetadataKey:         beadmeta.OutcomePass,
				beadmeta.SkippedBlockersMetadataKey: "bd-a,bd-b",
			},
		},
		{name: "fail with skipped blockers", outcome: beadmeta.OutcomeFail, skipped: []string{"bd-a"}, want: oneKey(beadmeta.OutcomeFail)},
		{name: "canceled with skipped blockers", outcome: beadmeta.OutcomeCanceled, skipped: []string{"bd-a", "bd-b"}, want: oneKey(beadmeta.OutcomeCanceled)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := workflowRootCloseMetadata(tc.outcome, tc.skipped); !maps.Equal(got, tc.want) {
				t.Fatalf("workflowRootCloseMetadata(%q, %v) = %v, want %v", tc.outcome, tc.skipped, got, tc.want)
			}
		})
	}
}

// A workflow-finalize whose direct blocks blockers include steps closed
// gc.outcome=skipped still passes (a skip is deliberately not a failure), but
// the root must say so: gc.skipped_blockers lists the skipped blockers' IDs,
// sorted and comma-joined, so a pass earned with skipped work reads
// differently from one whose every blocker ran (#6096).
func TestProcessWorkflowFinalizeMarksSkippedBlockersOnPass(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	passed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "ran", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "pass"},
	})
	skippedA := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "skipped a", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "skipped"},
	})
	skippedB := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "skipped b", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "skipped"},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow", Type: "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": root.ID},
	})
	// Edges added in descending-blocker order: MemStore returns dependencies
	// in insertion order, so the skipped IDs reach the scan unsorted.
	mustDepAdd(t, store, finalizer.ID, skippedB.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, passed.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, skippedA.ID, "blocks")
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")

	// Precondition: the skipped blockers really do arrive unsorted
	// (skippedB before skippedA), so the assertion below tests the marker's
	// sort, not the store's edge ordering.
	deps, err := store.DepList(finalizer.ID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	var depOrder []string
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		if dep.DependsOnID == skippedA.ID || dep.DependsOnID == skippedB.ID {
			depOrder = append(depOrder, dep.DependsOnID)
		}
	}
	if slices.IsSorted(depOrder) {
		t.Fatalf("precondition: skipped blockers' dependency-list order %v is sorted; add the edges in an order that sorts the other way", depOrder)
	}

	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass (a skip is not a failure)", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = %s/%q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	want := []string{skippedA.ID, skippedB.ID}
	slices.Sort(want)
	if got := rootAfter.Metadata["gc.skipped_blockers"]; got != strings.Join(want, ",") {
		t.Fatalf("root gc.skipped_blockers = %q, want %q (a pass with skipped blockers must be marked)", got, strings.Join(want, ","))
	}
}
