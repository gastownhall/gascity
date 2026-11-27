package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphroute"
)

// These tests pin issue #9507 through the control dispatcher's serve drain: a
// scope that aborts while a member is claimed leaves that member to its worker
// and settles once the worker closes it, with no bead.closed event to wake on.

// claimedAbortWorkflow is a graph.v2 workflow whose scope holds member A,
// which fails, and member B, which a worker holds while A's failure aborts the
// scope. Every member has its scope-check, and workflow-finalize blocks on the
// body and on every scope-check.
type claimedAbortWorkflow struct {
	root, body, a, aCheck, b, bCheck, finalize beads.Bead
}

func newClaimedAbortWorkflow(t *testing.T, store *beads.MemStore) claimedAbortWorkflow {
	t.Helper()
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		if b.Metadata == nil {
			b.Metadata = map[string]string{}
		}
		b.Metadata[beadmeta.FormulaContractMetadataKey] = beadmeta.FormulaContractGraphV2
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %s: %v", b.Title, err)
		}
		return created
	}
	blocks := func(from, to beads.Bead) {
		t.Helper()
		if err := store.DepAdd(from.ID, to.ID, "blocks"); err != nil {
			t.Fatalf("dep %s blocks on %s: %v", from.ID, to.ID, err)
		}
	}
	var wf claimedAbortWorkflow
	wf.root = create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
	}})
	inRoot := func(meta map[string]string) map[string]string {
		meta[beadmeta.RootBeadIDMetadataKey] = wf.root.ID
		return meta
	}
	wf.body = create(beads.Bead{Title: "body", Type: "task", Metadata: inRoot(map[string]string{
		beadmeta.KindMetadataKey:      beadmeta.KindScope,
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody,
		beadmeta.StepRefMetadataKey:   "demo.body",
	})})
	member := func(step string) beads.Bead {
		return create(beads.Bead{Title: step, Type: "task", Metadata: inRoot(map[string]string{
			beadmeta.ScopeRefMetadataKey:  "body",
			beadmeta.ScopeRoleMetadataKey: "member",
			beadmeta.StepRefMetadataKey:   "demo." + step,
			beadmeta.OnFailMetadataKey:    "abort_scope",
		})})
	}
	check := func(subject beads.Bead, step string) beads.Bead {
		created := create(beads.Bead{Title: "Finalize scope for " + step, Type: "task", Metadata: inRoot(map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindScopeCheck,
			beadmeta.ScopeRefMetadataKey:  "body",
			beadmeta.ScopeRoleMetadataKey: "control",
			beadmeta.StepRefMetadataKey:   "demo." + step + "-scope-check",
		})})
		blocks(created, subject)
		return created
	}
	wf.a = member("step-a")
	wf.aCheck = check(wf.a, "step-a")
	wf.b = member("step-b")
	wf.bCheck = check(wf.b, "step-b")
	blocks(wf.body, wf.a)
	blocks(wf.body, wf.b)
	wf.finalize = create(beads.Bead{Title: "Finalize workflow", Type: "task", Metadata: inRoot(map[string]string{
		beadmeta.KindMetadataKey:    beadmeta.KindWorkflowFinalize,
		beadmeta.StepRefMetadataKey: "demo.workflow-finalize",
	})})
	for _, blocker := range []beads.Bead{wf.body, wf.aCheck, wf.bCheck} {
		blocks(wf.finalize, blocker)
	}

	status, assignee := "in_progress", "worker-b"
	if err := store.Update(wf.b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("claim %s: %v", wf.b.ID, err)
	}
	if err := store.SetMetadataBatch(wf.a.ID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: "tests_failed",
	}); err != nil {
		t.Fatalf("fail %s: %v", wf.a.ID, err)
	}
	if err := store.Close(wf.a.ID); err != nil {
		t.Fatalf("close %s: %v", wf.a.ID, err)
	}
	return wf
}

// serveReadyControlRows answers the serve drain's queue the way the
// control-dispatcher route does: every open control bead whose blockers have
// all closed.
func serveReadyControlRows(t *testing.T, store *beads.MemStore) func() []hookBead {
	return func() []hookBead {
		var rows []hookBead
		for _, b := range memGraphReady(t, store) {
			if graphroute.IsControlDispatcherKind(b.Metadata[beadmeta.KindMetadataKey]) {
				rows = append(rows, serveQueueRow(b))
			}
		}
		return rows
	}
}

func requireBeadState(t *testing.T, store *beads.MemStore, label, id, wantStatus, wantOutcome string) beads.Bead {
	t.Helper()
	got := mustGetMemBead(t, store, id)
	if got.Status != wantStatus || got.Metadata[beadmeta.OutcomeMetadataKey] != wantOutcome {
		t.Fatalf("%s %s = %s/%q, want %s/%q", label, id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], wantStatus, wantOutcome)
	}
	return got
}

// TestServeDrainSettlesScopeAbortedWhileMemberClaimed is AC-8: the scope
// aborts while worker B runs, B's worker closes B passed straight in the store,
// and the next serve drain settles the body and the workflow failed while B
// keeps its worker's outcome.
func TestServeDrainSettlesScopeAbortedWhileMemberClaimed(t *testing.T) {
	cityDir, store := setUpServeSkipCity(t)
	wf := newClaimedAbortWorkflow(t, store)
	stubServeQueue(t, serveReadyControlRows(t, store))

	drainServeSkipCity(t, cityDir)

	requireBeadState(t, store, "A's scope-check", wf.aCheck.ID, "closed", beadmeta.OutcomePass)
	requireBeadState(t, store, "scope body", wf.body.ID, "open", beadmeta.OutcomeFail)
	requireBeadState(t, store, "B's scope-check", wf.bCheck.ID, "open", "")
	requireBeadState(t, store, "workflow root", wf.root.ID, "open", "")
	if b := requireBeadState(t, store, "worker B", wf.b.ID, "in_progress", ""); b.Assignee != "worker-b" {
		t.Fatalf("worker B assignee = %q, want worker-b", b.Assignee)
	}

	// B's worker closes B in the store directly: no bead.closed event.
	if err := store.SetMetadataBatch(wf.b.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("worker B outcome: %v", err)
	}
	if err := store.Close(wf.b.ID); err != nil {
		t.Fatalf("worker B close: %v", err)
	}

	result := drainServeSkipCity(t, cityDir)

	if !result.processedAny {
		t.Errorf("drain result = %+v, want B's scope-check processed", result)
	}
	requireBeadState(t, store, "B's scope-check", wf.bCheck.ID, "closed", beadmeta.OutcomePass)
	body := requireBeadState(t, store, "scope body", wf.body.ID, "closed", beadmeta.OutcomeFail)
	if got := body.Metadata[beadmeta.FailureSubjectMetadataKey]; got != wf.a.ID {
		t.Errorf("scope body gc.failure_subject = %q, want %s", got, wf.a.ID)
	}
	requireBeadState(t, store, "workflow-finalize", wf.finalize.ID, "closed", beadmeta.OutcomePass)
	requireBeadState(t, store, "workflow root", wf.root.ID, "closed", beadmeta.OutcomeFail)
	requireBeadState(t, store, "worker B", wf.b.ID, "closed", beadmeta.OutcomePass)
}
