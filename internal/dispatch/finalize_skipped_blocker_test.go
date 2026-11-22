package dispatch

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// skippedBlockerFixture is a graph.v2 workflow root, its finalizer, and the
// finalizer's direct blocks blockers, all in one MemStore.
type skippedBlockerFixture struct {
	store     beads.Store
	root      beads.Bead
	finalizer beads.Bead
	blockers  []beads.Bead
}

// newSkippedBlockerFixture creates a workflow root and one closed step bead per
// entry of blockerMeta (each entry merged over the step's root membership),
// then a finalizer blocked by every step. Blockers are created in slice order
// and their blocks edges added in edgeOrder (indexes into blockerMeta; nil
// means slice order).
func newSkippedBlockerFixture(t *testing.T, store beads.Store, blockerMeta []map[string]string, edgeOrder []int) skippedBlockerFixture {
	t.Helper()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	f := skippedBlockerFixture{store: store, root: root}
	for _, extra := range blockerMeta {
		meta := map[string]string{"gc.root_bead_id": root.ID}
		maps.Copy(meta, extra)
		f.blockers = append(f.blockers, mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "step",
			Type:     "task",
			Status:   "closed",
			Metadata: meta,
		}))
	}
	f.finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	if edgeOrder == nil {
		for i := range f.blockers {
			edgeOrder = append(edgeOrder, i)
		}
	}
	for _, i := range edgeOrder {
		mustDepAdd(t, store, f.finalizer.ID, f.blockers[i].ID, "blocks")
	}
	mustDepAdd(t, store, root.ID, f.finalizer.ID, "blocks")
	return f
}

// finalize processes the fixture's finalizer and returns the control result
// and the root as stored afterwards.
func (f skippedBlockerFixture) finalize(t *testing.T) (ControlResult, beads.Bead) {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(f.store), f.finalizer, ProcessOptions{Work: beadstest.Work(f.store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	return result, mustGetBead(t, f.store, f.root.ID)
}

func outcomeMeta(outcome string) map[string]string {
	return map[string]string{"gc.outcome": outcome}
}

// TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass pins #6096: a
// workflow whose finalizer blocker closed gc.outcome=skipped still passes,
// and the root's pass close records that blocker in gc.skipped_blockers.
func TestProcessWorkflowFinalizeRecordsSkippedBlockerOnPass(t *testing.T) {
	t.Parallel()

	f := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		outcomeMeta("pass"),
		outcomeMeta("skipped"),
	}, nil)
	result, root := f.finalize(t)

	if result.Action != "workflow-pass" {
		t.Fatalf("action = %q, want workflow-pass", result.Action)
	}
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root status=%q gc.outcome=%q, want closed/pass", root.Status, root.Metadata["gc.outcome"])
	}
	if got, want := root.Metadata["gc.skipped_blockers"], f.blockers[1].ID; got != want {
		t.Fatalf("root gc.skipped_blockers = %q, want %q", got, want)
	}
}

// TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass is the
// triage repro for #6096: a native step dependency force-closed via the skip
// directive must not finalize the root in a state identical to one whose step
// actually completed.
func TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass(t *testing.T) {
	t.Parallel()

	step := func(meta map[string]string) map[string]string {
		meta["gc.step_ref"] = "build-from-plan.publish"
		return meta
	}
	_, passed := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		step(outcomeMeta("pass")),
	}, nil).finalize(t)
	_, skipped := newSkippedBlockerFixture(t, beads.NewMemStore(), []map[string]string{
		step(map[string]string{
			"gc.outcome":   "skipped",
			"close_reason": sourceworkflow.WorkflowSkippedCloseReason,
		}),
	}, nil).finalize(t)

	if maps.Equal(passed.Metadata, skipped.Metadata) {
		t.Fatalf("root after a skip-directive-closed native step is indistinguishable from a real pass: both closed with metadata %v", skipped.Metadata)
	}
}
