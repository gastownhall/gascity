package dispatch

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// Fixtures for the gc.skipped_blockers contract (gastownhall/gascity#6096):
// a workflow whose finalizer has direct blocks blockers closed
// gc.outcome=skipped still finalizes pass, and the root's pass close records
// the skipped blockers' IDs so the pass is distinguishable from one where
// every sink ran.

// workflowFinalizeFixture is a graph.v2 workflow positioned for its finalizer
// to run: a root, direct blockers closed as their metadata says (their blocks
// edges added in slice order), the finalizer blocked by them, and any extra
// root members (created with no edges, so they are never reached). Tests
// drive the finalizer with ProcessControl and assert on the closed root.
type workflowFinalizeFixture struct {
	store     *beads.MemStore
	root      beads.Bead
	finalizer beads.Bead
	blockers  []beads.Bead
	members   []beads.Bead
}

// finalizeSpec names a workflow-finalize fixture's moving parts: extra root
// metadata over the kind and contract keys, the finalizer's direct blockers,
// and extra root members.
type finalizeSpec struct {
	rootMeta map[string]string
	blockers []beads.Bead
	members  []beads.Bead
	// edgeOrder optionally lists blocker indices in the order their blocks
	// edges are added (default: slice order). Tests use it to add edges out
	// of ID order, since blocker IDs ascend with creation order.
	edgeOrder []int
}

// newWorkflowFinalizeFixture builds the fixture. Blocker and member metadata
// is copied over a gc.root_bead_id pointing at the root, so callers name only
// what differs.
func newWorkflowFinalizeFixture(t *testing.T, spec finalizeSpec) *workflowFinalizeFixture {
	t.Helper()
	store := beads.NewMemStore()
	meta := map[string]string{
		"gc.kind":             "workflow",
		"gc.formula_contract": "graph.v2",
	}
	for k, v := range spec.rootMeta {
		meta[k] = v
	}
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: meta,
	})
	f := &workflowFinalizeFixture{store: store, root: root}
	create := func(b beads.Bead) beads.Bead {
		m := map[string]string{"gc.root_bead_id": root.ID}
		for k, v := range b.Metadata {
			m[k] = v
		}
		return mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    b.Title,
			Type:     "task",
			Status:   b.Status,
			Metadata: m,
		})
	}
	for _, b := range spec.blockers {
		f.blockers = append(f.blockers, create(b))
	}
	f.finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	edgeOrder := spec.edgeOrder
	if edgeOrder == nil {
		for i := range f.blockers {
			edgeOrder = append(edgeOrder, i)
		}
	}
	for _, i := range edgeOrder {
		mustDepAdd(t, store, f.finalizer.ID, f.blockers[i].ID, "blocks")
	}
	mustDepAdd(t, store, root.ID, f.finalizer.ID, "blocks")
	for _, m := range spec.members {
		f.members = append(f.members, create(m))
	}
	return f
}

// drive processes the fixture's finalizer once and fails the test on error.
func (f *workflowFinalizeFixture) drive(t *testing.T) ControlResult {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(f.store), f.finalizer, ProcessOptions{Work: beadstest.Work(f.store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	return result
}

// closedRoot re-reads the workflow root and fails the test unless it closed.
func (f *workflowFinalizeFixture) closedRoot(t *testing.T) beads.Bead {
	t.Helper()
	root := mustGetBead(t, f.store, f.root.ID)
	if root.Status != "closed" {
		t.Fatalf("workflow status = %q, want closed", root.Status)
	}
	return root
}

// TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass pins the pass arm of
// the gc.skipped_blockers contract: a skipped direct blocker does not fail the
// workflow, and the root's pass close records the blocker's ID, so a pass with
// a skipped sink is distinguishable from a pass where every sink ran.
func TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass(t *testing.T) {
	t.Parallel()

	f := newWorkflowFinalizeFixture(t, finalizeSpec{blockers: []beads.Bead{
		{Title: "publish", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
		{Title: "shipped", Status: "closed", Metadata: map[string]string{"gc.outcome": "pass"}},
	}})

	result := f.drive(t)
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	root := f.closedRoot(t)
	if got := root.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("root gc.outcome = %q, want pass", got)
	}
	if got := root.Metadata["gc.skipped_blockers"]; got != f.blockers[0].ID {
		t.Fatalf("root gc.skipped_blockers = %q, want the skipped blocker's ID %q", got, f.blockers[0].ID)
	}
}

// TestProcessWorkflowFinalizeRecordsSkippedBlockersSorted pins the value
// shape of gc.skipped_blockers: with several skipped direct blockers whose
// blocks edges were added out of ID order, the root records exactly the
// skipped IDs, sorted ascending and joined with ",", and no passed blocker's
// ID.
func TestProcessWorkflowFinalizeRecordsSkippedBlockersSorted(t *testing.T) {
	t.Parallel()

	// Blocker IDs ascend with creation (slice) order. edgeOrder adds the
	// blocks edges out of ID order, so a scan that records blockers in edge
	// order instead of sorted order fails the exact-value assertion.
	f := newWorkflowFinalizeFixture(t, finalizeSpec{
		blockers: []beads.Bead{
			{Title: "skipped-a", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
			{Title: "skipped-b", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
			{Title: "passed", Status: "closed", Metadata: map[string]string{"gc.outcome": "pass"}},
			{Title: "skipped-c", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
		},
		edgeOrder: []int{3, 1, 0, 2},
	})

	result := f.drive(t)
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	root := f.closedRoot(t)
	want := []string{f.blockers[0].ID, f.blockers[1].ID, f.blockers[3].ID}
	slices.Sort(want)
	if got := root.Metadata["gc.skipped_blockers"]; got != strings.Join(want, ",") {
		t.Fatalf("root gc.skipped_blockers = %q, want the skipped IDs sorted and comma-joined %q", got, strings.Join(want, ","))
	}
	if strings.Contains(root.Metadata["gc.skipped_blockers"], f.blockers[2].ID) {
		t.Fatalf("root gc.skipped_blockers = %q, must not contain the passed blocker's ID %q", root.Metadata["gc.skipped_blockers"], f.blockers[2].ID)
	}
}

// TestProcessWorkflowFinalizeAllPassLeavesSkippedBlockersAbsent guards the
// marker's absence: with every direct blocker closed pass, the root closes
// pass with no gc.skipped_blockers key, even though the finalize sweep then
// closes a never-reached member gc.outcome=skipped.
func TestProcessWorkflowFinalizeAllPassLeavesSkippedBlockersAbsent(t *testing.T) {
	t.Parallel()

	f := newWorkflowFinalizeFixture(t, finalizeSpec{
		blockers: []beads.Bead{
			{Title: "shipped", Status: "closed", Metadata: map[string]string{"gc.outcome": "pass"}},
		},
		members: []beads.Bead{
			{Title: "never reached", Status: "open"},
		},
	})

	result := f.drive(t)
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	root := f.closedRoot(t)
	if got := root.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("root gc.outcome = %q, want pass", got)
	}
	if _, ok := root.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root carries gc.skipped_blockers = %q, want the key absent when every blocker passed", root.Metadata["gc.skipped_blockers"])
	}
	// The sweep really did close the never-reached member skipped; those
	// closes must not reach the marker.
	member := mustGetBead(t, f.store, f.members[0].ID)
	if member.Status != "closed" || member.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("never-reached member = status %q outcome %q, want closed/skipped by the finalize sweep", member.Status, member.Metadata["gc.outcome"])
	}
}

// TestProcessWorkflowFinalizeSkippedBlockerPassEdgeCases covers the pass-arm
// edges of the gc.skipped_blockers contract.
func TestProcessWorkflowFinalizeSkippedBlockerPassEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("whitespace-padded outcome is recorded", func(t *testing.T) {
		t.Parallel()

		f := newWorkflowFinalizeFixture(t, finalizeSpec{blockers: []beads.Bead{
			{Title: "publish", Status: "closed", Metadata: map[string]string{"gc.outcome": " skipped "}},
		}})

		if result := f.drive(t); !result.Processed || result.Action != "workflow-pass" {
			t.Fatalf("result = %+v, want processed workflow-pass", result)
		}
		root := f.closedRoot(t)
		if got := root.Metadata["gc.skipped_blockers"]; got != f.blockers[0].ID {
			t.Fatalf("root gc.skipped_blockers = %q, want the skipped blocker's ID %q", got, f.blockers[0].ID)
		}
	})

	t.Run("finalizer keeps no skip close_reason after a pass with a skipped blocker", func(t *testing.T) {
		t.Parallel()

		// Regression row for the report's secondary mechanism: the finalize
		// sweep must not close the finalizer itself with the skip-directive
		// close_reason, leaving a pass-closed finalizer that reads as skipped.
		f := newWorkflowFinalizeFixture(t, finalizeSpec{blockers: []beads.Bead{
			{Title: "publish", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
		}})

		if result := f.drive(t); !result.Processed || result.Action != "workflow-pass" {
			t.Fatalf("result = %+v, want processed workflow-pass", result)
		}
		finalizerAfter := mustGetBead(t, f.store, f.finalizer.ID)
		if got := finalizerAfter.Metadata["close_reason"]; got == sourceworkflow.WorkflowSkippedCloseReason {
			t.Fatalf("finalizer close_reason = %q, want anything but the skip-directive reason", got)
		}
	})

	t.Run("crash retry after the root close leaves the same marker value", func(t *testing.T) {
		t.Parallel()

		// The root closes before the finalizer; a crash between the two
		// leaves an open finalizer whose retry re-runs processWorkflowFinalize
		// over the already-closed root. The marker must keep the same value.
		f := newWorkflowFinalizeFixture(t, finalizeSpec{blockers: []beads.Bead{
			{Title: "publish", Status: "closed", Metadata: map[string]string{"gc.outcome": "skipped"}},
			{Title: "shipped", Status: "closed", Metadata: map[string]string{"gc.outcome": "pass"}},
		}})

		if result := f.drive(t); !result.Processed || result.Action != "workflow-pass" {
			t.Fatalf("result = %+v, want processed workflow-pass", result)
		}
		root := f.closedRoot(t)
		want := root.Metadata["gc.skipped_blockers"]
		if want == "" {
			t.Fatal("first run left no gc.skipped_blockers to retry against")
		}
		retryFinalizer := f.finalizer // still open: the crash window's stale row
		if _, err := processWorkflowFinalize(beadstest.Graph(f.store), retryFinalizer, ProcessOptions{Work: beadstest.Work(f.store)}); err != nil {
			t.Fatalf("retry processWorkflowFinalize: %v", err)
		}
		rootAfter := f.closedRoot(t)
		if got := rootAfter.Metadata["gc.skipped_blockers"]; got != want {
			t.Fatalf("root gc.skipped_blockers after retry = %q, want the same value %q", got, want)
		}
		if strings.Count(rootAfter.Metadata["gc.skipped_blockers"], f.blockers[0].ID) != 1 {
			t.Fatalf("root gc.skipped_blockers = %q, want the skipped ID exactly once", rootAfter.Metadata["gc.skipped_blockers"])
		}
	})
}
