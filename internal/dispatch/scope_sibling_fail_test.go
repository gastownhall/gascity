package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestScopeMemberPassAfterSiblingFailureClosesBodyFail covers gastownhall/gascity#9507:
// member B failed and the scope aborted while member A was still in progress.
// When A later closes pass, the reconcile must settle the scope as failed (the
// abort already decided it), not close the body pass.
func TestScopeMemberPassAfterSiblingFailureClosesBodyFail(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "member B (failed)",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.b",
			"gc.outcome":      "fail",
		},
	})
	inFlight := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "member A (in flight)",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.a",
		},
	})
	mustDep(t, store, body.ID, failed.ID, "blocks")
	mustDep(t, store, body.ID, inFlight.ID, "blocks")

	// A worker closes A as pass: the real close path, not a hand-written status.
	if err := setOutcomeAndClose(beadstest.Graph(store), inFlight.ID, "pass"); err != nil {
		t.Fatalf("setOutcomeAndClose(A pass): %v", err)
	}
	if _, err := reconcileClosedScopeMember(beadstest.Graph(store), inFlight.ID); err != nil {
		t.Fatalf("reconcileClosedScopeMember(A): %v", err)
	}

	bodyAfter := mustGet(t, store, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed once A settled", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("body gc.outcome = %q, want fail: the scope aborted when B failed", got)
	}
}
