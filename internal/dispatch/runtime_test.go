package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestProcessScopeCheckClosesScopeOnSuccess(t *testing.T) {
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
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "implement",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "cleanup",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "teardown",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, cleanup.ID, body.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if !result.Processed || result.Action != "scope-pass" {
		t.Fatalf("scope result = %+v, want processed scope-pass", result)
	}

	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}

	cleanupReady := mustReadyContains(t, store, cleanup.ID)
	if !cleanupReady {
		t.Fatalf("cleanup %s should be ready after body closes", cleanup.ID)
	}
}

func TestProcessScopeCheckSuccessUsesScopedSnapshotQueries(t *testing.T) {
	t.Parallel()

	base := beads.NewMemStore()
	store := &scopeSnapshotQueryGuardStore{Store: base}
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
			"gc.step_ref":     "body",
		},
	})
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "implement",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)

	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if !result.Processed || result.Action != "scope-pass" {
		t.Fatalf("scope result = %+v, want processed scope-pass", result)
	}
	if store.broadRootQueries != 0 {
		t.Fatalf("broad workflow-root queries = %d, want 0", store.broadRootQueries)
	}
	if store.scopedMemberQueries == 0 {
		t.Fatal("expected scoped member query")
	}
	if store.scopeBodyQueries == 0 {
		t.Fatal("expected scope body query")
	}
}

func TestProcessScopeCheckPassWithRemainingOpenAvoidsClosedSnapshot(t *testing.T) {
	t.Parallel()

	base := beads.NewMemStore()
	store := &scopeSnapshotQueryGuardStore{Store: base}
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "body",
		},
	})
	done := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "done",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	stillOpen := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "still open",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for done",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, done.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if !result.Processed || result.Action != "continue" {
		t.Fatalf("scope result = %+v, want processed continue", result)
	}
	if store.closedScopedQueries != 0 {
		t.Fatalf("closed scoped snapshot queries = %d, want 0", store.closedScopedQueries)
	}
	if store.activeScopedQueries == 0 {
		t.Fatal("expected active scoped completion query")
	}
	remaining, err := store.Get(stillOpen.ID)
	if err != nil {
		t.Fatalf("Get remaining member: %v", err)
	}
	if remaining.Status != "open" {
		t.Fatalf("remaining member status = %q, want open", remaining.Status)
	}
}

func TestProcessScopeCheckKeepsControlOpenIfBodyCloseoutFails(t *testing.T) {
	t.Parallel()

	base := beads.NewMemStore()
	store := &failBodyMetadataStore{Store: base}
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
			"gc.step_ref":     "body",
		},
	})
	store.failID = body.ID
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
			"review.verdict":  "done",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for apply fixes",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	_, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if err == nil {
		t.Fatal("ProcessControl(scope-check) succeeded, want injected closeout error")
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open so dispatcher can retry body closeout", controlAfter.Status)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open after failed closeout", bodyAfter.Status)
	}
}

func TestProcessScopeCheckAbortsScopeOnFailure(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	futureControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	futureMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "cleanup",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "teardown",
		},
	})

	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, cleanup.ID, body.ID, "blocks")
	mustDepAdd(t, store, futureControl.ID, futureMember.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check fail): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("scope result = %+v, want processed scope-fail", result)
	}
	if result.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", result.Skipped)
	}

	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}

	for _, beadID := range []string{futureMember.ID, futureControl.ID} {
		member, err := store.Get(beadID)
		if err != nil {
			t.Fatalf("get skipped member %s: %v", beadID, err)
		}
		if member.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, member.Status)
		}
		if got := member.Metadata["gc.outcome"]; got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", beadID, got)
		}
	}
	controlDeps, err := store.DepList(futureControl.ID, "down")
	if err != nil {
		t.Fatalf("dep list future control: %v", err)
	}
	if len(controlDeps) != 1 || controlDeps[0].DependsOnID != futureMember.ID || controlDeps[0].Type != "blocks" {
		t.Fatalf("future control deps = %+v, want preserved block on %s", controlDeps, futureMember.ID)
	}

	cleanupReady := mustReadyContains(t, store, cleanup.ID)
	if !cleanupReady {
		t.Fatalf("cleanup %s should be ready after body fails closed", cleanup.ID)
	}
}

// TestProcessScopeCheckAbortSkipsMemberBlockedOnInFlightControl covers the shape
// that TestProcessScopeCheckAbortsScopeOnFailure deliberately omits: a scope
// member whose only blocker is the scope-check currently aborting the scope.
// skipScopeMembers judges such a member skippable — runtime.go excludes the
// in-flight control from the pending set, and canSkipScopeMemberWithDeps only
// consults that set — so it is closed while its blocker is still open. That is
// legal only on a forced close; it is ga-4ote2, and closing it with an unforced
// update is what real bd refused.
//
// The compiler mints this shape directly. formula/graph.go mints one scope-check
// per scoped step, carrying that step's gc.scope_ref, and rewriteGraphRefs then
// repoints every reference at a scoped step onto that step's scope-check —
// leaving the raw name only for the body the control closes. So a scoped step B
// that needs same-scope sibling A compiles to "B needs A-scope-check", with B
// and the control sharing a scope_ref. When A fails, B is an open member of the
// very scope its own blocker is aborting.
//
// The assertions are deliberately over-specified, because several wrong fixes
// produce a clean abort too: deleting the offending dep, dropping the skipped
// outcome, or closing the control before its members. Each has an assertion
// aimed at it.
func TestProcessScopeCheckAbortSkipsMemberBlockedOnInFlightControl(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	futureMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})

	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, futureMember.ID, control.ID, "blocks")

	// Seeding "failed" as closed goes through the store, so drop what the
	// fixture recorded: the assertion below is about the abort's ordering.
	store.closeOrder = nil

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check fail): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("result = %+v, want Processed with action scope-fail", result)
	}
	if result.Skipped != 1 {
		t.Fatalf("result.Skipped = %d, want 1", result.Skipped)
	}

	memberAfter := mustGetBead(t, store, futureMember.ID)
	if memberAfter.Status != "closed" {
		t.Fatalf("futureMember status = %q, want closed", memberAfter.Status)
	}
	// A fix that closes the member but drops the metadata leaves outcome
	// aggregation unable to tell "skipped" from "passed".
	if got := memberAfter.Metadata["gc.outcome"]; got != "skipped" {
		t.Fatalf("futureMember outcome = %q, want skipped", got)
	}
	// close_reason is not decoration. BdStore.CloseAll forwards it as --reason,
	// and a city running validation.on-close=error rejects a close whose reason
	// is under 20 characters — so a skip that carries no reason trades one
	// failure mode for another in exactly the cities that validate hardest.
	assertScopeSkipCloseReason(t, memberAfter)

	// The member's edge onto the control must survive. Deleting the dep is the
	// other obvious way to make this abort succeed, and it would destroy the
	// audit graph while passing every status assertion above.
	deps, err := store.DepList(futureMember.ID, "down")
	if err != nil {
		t.Fatalf("dep list future member: %v", err)
	}
	if len(deps) != 1 || deps[0].Type != "blocks" || deps[0].DependsOnID != control.ID {
		t.Fatalf("futureMember deps = %+v, want exactly one blocks edge onto %s", deps, control.ID)
	}

	if bodyAfter := mustGetBead(t, store, body.ID); bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if controlAfter := mustGetBead(t, store, control.ID); controlAfter.Status != "closed" {
		t.Fatalf("control status = %q, want closed", controlAfter.Status)
	}

	// Ordering is the ga-a6zy9 invariant: the control closes last, after every
	// bead it is responsible for closing. Closing it first would also produce a
	// clean abort and identical end state, while reintroducing the dispatch race
	// where a member of an aborted scope becomes ready and gets picked up.
	wantOrder := []string{futureMember.ID, body.ID, control.ID}
	if !slices.Equal(store.closeOrder, wantOrder) {
		t.Fatalf("close order = %v, want %v", store.closeOrder, wantOrder)
	}
}

// TestProcessScopeCheckAbortSkipsMemberBlockedOnOutOfScopeOpenBead pins the one
// behavior this fix genuinely widened, so that narrowing it later is a decision
// rather than an accident.
//
// canSkipScopeMemberWithDeps only refuses blockers that are in the pending set,
// so it is blind to an open blocker outside the aborting scope. Under the old
// unforced write that blindness was covered by accident: bd's own guard refused
// the close, and the abort failed loudly. Closing with --force removes that
// backstop, and this member now closes over its out-of-scope blocker.
//
// That is the intended reading of a skip — "this work will never run" is true
// regardless of what else was blocking it, and an aborted scope has no business
// waiting on anything. But it is a real change in what gc will do without
// complaint, so it gets a test that says so out loud. If someone later decides
// the skip judgment should consult blockers outside the pending set, this test
// is the one that should fail and force the conversation.
func TestProcessScopeCheckAbortSkipsMemberBlockedOnOutOfScopeOpenBead(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	member := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	// Deliberately carries no gc.root_bead_id: it is not a member of this scope,
	// so it never enters the pending set and the skip judgment cannot see it.
	outsider := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "unrelated open work",
		Type:  "task",
	})

	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, member.ID, control.ID, "blocks")
	mustDepAdd(t, store, member.ID, outsider.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check fail): %v", err)
	}
	if result.Action != "scope-fail" || result.Skipped != 1 {
		t.Fatalf("result = %+v, want action scope-fail with Skipped 1", result)
	}

	memberAfter := mustGetBead(t, store, member.ID)
	if memberAfter.Status != "closed" {
		t.Fatalf("member status = %q, want closed — the skip closes over the out-of-scope blocker", memberAfter.Status)
	}
	if got := memberAfter.Metadata["gc.outcome"]; got != "skipped" {
		t.Fatalf("member outcome = %q, want skipped", got)
	}
	assertScopeSkipCloseReason(t, memberAfter)

	// The outsider is untouched. Force-closing over a blocker must not be
	// mistaken for permission to touch the blocker itself.
	if outsiderAfter := mustGetBead(t, store, outsider.ID); outsiderAfter.Status != "open" {
		t.Fatalf("outsider status = %q, want open — it is not part of this scope", outsiderAfter.Status)
	}
	deps, err := store.DepList(member.ID, "down")
	if err != nil {
		t.Fatalf("dep list member: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("member deps = %+v, want both blocks edges intact", deps)
	}

	if bodyAfter := mustGetBead(t, store, body.ID); bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
}

// assertScopeSkipCloseReason pins the reason stamped on a member closed by a
// scope abort. The length check is the part that matters and is not redundant
// with the equality check: bd's validation.on-close=error validator rejects a
// close whose reason is under 20 characters, so if someone later shortens
// scopeAbortSkippedCloseReason to a terse token, equality alone would happily
// follow them into a city where every skip fails validation.
func assertScopeSkipCloseReason(t *testing.T, b beads.Bead) {
	t.Helper()
	got := b.Metadata["close_reason"]
	if got != scopeAbortSkippedCloseReason {
		t.Fatalf("%s close_reason = %q, want %q", b.ID, got, scopeAbortSkippedCloseReason)
	}
	if len(got) < 20 {
		t.Fatalf("%s close_reason %q is %d chars; bd's on-close validator rejects reasons under 20", b.ID, got, len(got))
	}
}

// TestReconcileTerminalScopedMemberSkipsSiblingBlockedOnPreservedControl covers
// the second, independent way a scope member is closed while its blocker is
// still open — and the one that decides how ga-4ote2 has to be fixed.
//
// preserveScopeCheckForSubject (runtime.go) deliberately drops the failed
// subject's own scope-check from the pending set WITHOUT closing it, because
// that control is the idempotent recovery path if the abort dies before closing
// the body. A sibling blocked on that preserved control is then judged skippable
// with its blocker still open, exactly as in the in-flight-control case.
//
// The distinction matters: here the blocker must stay open by design, so no
// amount of reordering closes it first. That rules out fixing ga-4ote2 by
// changing when the control closes or which beads land in the pending set, and
// leaves the write itself — a forced close — as the only repair that covers both
// triggers. If someone later "fixes" this by reordering, this test is what fails.
func TestReconcileTerminalScopedMemberSkipsSiblingBlockedOnPreservedControl(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	// The failed member's own scope-check. preserveScopeCheckForSubject keeps
	// this one open and replayable.
	preserved := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	sibling := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})

	mustDepAdd(t, store, preserved.ID, failed.ID, "blocks")
	mustDepAdd(t, store, sibling.ID, preserved.ID, "blocks")

	result, err := reconcileTerminalScopedMember(store, mustGetBead(t, store, failed.ID))
	if err != nil {
		t.Fatalf("reconcileTerminalScopedMember(fail): %v", err)
	}
	if result.Action != "scope-fail" {
		t.Fatalf("action = %q, want scope-fail", result.Action)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", result.Skipped)
	}

	// The whole point: the blocker is still open, and the sibling closed anyway.
	if preservedAfter := mustGetBead(t, store, preserved.ID); preservedAfter.Status != "open" {
		t.Fatalf("preserved control status = %q, want open — it is the replay path", preservedAfter.Status)
	}
	siblingAfter := mustGetBead(t, store, sibling.ID)
	if siblingAfter.Status != "closed" {
		t.Fatalf("sibling status = %q, want closed", siblingAfter.Status)
	}
	if got := siblingAfter.Metadata["gc.outcome"]; got != "skipped" {
		t.Fatalf("sibling outcome = %q, want skipped", got)
	}
	assertScopeSkipCloseReason(t, siblingAfter)

	deps, err := store.DepList(sibling.ID, "down")
	if err != nil {
		t.Fatalf("dep list sibling: %v", err)
	}
	if len(deps) != 1 || deps[0].Type != "blocks" || deps[0].DependsOnID != preserved.ID {
		t.Fatalf("sibling deps = %+v, want exactly one blocks edge onto %s", deps, preserved.ID)
	}

	if bodyAfter := mustGetBead(t, store, body.ID); bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
}

// TestProcessScopeCheckAbortRefusesLegacyBodyBlockedOnControl pins the original
// ga-a6zy9 deadlock: the pre-Slice-4 topology where the scope body carries a
// blocks edge to the very scope-check that closes it. Slice 4 stopped minting
// this shape, so no live store should contain it, but nothing prevents a formula
// or a hand-built fixture from reintroducing it — and under real bd it is
// unrecoverable, because the control can never close the body it is blocked by.
func TestProcessScopeCheckAbortRefusesLegacyBodyBlockedOnControl(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)

	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	_, err := ProcessControl(store, control, ProcessOptions{})
	if err == nil {
		t.Fatalf("ProcessControl(legacy body self-edge) succeeded; expected the ga-a6zy9 deadlock refusal")
	}
	if !strings.Contains(err.Error(), "cannot close blocked issue") {
		t.Fatalf("ProcessControl error = %v, want a blocked-close refusal naming body %s", err, body.ID)
	}
	if bodyAfter := mustGetBead(t, store, body.ID); bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open — bd refuses to close a bead blocked by the closing control", bodyAfter.Status)
	}
}

func TestProcessScopeCheckHardFailureOverridesClosedPassBody(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "body",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
			"gc.outcome":      "pass",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":  "wf-1",
			"gc.scope_ref":     "body",
			"gc.scope_role":    "member",
			"gc.outcome":       "fail",
			"gc.failure_class": "hard",
			"gc.on_fail":       "abort_scope",
			"preflight.report": "failed",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, control.ID, failed.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check hard fail): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("scope result = %+v, want processed scope-fail", result)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}
	if got := bodyAfter.Metadata["preflight.report"]; got != "failed" {
		t.Fatalf("body preflight.report = %q, want failed", got)
	}
}

// scopeCheckAbortScopeFixture builds the canonical scope topology used by the
// gc.on_fail=abort_scope contract tests: a scope body, a closed subject member
// (metadata supplied by the caller), the subject's scope-check control, and a
// downstream member + control wired the way graph compile rewires them
// (downstream blocks on the subject's scope-check, not on the subject).
func scopeCheckAbortScopeFixture(t *testing.T, store beads.Store, subjectMeta map[string]string) (control, futureMember, futureControl, body beads.Bead) {
	t.Helper()
	body = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	meta := map[string]string{
		"gc.root_bead_id": "wf-1",
		"gc.scope_ref":    "body",
		"gc.scope_role":   "member",
	}
	for k, v := range subjectMeta {
		meta[k] = v
	}
	subject := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "preflight",
		Type:     "task",
		Status:   "closed",
		Metadata: meta,
	})
	control = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	futureMember = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.on_fail":      "abort_scope",
		},
	})
	futureControl = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, control.ID, subject.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, futureMember.ID, control.ID, "blocks")
	mustDepAdd(t, store, futureControl.ID, futureMember.ID, "blocks")
	return control, futureMember, futureControl, body
}

// Regression test for gastownhall/gascity#1657: a scoped step that opted into
// gc.on_fail=abort_scope and closes without an affirmative gc.outcome (the
// worker-result contract violation — e.g. `bd close --reason "FAIL: ..."`)
// must abort the scope. Downstream steps must be closed as skipped and must
// NOT appear in ready-work queries, so no pool worker can claim and run them.
func TestProcessScopeCheckAbortScopeNormalizesFailureContract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		subjectMeta map[string]string
	}{
		{
			name: "bare close without outcome",
			subjectMeta: map[string]string{
				"gc.on_fail":   "abort_scope",
				"close_reason": "FAIL: subagent returned non-zero",
			},
		},
		{
			name: "unknown outcome value",
			subjectMeta: map[string]string{
				"gc.on_fail": "abort_scope",
				"gc.outcome": "banana",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			control, futureMember, futureControl, body := scopeCheckAbortScopeFixture(t, store, tc.subjectMeta)

			result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
			if err != nil {
				t.Fatalf("ProcessControl(scope-check): %v", err)
			}
			if !result.Processed || result.Action != "scope-fail" {
				t.Fatalf("result = %+v, want processed scope-fail", result)
			}
			if result.Skipped != 2 {
				t.Fatalf("skipped = %d, want 2", result.Skipped)
			}

			bodyAfter := mustGetBead(t, store, body.ID)
			if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
				t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
			}
			for _, beadID := range []string{futureMember.ID, futureControl.ID} {
				member := mustGetBead(t, store, beadID)
				if member.Status != "closed" || member.Metadata["gc.outcome"] != "skipped" {
					t.Fatalf("%s = status %q outcome %q, want closed/skipped", beadID, member.Status, member.Metadata["gc.outcome"])
				}
			}
			// The issue's repro assertion: with the subject's scope-check
			// closed, the downstream member's dependencies are satisfied —
			// only its skipped close keeps it out of ready-work queries.
			if mustReadyContains(t, store, futureMember.ID) {
				t.Fatalf("downstream member %s is claimable after abort_scope failure", futureMember.ID)
			}
		})
	}
}

func TestProcessRetryControlHardFailClosesScopeBodyWithPendingScopeCheck(t *testing.T) {
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
			"gc.root_bead_id": workflow.ID,
			"gc.scope_role":   "body",
			"gc.step_ref":     "demo.body",
		},
	})
	preflight := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.max_attempts": "1",
			"gc.on_exhausted": "hard_fail",
			"gc.on_fail":      "abort_scope",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_id":      "preflight-tests",
			"gc.step_ref":     "demo.preflight-tests",
		},
	})
	preflightAttempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight attempt",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.attempt":         "1",
			"gc.logical_bead_id": preflight.ID,
			"gc.outcome":         "fail",
			"gc.failure_class":   "hard",
			"gc.failure_reason":  "preflight_failed",
			"gc.root_bead_id":    workflow.ID,
			"gc.step_id":         "preflight-tests",
			"gc.step_ref":        "demo.preflight-tests.attempt.1",
		},
	})
	preflightScopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.control_for":  "preflight-tests",
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
			"gc.step_id":      "preflight-tests",
			"gc.step_ref":     "demo.preflight-tests-scope-check",
		},
	})
	implement := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.max_attempts": "1",
			"gc.on_exhausted": "hard_fail",
			"gc.on_fail":      "abort_scope",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_id":      "implement",
			"gc.step_ref":     "demo.implement",
		},
	})
	implementScopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.control_for":  "implement",
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
			"gc.step_id":      "implement",
			"gc.step_ref":     "demo.implement-scope-check",
		},
	})
	mustDepAdd(t, store, preflight.ID, preflightAttempt.ID, "blocks")
	mustDepAdd(t, store, preflightScopeCheck.ID, preflight.ID, "blocks")
	mustDepAdd(t, store, implement.ID, preflightScopeCheck.ID, "blocks")
	mustDepAdd(t, store, implementScopeCheck.ID, implement.ID, "blocks")
	mustDepAdd(t, store, body.ID, preflightScopeCheck.ID, "blocks")
	mustDepAdd(t, store, body.ID, implementScopeCheck.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, preflight.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry hard fail): %v", err)
	}
	if !result.Processed || result.Action != "hard-fail" {
		t.Fatalf("result = %+v, want processed hard-fail", result)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	controlAfter := mustGetBead(t, store, preflightScopeCheck.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("failed member scope-check status = %q, want open for idempotent body-close recovery", controlAfter.Status)
	}
	downstreamControlAfter := mustGetBead(t, store, implementScopeCheck.ID)
	if downstreamControlAfter.Status != "closed" || downstreamControlAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("downstream scope-check = status %q outcome %q, want closed/skipped", downstreamControlAfter.Status, downstreamControlAfter.Metadata["gc.outcome"])
	}
}

// Scoped members that did NOT opt into gc.on_fail=abort_scope keep the
// legacy lenient contract: a bare close advances the scope, and an
// affirmative pass never aborts even with the opt-in present.
func TestProcessScopeCheckAbortScopeAffirmativeAndLegacyOutcomes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		subjectMeta map[string]string
	}{
		{
			name:        "bare close without on_fail keeps legacy pass behavior",
			subjectMeta: map[string]string{"close_reason": "FAIL: subagent returned non-zero"},
		},
		{
			name: "pass outcome with abort_scope does not abort",
			subjectMeta: map[string]string{
				"gc.on_fail": "abort_scope",
				"gc.outcome": "pass",
			},
		},
		{
			name: "skipped outcome with abort_scope does not abort",
			subjectMeta: map[string]string{
				"gc.on_fail": "abort_scope",
				"gc.outcome": "skipped",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := beads.NewMemStore()
			control, futureMember, _, body := scopeCheckAbortScopeFixture(t, store, tc.subjectMeta)

			result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
			if err != nil {
				t.Fatalf("ProcessControl(scope-check): %v", err)
			}
			if !result.Processed || result.Action != "continue" {
				t.Fatalf("result = %+v, want processed continue", result)
			}
			memberAfter := mustGetBead(t, store, futureMember.ID)
			if memberAfter.Status != "open" {
				t.Fatalf("downstream member status = %q, want open", memberAfter.Status)
			}
			bodyAfter := mustGetBead(t, store, body.ID)
			if bodyAfter.Status != "open" {
				t.Fatalf("body status = %q, want open while members remain", bodyAfter.Status)
			}
			if !mustReadyContains(t, store, futureMember.ID) {
				t.Fatalf("downstream member %s should be claimable after scope continues", futureMember.ID)
			}
		})
	}
}

// Retry-managed attempt subjects are exempt from the fail-closed abort_scope
// contract: a bare-closed nested-retry attempt (gc.logical_bead_id +
// gc.attempt, with the opt-in hardcoded at dispatch) must keep routing
// through retry-eval as a transient contract violation, not abort its
// iteration scope. An explicit gc.outcome=fail still counts as failed. The
// opt-in match itself is whitespace-tolerant so formula-authored variants
// cannot silently keep the legacy lenient contract.
func TestBeadOutcomeFailedRetryAttemptExemptionAndOptInTrim(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{
			name: "bare-closed retry attempt is exempt from fail-closed",
			meta: map[string]string{
				"gc.on_fail":         "abort_scope",
				"gc.logical_bead_id": "ga-logical-1",
				"gc.attempt":         "1",
			},
			want: false,
		},
		{
			name: "explicit fail on retry attempt still counts as failed",
			meta: map[string]string{
				"gc.on_fail":         "abort_scope",
				"gc.logical_bead_id": "ga-logical-1",
				"gc.attempt":         "1",
				"gc.outcome":         "fail",
			},
			want: true,
		},
		{
			name: "whitespace-padded opt-in still fail-closes a bare close",
			meta: map[string]string{
				"gc.on_fail": " abort_scope ",
			},
			want: true,
		},
		{
			name: "canceled outcome on an abort_scope member is a terminal non-failure",
			meta: map[string]string{
				"gc.on_fail": "abort_scope",
				"gc.outcome": "canceled",
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			subject := beads.Bead{
				ID:       "ga-subject-1",
				Status:   "closed",
				Metadata: tc.meta,
			}
			if got := beadOutcomeFailed(subject); got != tc.want {
				t.Fatalf("beadOutcomeFailed = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestLoadDownDepsForScopeSkipFallsBackWhenBatchUnsupported(t *testing.T) {
	t.Parallel()

	store := &scopeSkipBatchStore{MemStore: beads.NewMemStore(), batchUnsupported: true}
	blocker := mustCreateWorkflowBead(t, store, beads.Bead{Title: "blocker", Type: "task"})
	dependent := mustCreateWorkflowBead(t, store, beads.Bead{Title: "dependent", Type: "task"})
	mustDepAdd(t, store, dependent.ID, blocker.ID, "blocks")

	deps, err := loadDownDepsForScopeSkip(store, []string{dependent.ID})
	if err != nil {
		t.Fatalf("loadDownDepsForScopeSkip returned %v; a capability miss must fall back, not abort the scope", err)
	}
	if store.depListCalls == 0 {
		t.Fatalf("per-anchor DepList never ran; the sentinel must route to the fallback path")
	}
	if got := len(deps[dependent.ID]); got != 1 {
		t.Fatalf("fallback returned %d edge(s) for %s, want 1", got, dependent.ID)
	}
}

func TestLoadDownDepsForScopeSkipStillFailsClosedOnRealBatchError(t *testing.T) {
	t.Parallel()

	store := &scopeSkipFailingBatchStore{MemStore: beads.NewMemStore()}
	bead := mustCreateWorkflowBead(t, store, beads.Bead{Title: "anchor", Type: "task"})

	if _, err := loadDownDepsForScopeSkip(store, []string{bead.ID}); err == nil {
		t.Fatal("loadDownDepsForScopeSkip succeeded on a transport error; only the capability sentinel may fall back")
	}
}

type scopeSkipFailingBatchStore struct {
	*beads.MemStore
}

func (s *scopeSkipFailingBatchStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, errors.New("dolt: connection reset")
}

func TestSkipOpenScopeMembersBatchesDependencyChecksAndCloses(t *testing.T) {
	t.Parallel()

	store := &scopeSkipBatchStore{MemStore: beads.NewMemStore()}
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	futureMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	futureControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	independent := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "independent cleanup marker",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})

	mustDepAdd(t, store, futureMember.ID, control.ID, "blocks")
	mustDepAdd(t, store, futureControl.ID, futureMember.ID, "blocks")

	snapshot := scopeSnapshot{
		rootID:      "wf-1",
		scopeRef:    "body",
		allComplete: true,
		members:     []beads.Bead{body, failed, control, futureMember, futureControl, independent},
		body:        body,
	}
	skipped, err := snapshot.skipOpenScopeMembers(store, control.ID)
	if err != nil {
		t.Fatalf("skipOpenScopeMembers: %v", err)
	}
	if skipped != 3 {
		t.Fatalf("skipped = %d, want 3", skipped)
	}
	if store.depListCalls != 0 {
		t.Fatalf("DepList calls = %d, want 0 when batch dep listing is available", store.depListCalls)
	}
	if store.depListBatchCalls != 2 {
		t.Fatalf("DepListBatch calls = %d, want 2 dependency waves", store.depListBatchCalls)
	}
	// The skip path must not dribble out per-id writes; it batches each
	// dependency wave into one call.
	if store.updateCalls != 0 {
		t.Fatalf("Update calls = %d, want 0 — the skip path closes, it does not update", store.updateCalls)
	}
	if store.closeAllCalls != 2 {
		t.Fatalf("CloseAll calls = %d, want 2 dependency waves", store.closeAllCalls)
	}
	if got := []int{len(store.closeAllIDs[0]), len(store.closeAllIDs[1])}; !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("CloseAll wave sizes = %v, want [2 1]", got)
	}
	for _, beadID := range []string{futureMember.ID, futureControl.ID, independent.ID} {
		member := mustGetBead(t, store, beadID)
		if member.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, member.Status)
		}
		if got := member.Metadata["gc.outcome"]; got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", beadID, got)
		}
	}
}

// newPinnedScopeSkipStore returns a scope-skip store that honors explicit bead
// ids. The skip planner walks pending members in sorted id order, so the id
// order is part of these tests' contract, not an accident of id minting.
func newPinnedScopeSkipStore() *scopeSkipBatchStore {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return &scopeSkipBatchStore{MemStore: mem}
}

func mustCreateScopeMember(t *testing.T, store beads.Store, id, title, kind, role string) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		"gc.root_bead_id": "wf-1",
		"gc.scope_ref":    "body",
		"gc.scope_role":   role,
	}
	if kind != "" {
		metadata["gc.kind"] = kind
	}
	return mustCreateWorkflowBead(t, store, beads.Bead{
		ID:       id,
		Title:    title,
		Type:     "task",
		Metadata: metadata,
	})
}

func mustCreateScopeSkipBody(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	return mustCreateWorkflowBead(t, store, beads.Bead{
		ID:    "gcg-267010",
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
}

// TestSkipOpenScopeMembersContinuesAfterPreserveOnlyRound pins the specimen
// behind ga-kzrlh: run gcg-5436965277264861 (beads#6038) quarantined its
// control bead at 2026-08-30T04:26:11Z with "unable to skip remaining scope
// members", while the healthy dup twin closed every one of those members one
// second later. No store call ever failed — the round that PRESERVED the
// subject's own scope-check (deleting the linchpin blocker from pending)
// counted as no progress, so a graph that needed one more round was declared
// deadlocked.
//
// The shape is the common one, not an exotic one: finalize scope-checks are
// minted after the members they block, so their ids sort AFTER their
// dependents and the preserve lands too late in the pass to unblock anything
// in the same round.
func TestSkipOpenScopeMembersContinuesAfterPreserveOnlyRound(t *testing.T) {
	t.Parallel()

	store := newPinnedScopeSkipStore()
	body := mustCreateScopeSkipBody(t, store)
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		ID:     "gcg-267018",
		Title:  "scorecard",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateScopeMember(t, store, "gcg-267019", "Finalize scope for scorecard", "scope-check", "control")
	apply := mustCreateScopeMember(t, store, "gcg-267022", "apply", "", "member")
	applyTwin := mustCreateScopeMember(t, store, "gcg-267024", "apply twin", "", "member")
	scorecardFinalize := mustCreateScopeMember(t, store, "gcg-267031", "Finalize scope for scorecard twin", "scope-check", "control")
	applyFinalize := mustCreateScopeMember(t, store, "gcg-267033", "Finalize scope for apply", "scope-check", "control")
	applyTwinFinalize := mustCreateScopeMember(t, store, "gcg-267034", "Finalize scope for apply twin", "scope-check", "control")

	mustDepAdd(t, store, apply.ID, scorecardFinalize.ID, "blocks")
	mustDepAdd(t, store, applyTwin.ID, scorecardFinalize.ID, "blocks")
	mustDepAdd(t, store, scorecardFinalize.ID, control.ID, "blocks")
	mustDepAdd(t, store, applyFinalize.ID, apply.ID, "blocks")
	mustDepAdd(t, store, applyTwinFinalize.ID, applyTwin.ID, "blocks")

	snapshot := scopeSnapshot{
		rootID:      "wf-1",
		scopeRef:    "body",
		allComplete: true,
		members:     []beads.Bead{body, failed, control, apply, applyTwin, scorecardFinalize, applyFinalize, applyTwinFinalize},
		body:        body,
	}
	skipped, err := snapshot.skipOpenScopeMembers(store, control.ID)
	if err != nil {
		t.Fatalf("skipOpenScopeMembers: %v", err)
	}
	if skipped != 4 {
		t.Fatalf("skipped = %d, want 4", skipped)
	}
	// Three rounds: preserve-only, then the two dependency waves it unblocked.
	if store.depListBatchCalls != 3 {
		t.Fatalf("DepListBatch calls = %d, want 3 (preserve-only round then two skip waves)", store.depListBatchCalls)
	}
	if got := [][]string{store.closeAllIDs[0], store.closeAllIDs[1]}; !slices.Equal(got[0], []string{apply.ID, applyTwin.ID}) || !slices.Equal(got[1], []string{applyFinalize.ID, applyTwinFinalize.ID}) {
		t.Fatalf("CloseAll waves = %v, want [[%s %s] [%s %s]]", got, apply.ID, applyTwin.ID, applyFinalize.ID, applyTwinFinalize.ID)
	}
	for _, beadID := range []string{apply.ID, applyTwin.ID, applyFinalize.ID, applyTwinFinalize.ID} {
		member := mustGetBead(t, store, beadID)
		if member.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, member.Status)
		}
		if got := member.Metadata["gc.outcome"]; got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", beadID, got)
		}
	}
	// The subject's own scope-check stays open: it is the replayable recovery
	// path, which is the whole reason preserving it is progress and not a stall.
	preserved := mustGetBead(t, store, scorecardFinalize.ID)
	if preserved.Status != "open" {
		t.Fatalf("preserved scope-check status = %q, want open", preserved.Status)
	}
}

// TestSkipOpenScopeMembersStillFailsOnGenuineBlocksCycle is the control for the
// fix above: progress-aware rounds must not paper over a real deadlock. Two
// pending members that block each other can never be skipped, so hard-fail and
// quarantine remain the correct disposition.
func TestSkipOpenScopeMembersStillFailsOnGenuineBlocksCycle(t *testing.T) {
	t.Parallel()

	store := newPinnedScopeSkipStore()
	body := mustCreateScopeSkipBody(t, store)
	control := mustCreateScopeMember(t, store, "gcg-267019", "Finalize scope for scorecard", "scope-check", "control")
	left := mustCreateScopeMember(t, store, "gcg-267041", "left", "", "member")
	right := mustCreateScopeMember(t, store, "gcg-267042", "right", "", "member")

	mustDepAdd(t, store, left.ID, right.ID, "blocks")
	mustDepAdd(t, store, right.ID, left.ID, "blocks")

	snapshot := scopeSnapshot{
		rootID:      "wf-1",
		scopeRef:    "body",
		allComplete: true,
		members:     []beads.Bead{body, control, left, right},
		body:        body,
	}
	skipped, err := snapshot.skipOpenScopeMembers(store, control.ID)
	if err == nil {
		t.Fatalf("skipOpenScopeMembers = (%d, nil), want a deadlock error for a genuine blocks cycle", skipped)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	for _, want := range []string{
		"unable to skip remaining scope members",
		left.ID + " (blocked by " + right.ID + ")",
		right.ID + " (blocked by " + left.ID + ")",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q, want it to contain %q", err.Error(), want)
		}
	}
	for _, beadID := range []string{left.ID, right.ID} {
		member := mustGetBead(t, store, beadID)
		if member.Status != "open" {
			t.Fatalf("%s status = %q, want open — a genuine deadlock closes nothing", beadID, member.Status)
		}
	}
}

// TestSkipOpenScopeMembersDeadlockErrorNamesCurrentStuckMembers pins F2. The
// specimen's error listed the round-START ids, which included the member the
// same round had correctly preserved — that stale list is what misdirected the
// initial diagnosis toward store failures. The error must describe the members
// that are actually stuck now, with the blockers that hold them.
func TestSkipOpenScopeMembersDeadlockErrorNamesCurrentStuckMembers(t *testing.T) {
	t.Parallel()

	store := newPinnedScopeSkipStore()
	body := mustCreateScopeSkipBody(t, store)
	control := mustCreateScopeMember(t, store, "gcg-267019", "Finalize scope for scorecard", "scope-check", "control")
	left := mustCreateScopeMember(t, store, "gcg-267041", "left", "", "member")
	right := mustCreateScopeMember(t, store, "gcg-267042", "right", "", "member")
	preserved := mustCreateScopeMember(t, store, "gcg-267090", "Finalize scope for scorecard twin", "scope-check", "control")

	mustDepAdd(t, store, left.ID, right.ID, "blocks")
	mustDepAdd(t, store, right.ID, left.ID, "blocks")
	mustDepAdd(t, store, preserved.ID, control.ID, "blocks")

	snapshot := scopeSnapshot{
		rootID:      "wf-1",
		scopeRef:    "body",
		allComplete: true,
		members:     []beads.Bead{body, control, left, right, preserved},
		body:        body,
	}
	_, err := snapshot.skipOpenScopeMembers(store, control.ID)
	if err == nil {
		t.Fatal("skipOpenScopeMembers = nil error, want a deadlock error for the surviving blocks cycle")
	}
	if strings.Contains(err.Error(), preserved.ID) {
		t.Fatalf("error %q names the preserved scope-check %s — it is not stuck, it was deliberately left open", err.Error(), preserved.ID)
	}
	want := "unable to skip remaining scope members: " + left.ID + " (blocked by " + right.ID + "), " + right.ID + " (blocked by " + left.ID + ")"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q, want it to contain %q", err.Error(), want)
	}
}

// TestSkipOpenScopeMembersConvergesOverManyRounds pins that progress-aware
// rounds converge on a chain deep enough to need more than the two waves the
// original harness exercised: one preserve-only round followed by four
// single-member waves as each skip unblocks the next link.
func TestSkipOpenScopeMembersConvergesOverManyRounds(t *testing.T) {
	t.Parallel()

	store := newPinnedScopeSkipStore()
	body := mustCreateScopeSkipBody(t, store)
	control := mustCreateScopeMember(t, store, "gcg-267019", "Finalize scope for scorecard", "scope-check", "control")
	chain := []beads.Bead{
		mustCreateScopeMember(t, store, "gcg-267041", "link 1", "", "member"),
		mustCreateScopeMember(t, store, "gcg-267042", "link 2", "", "member"),
		mustCreateScopeMember(t, store, "gcg-267043", "link 3", "", "member"),
		mustCreateScopeMember(t, store, "gcg-267044", "link 4", "", "member"),
	}
	// Sorts after every link it blocks, so round one can only preserve.
	preserved := mustCreateScopeMember(t, store, "gcg-267090", "Finalize scope for scorecard twin", "scope-check", "control")

	mustDepAdd(t, store, preserved.ID, control.ID, "blocks")
	mustDepAdd(t, store, chain[0].ID, preserved.ID, "blocks")
	for i := 1; i < len(chain); i++ {
		mustDepAdd(t, store, chain[i].ID, chain[i-1].ID, "blocks")
	}

	members := append([]beads.Bead{body, control, preserved}, chain...)
	snapshot := scopeSnapshot{
		rootID:      "wf-1",
		scopeRef:    "body",
		allComplete: true,
		members:     members,
		body:        body,
	}
	skipped, err := snapshot.skipOpenScopeMembers(store, control.ID)
	if err != nil {
		t.Fatalf("skipOpenScopeMembers: %v", err)
	}
	if skipped != len(chain) {
		t.Fatalf("skipped = %d, want %d", skipped, len(chain))
	}
	if store.depListBatchCalls != len(chain)+1 {
		t.Fatalf("DepListBatch calls = %d, want %d (preserve-only round then one wave per link)", store.depListBatchCalls, len(chain)+1)
	}
	for _, member := range chain {
		got := mustGetBead(t, store, member.ID)
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", member.ID, got.Status)
		}
	}
	if got := mustGetBead(t, store, preserved.ID); got.Status != "open" {
		t.Fatalf("preserved scope-check status = %q, want open", got.Status)
	}
}

func TestProcessScopeCheckTreatsRetryAttemptFailureAsNonTerminalForScope(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review codex",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.review-codex",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review codex attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    "wf-1",
			"gc.scope_ref":       "body",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.review-codex.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "transient_test_failure",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for review codex attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, run1.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check retry attempt): %v", err)
	}
	if !result.Processed || result.Action != "continue" {
		t.Fatalf("result = %+v, want processed continue", result)
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "closed" {
		t.Fatalf("control status = %q, want closed", controlAfter.Status)
	}
	if got := controlAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("control outcome = %q, want pass", got)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open", bodyAfter.Status)
	}
}

// Regression: scope-check must not pass when subject is still open.
// This catches the case where a retry control bead hasn't completed
// (its attempt is missing or still running) but the scope-check passes
// anyway, allowing the workflow to proceed without actual work done.
func TestProcessScopeCheckReturnsPendingWhenSubjectStillOpen(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	mustCreateWorkflowBead(t, store, beads.Bead{
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
			"gc.root_bead_id": "gc-1",
			"gc.step_ref":     "demo.body",
		},
	})
	// Retry control bead — still open, its attempt hasn't run yet.
	retryControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review codex",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": "gc-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.review-codex",
			"gc.max_attempts": "3",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for review codex",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "gc-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, retryControl.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	_, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(scope-check with open subject) = %v, want ErrControlPending", err)
	}

	// Verify nothing was closed.
	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control should stay open, got %q", controlAfter.Status)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "open" {
		t.Fatalf("body should stay open, got %q", bodyAfter.Status)
	}
}

func TestProcessScopeCheckReturnsPendingWhenRetryAttemptSubjectStillOpen(t *testing.T) {
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
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.apply-fixes",
		},
	})
	attempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    workflow.ID,
			"gc.scope_ref":       "body",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.apply-fixes.attempt.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for apply fixes attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, attempt.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, logical.ID, control.ID, "blocks")

	_, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(scope-check with open retry attempt) = %v, want ErrControlPending", err)
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open", controlAfter.Status)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open", bodyAfter.Status)
	}
}

func TestProcessScopeCheckReturnsPendingWhenRetryAttemptSubjectOutsideScopeStillOpen(t *testing.T) {
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
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.apply-fixes",
			"gc.outcome":      "pass",
		},
	})
	attempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":    workflow.ID,
			"gc.scope_ref":       "attempt-body",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.apply-fixes.attempt.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.output_json":     `{"verdict":"unfinished"}`,
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for apply fixes attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, attempt.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, logical.ID, control.ID, "blocks")

	_, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(scope-check with open out-of-scope retry attempt) = %v, want ErrControlPending", err)
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open", controlAfter.Status)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "open" {
		t.Fatalf("body status = %q, want open", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.output_json"]; got != "" {
		t.Fatalf("body gc.output_json = %q, want empty", got)
	}
}

func TestProcessScopeCheckRetryAttemptPassPropagatesMemberMetadata(t *testing.T) {
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
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.apply-fixes",
			"gc.outcome":      "pass",
		},
	})
	attempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":    workflow.ID,
			"gc.scope_ref":       "body",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.apply-fixes.attempt.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.outcome":         "pass",
			"review.verdict":     "done",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for apply fixes attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, attempt.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, logical.ID, control.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check retry attempt): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("action = %q, want scope-pass", result.Action)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["review.verdict"]; got != "done" {
		t.Fatalf("body review.verdict = %q, want done", got)
	}
}

func TestProcessScopeCheckReturnsMalformedWhenScopeBodyMissing(t *testing.T) {
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
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, step.ID, "blocks")

	_, err := ProcessControl(store, control, ProcessOptions{})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl(scope-check missing body) err = %v, want %v", err, ErrControlGraphMalformed)
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open", controlAfter.Status)
	}
}

func TestProcessScopeCheckRetriesTransientMissingScopeBody(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	step := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, mem, control.ID, step.ID, "blocks")

	store := &transientMissingScopeBodyStore{
		MemStore:  mem,
		bodyID:    body.ID,
		rootID:    workflow.ID,
		hideReads: 3,
	}
	var trace bytes.Buffer
	result, err := ProcessControl(store, control, ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("action = %q, want scope-pass", result.Action)
	}
	if store.hiddenReads == 0 {
		t.Fatal("hiddenReads = 0, want transient missing-body path exercised")
	}
	traceText := trace.String()
	if !strings.Contains(traceText, "resolve-body attempt=") ||
		!strings.Contains(traceText, "reason=missing_body") ||
		!strings.Contains(traceText, "result=ok") {
		t.Fatalf("trace = %q, want per-attempt missing-body retry and success", traceText)
	}
	bodyAfter := mustGetBead(t, mem, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}
}

func TestResolveScopeBodyStopsRetryWhenContextCanceled(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	store := &transientMissingScopeBodyStore{
		MemStore:  mem,
		bodyID:    body.ID,
		rootID:    workflow.ID,
		hideReads: scopeBodyResolveAttempts,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := resolveScopeBody(store, workflow.ID, "body", "control", ProcessOptions{Context: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveScopeBody error = %v, want context.Canceled", err)
	}
	if store.hiddenReads == 0 || store.hiddenReads >= store.hideReads {
		t.Fatalf("hiddenReads = %d, want cancellation before exhausting %d hidden reads", store.hiddenReads, store.hideReads)
	}
}

func TestProcessFanoutReturnsMalformedWhenScopeBodyMissing(t *testing.T) {
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
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "fanout",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.fanout_state": "spawned",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "missing-scope",
			"gc.scope_role":   "member",
		},
	})

	_, err := ProcessControl(store, fanout, ProcessOptions{})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl(fanout missing scope body) err = %v, want %v", err, ErrControlGraphMalformed)
	}
}

func TestProcessFanoutReturnsMalformedForInvalidSourceOutputJSON(t *testing.T) {
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
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare review items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  "/tmp/gc.output_json.pretty.json",
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Fan out review items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.prepare-review-items",
			"gc.for_each":     "output.personas",
			"gc.bond":         "expansion-review",
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	_, err := ProcessControl(store, fanout, ProcessOptions{})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl(fanout invalid output JSON) err = %v, want %v", err, ErrControlGraphMalformed)
	}
}

func TestProcessFanoutReturnsMalformedForInvalidBondVars(t *testing.T) {
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
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare review items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"personas":[{"name":"architect"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Fan out review items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.prepare-review-items",
			"gc.for_each":     "output.personas",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    "{not-json",
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	_, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{t.TempDir()}})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl(fanout invalid bond vars) err = %v, want %v", err, ErrControlGraphMalformed)
	}
}

func TestProcessFanoutReturnsMalformedForMissingRequiredBondVar(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[vars.source_convoy_id]
required = true

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
description = "Source {source_convoy_id}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare review items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"personas":[{"name":"architect"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Fan out review items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.prepare-review-items",
			"gc.for_each":     "output.personas",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	_, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl(fanout missing required bond var) err = %v, want %v", err, ErrControlGraphMalformed)
	}
	if !strings.Contains(err.Error(), `variable "source_convoy_id" is required`) {
		t.Fatalf("ProcessControl error = %v, want missing source_convoy_id", err)
	}
}

func TestReconcileTerminalScopedMemberReusesResolvedBodyForFailingScope(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title:  "failed step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
			"review.verdict":  "iterate",
		},
	})
	openStep := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "later step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	store := &scopeBodyVanishAfterFirstResolveStore{
		MemStore: mem,
		bodyID:   body.ID,
		rootID:   workflow.ID,
	}

	result, err := reconcileTerminalScopedMember(store, failed)
	if err != nil {
		t.Fatalf("reconcileTerminalScopedMember(fail): %v", err)
	}
	if result.Action != "scope-fail" {
		t.Fatalf("action = %q, want scope-fail", result.Action)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", result.Skipped)
	}
	bodyAfter := mustGetBead(t, mem, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}
	if got := bodyAfter.Metadata["review.verdict"]; got != "iterate" {
		t.Fatalf("body review.verdict = %q, want iterate", got)
	}
	openAfter := mustGetBead(t, mem, openStep.ID)
	if openAfter.Status != "closed" {
		t.Fatalf("open step status = %q, want closed", openAfter.Status)
	}
	if got := openAfter.Metadata["gc.outcome"]; got != "skipped" {
		t.Fatalf("open step outcome = %q, want skipped", got)
	}
}

// Regression test for gastownhall/gascity#1657, reconcile-path symmetry: a
// scoped member with gc.on_fail=abort_scope reconciled after a bare close
// (no gc.outcome) must abort the scope instead of treating the close as pass.
func TestReconcileTerminalScopedMemberAbortScopeBareCloseAbortsScope(t *testing.T) {
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
		Title:  "failed step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.on_fail":      "abort_scope",
			"close_reason":    "FAIL: subagent returned non-zero",
		},
	})
	openStep := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "later step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})

	result, err := reconcileTerminalScopedMember(store, failed)
	if err != nil {
		t.Fatalf("reconcileTerminalScopedMember(bare close): %v", err)
	}
	if result.Action != "scope-fail" {
		t.Fatalf("action = %q, want scope-fail", result.Action)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", result.Skipped)
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	openAfter := mustGetBead(t, store, openStep.ID)
	if openAfter.Status != "closed" || openAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("open step = status %q outcome %q, want closed/skipped", openAfter.Status, openAfter.Metadata["gc.outcome"])
	}
}

func TestReconcileTerminalScopedMemberReusesResolvedBodyForPassingScope(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	step := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title:  "finished step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":  workflow.ID,
			"gc.scope_ref":     "body",
			"gc.scope_role":    "member",
			"gc.outcome":       "pass",
			"gc.output_json":   `{"verdict":"approved"}`,
			"review.verdict":   "done",
			"operator.summary": "complete",
		},
	})
	store := &scopeBodyVanishAfterFirstResolveStore{
		MemStore: mem,
		bodyID:   body.ID,
		rootID:   workflow.ID,
	}

	result, err := reconcileTerminalScopedMember(store, step)
	if err != nil {
		t.Fatalf("reconcileTerminalScopedMember(pass): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("action = %q, want scope-pass", result.Action)
	}
	bodyAfter := mustGetBead(t, mem, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}
	if got := bodyAfter.Metadata["gc.output_json"]; got != `{"verdict":"approved"}` {
		t.Fatalf("body gc.output_json = %q, want propagated output", got)
	}
	if got := bodyAfter.Metadata["review.verdict"]; got != "done" {
		t.Fatalf("body review.verdict = %q, want done", got)
	}
	if got := bodyAfter.Metadata["operator.summary"]; got != "complete" {
		t.Fatalf("body operator.summary = %q, want complete", got)
	}
}

func TestProcessScopeCheckUsesSingleWorkflowSnapshotAndEmitsTrace(t *testing.T) {
	t.Parallel()

	store := &countingListStore{MemStore: beads.NewMemStore()}
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
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "submit",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for submit",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	var trace bytes.Buffer
	result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("action = %q, want scope-pass", result.Action)
	}
	if store.listCalls != 3 {
		t.Fatalf("List calls = %d, want 3 scoped completion/snapshot queries", store.listCalls)
	}
	if len(store.queries) != 3 {
		t.Fatalf("queries = %d, want 3", len(store.queries))
	}
	for i, query := range store.queries {
		if got := query.Metadata["gc.root_bead_id"]; got != workflow.ID {
			t.Fatalf("query[%d] root metadata = %q, want %q", i, got, workflow.ID)
		}
	}
	if got := store.queries[0].Metadata["gc.kind"]; got != "scope" {
		t.Fatalf("query[0] gc.kind = %q, want scope", got)
	}
	if got := store.queries[0].Metadata["gc.scope_role"]; got != "body" {
		t.Fatalf("query[0] gc.scope_role = %q, want body", got)
	}
	if store.queries[0].IncludeClosed {
		t.Fatal("query[0] should be active-only body lookup")
	}
	if got := store.queries[1].Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("query[1] gc.scope_ref = %q, want body", got)
	}
	if store.queries[1].IncludeClosed {
		t.Fatal("query[1] should be active-only completion check")
	}
	if got := store.queries[2].Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("query[2] gc.scope_ref = %q, want body", got)
	}
	if !store.queries[2].IncludeClosed {
		t.Fatal("query[2] should load closed scope members for final snapshot")
	}
	traceText := trace.String()
	for _, want := range []string{
		"scope-check bead=" + control.ID + " phase=load-snapshot start",
		"scope-check bead=" + control.ID + " phase=load-snapshot ok",
		"scope-check bead=" + control.ID + " snapshot root=" + workflow.ID,
		"scope-check bead=" + control.ID + " phase=propagate-metadata",
		"scope-check bead=" + control.ID + " phase=close-body",
	} {
		if !strings.Contains(traceText, want) {
			t.Fatalf("trace missing %q:\n%s", want, traceText)
		}
	}
}

type strictCloseStore struct {
	*beads.MemStore
	// closeOrder records every bead this store crossed into "closed", in the
	// order it happened, across all three write paths. Ordering between a
	// control and the beads that control closes is the entire ga-a6zy9 failure
	// mode, and assertions on final status alone cannot see it: a fix that
	// closed the control first would leave exactly the same end state.
	closeOrder []string
}

type countingListStore struct {
	*beads.MemStore
	listCalls int
	queries   []beads.ListQuery
}

type scopeSkipBatchStore struct {
	*beads.MemStore
	depListCalls      int
	depListBatchCalls int
	batchUnsupported  bool
	updateCalls       int
	closeAllCalls     int
	closeAllIDs       [][]string
}

type scopeBodyVanishAfterFirstResolveStore struct {
	*beads.MemStore
	mu       sync.Mutex
	bodyID   string
	rootID   string
	resolved bool
}

type transientMissingScopeBodyStore struct {
	*beads.MemStore
	bodyID      string
	rootID      string
	hideReads   int
	hiddenReads int
}

type workflowFinalizeCloseFailStore struct {
	beads.Store
	finalizerID string
}

type closeErrorStore struct {
	beads.Store
	failID string
	err    error
}

type statusUpdateNoopCloseStore struct {
	beads.Store
}

func (s *countingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	s.queries = append(s.queries, query)
	return s.MemStore.List(query)
}

func (s statusUpdateNoopCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		opts.Status = nil
	}
	return s.Store.Update(id, opts)
}

func (s *scopeSkipBatchStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return s.MemStore.DepList(id, direction)
}

func (s *scopeSkipBatchStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.depListBatchCalls++
	if s.batchUnsupported {
		return nil, beads.ErrDepListBatchUnsupported
	}
	return s.MemStore.DepListBatch(ids)
}

func (s *scopeSkipBatchStore) Update(id string, opts beads.UpdateOpts) error {
	s.updateCalls++
	return s.MemStore.Update(id, opts)
}

func (s *scopeSkipBatchStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.closeAllCalls++
	s.closeAllIDs = append(s.closeAllIDs, slices.Clone(ids))
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *scopeBodyVanishAfterFirstResolveStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	if !s.canResolveScopeBody(query) {
		return result, nil
	}

	s.mu.Lock()
	hideBody := s.resolved
	if !s.resolved && containsBeadID(result, s.bodyID) {
		s.resolved = true
	}
	s.mu.Unlock()
	if !hideBody {
		return result, nil
	}
	return filterBeadID(result, s.bodyID), nil
}

func (s *transientMissingScopeBodyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	if !canResolveScopeBodyQuery(query, s.rootID) || !containsBeadID(result, s.bodyID) || s.hiddenReads >= s.hideReads {
		return result, nil
	}
	s.hiddenReads++
	return filterBeadID(result, s.bodyID), nil
}

func (s *scopeBodyVanishAfterFirstResolveStore) canResolveScopeBody(query beads.ListQuery) bool {
	return canResolveScopeBodyQuery(query, s.rootID)
}

func canResolveScopeBodyQuery(query beads.ListQuery, rootID string) bool {
	if query.Metadata["gc.root_bead_id"] != rootID {
		return false
	}
	if query.Metadata["gc.kind"] == "scope" && query.Metadata["gc.scope_role"] == "body" {
		return true
	}
	return len(query.Metadata) == 1
}

func containsBeadID(items []beads.Bead, id string) bool {
	for _, bead := range items {
		if bead.ID == id {
			return true
		}
	}
	return false
}

func filterBeadID(items []beads.Bead, id string) []beads.Bead {
	filtered := items[:0]
	for _, bead := range items {
		if bead.ID != id {
			filtered = append(filtered, bead)
		}
	}
	return filtered
}

func (s *workflowFinalizeCloseFailStore) Close(id string) error {
	if id == s.finalizerID {
		return errors.New("finalizer close failed")
	}
	return s.Store.Close(id)
}

func (s *workflowFinalizeCloseFailStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.finalizerID && opts.Status != nil && *opts.Status == "closed" {
		opts.Status = nil
	}
	return s.Store.Update(id, opts)
}

func (s closeErrorStore) Close(id string) error {
	if id == s.failID {
		return s.err
	}
	return s.Store.Close(id)
}

func (s closeErrorStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID && opts.Status != nil && *opts.Status == "closed" {
		opts.Status = nil
	}
	return s.Store.Update(id, opts)
}

type scopeSnapshotQueryGuardStore struct {
	beads.Store
	broadRootQueries    int
	scopedMemberQueries int
	scopeBodyQueries    int
	activeScopedQueries int
	closedScopedQueries int
}

func (s *scopeSnapshotQueryGuardStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if root := strings.TrimSpace(query.Metadata["gc.root_bead_id"]); root != "" {
		switch {
		case len(query.Metadata) == 1:
			s.broadRootQueries++
			return nil, fmt.Errorf("unexpected broad workflow-root query for %s", root)
		case query.Metadata["gc.scope_ref"] != "":
			s.scopedMemberQueries++
			if query.IncludeClosed {
				s.closedScopedQueries++
			} else {
				s.activeScopedQueries++
			}
		case query.Metadata["gc.kind"] == "scope":
			s.scopeBodyQueries++
		}
	}
	return s.Store.List(query)
}

type failBodyMetadataStore struct {
	beads.Store
	failID string
}

func (s *failBodyMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID && kvs["review.verdict"] != "" {
		return errors.New("injected body metadata failure")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func newStrictCloseStore() *strictCloseStore {
	return &strictCloseStore{MemStore: beads.NewMemStore()}
}

// The rules below mirror bd's close policy as measured against the installed
// binaries, not as read off a source tree: the version banners on these dev
// builds are unreliable (one reports its build ref as whatever branch it was
// compiled on), so behavior is the only trustworthy evidence. Each rule cites
// the observation that pins it; the probe transcripts are recorded on ga-a6zy9.
//
// The guard on the update path is beads #5206, which refuses a status update
// that crosses a bead into done. It is not #4893 — that one guards "bd close",
// a path BdStore always bypasses with --force (bdCloseArgs, bdstore.go).

// blockingDepTypes are the dep types that stop a close. The empty string is in
// the set because MemStore.DepAdd stores the caller's type verbatim, while bd
// normalizes an unset type to "blocks" when the dep is added — "bd dep add"
// with no --type reports "(blocks)" and stores dependency_type "blocks", so bd
// never holds a ""-typed dep to evaluate in the first place. Mirroring bd's
// behavior from a MemStore-backed double therefore means treating "" as
// blocking; MemStore's own ready filter (memstore.go) omits it and would
// under-report.
var blockingDepTypes = map[string]bool{
	"":                   true,
	"blocks":             true,
	"waits-for":          true,
	"conditional-blocks": true,
}

// blockerStopsClose reports whether a blocker in the given status stops a close.
// Pinned blockers are exempt in bd — pinning a blocker lets the blocked bead
// close — and closed ones obviously are. Everything else stops it, in_progress
// included, which is why this is not a status == "open" test.
func blockerStopsClose(status string) bool {
	return status != "closed" && status != "pinned"
}

// openBlockersOf returns the beads whose status stops a close of id.
func (s *strictCloseStore) openBlockersOf(id string) ([]string, error) {
	deps, err := s.DepList(id, "down")
	if err != nil {
		return nil, err
	}
	var openBlockers []string
	for _, dep := range deps {
		if !blockingDepTypes[dep.Type] {
			continue
		}
		blocker, err := s.Get(dep.DependsOnID)
		if err != nil {
			return nil, err
		}
		if blockerStopsClose(blocker.Status) {
			openBlockers = append(openBlockers, blocker.ID)
		}
	}
	return openBlockers, nil
}

// errCloseBlocked mirrors bd's blocked-close refusal. The wording matters:
// "cannot close blocked issue" is the needle the dispatcher's tier table
// matches on to classify the refusal as semantic rather than availability
// (control.go), so a double that phrased it differently would exercise neither
// the refusal nor its classification.
func (s *strictCloseStore) errCloseBlocked(id string, openBlockers []string) error {
	return fmt.Errorf("cannot close blocked issue: %s is blocked by [%s]", id, strings.Join(openBlockers, " "))
}

// errCloseOpenChildren mirrors bd's open-children refusal, which is a separate
// rule from the blocked-dep one and carries its own wording.
func (s *strictCloseStore) errCloseOpenChildren(id string, open int) error {
	return fmt.Errorf("cannot close %s: %d open child issue(s); close children first or use --force to override", id, open)
}

// refuseClose returns the refusal bd would raise for closing id, or nil if bd
// would allow it.
func (s *strictCloseStore) refuseClose(id string) error {
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	// bd gates the policy on crossing INTO done, so restating closed as closed
	// is allowed even with an open child or an open blocker. Idempotent
	// re-processing is this codebase's stated convergence mechanism, so a double
	// that refused a re-close would be stricter than production in exactly the
	// direction that produces false failures.
	if current.Status == "closed" {
		return nil
	}
	// Children first: a bead with both an open child and an open blocker is
	// refused for the child, so evaluating blockers first would report the wrong
	// rule.
	children, err := s.Children(current.ID)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return s.errCloseOpenChildren(id, len(children))
	}
	openBlockers, err := s.openBlockersOf(id)
	if err != nil {
		return err
	}
	if len(openBlockers) > 0 {
		return s.errCloseBlocked(id, openBlockers)
	}
	return nil
}

// Close is stricter than production. BdStore.close and CloseAll both emit
// "bd close --force" (bdstore.go bdCloseArgs), and --force bypasses bd's
// blocked-close guard, so real bd never refuses on this path. The override is
// kept anyway as a belt-and-braces guard for the fallback in
// updateMetadataAndClose: a test that trips it has built a shape whose safety
// depends on --force, which is worth knowing even though production survives it.
// The load-bearing override is Update, below.
func (s *strictCloseStore) Close(id string) error {
	if err := s.refuseClose(id); err != nil {
		return err
	}
	s.recordClose(id)
	return s.MemStore.Close(id)
}

// recordClose appends id to closeOrder when this write is about to cross it
// into closed. Already-closed beads are skipped so that idempotent
// re-processing — which this codebase relies on to converge — does not inflate
// the sequence and make an ordering assertion flaky.
func (s *strictCloseStore) recordClose(id string) {
	current, err := s.Get(id)
	if err != nil || current.Status == "closed" {
		return
	}
	s.closeOrder = append(s.closeOrder, id)
}

// CloseAll deliberately does NOT enforce refuseClose, and no one should add it.
// BdStore.CloseAll closes with "bd close --force" (bdCloseArgs, bdstore.go), and
// --force bypasses bd's close policy by design, so a double that refused here
// would be stricter than production — and would reject the very shape ga-4ote2
// requires, where a scope member is closed while the control aborting its scope
// is still open. The override exists only to record close order.
func (s *strictCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	for _, id := range ids {
		s.recordClose(id)
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// Update enforces the same refusal as Close. The dispatcher does not close beads
// through Close: updateMetadataAndClose sets Status="closed" via Update and only
// falls back to Close when that did not take (control.go). A double that guarded
// Close alone therefore never fired on the path production actually uses, which
// let a swap of any control's close ordering — closing the bead a control closes
// before the control itself — pass the whole suite while deadlocking the fleet.
// That is the ga-a6zy9 failure mode.
func (s *strictCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		if err := s.refuseClose(id); err != nil {
			return err
		}
		s.recordClose(id)
	}
	return s.MemStore.Update(id, opts)
}

// UpdateAll enforces the same refusal as Update. BdStore.UpdateAll emits a plain
// "bd update --status closed" with no --force (bdstore.go), unlike CloseAll, so
// bd refuses it on a blocked bead — measured against both bd binaries installed
// on this host, which reject an unforced "update --status closed" on a blocked
// bead and accept it on an unblocked one.
//
// The scope-skip path used to route here, which is what ga-4ote2 was: it closed
// members while their blocker was still open, on the one batch primitive bd can
// refuse. That path now uses CloseAll. This override is kept because
// BdStore.UpdateAll still exists and any future caller inherits the same
// refusal, and because MemStore has no UpdateAll of its own — so without this
// declaration a store-shaped double would silently not have one at all.
func (s *strictCloseStore) UpdateAll(ids []string, opts beads.UpdateOpts) (int, error) {
	updated := 0
	for _, id := range ids {
		if err := s.Update(id, opts); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// TestStrictCloseStoreMirrorsBdClosePolicy covers the double itself. It exists
// because an unexercised rule in this store is indistinguishable from a missing
// one — the Close-only guard this file used to carry looked correct for as long
// as it went unexercised, and the whole point of the store is to be a rule the
// dispatcher cannot route around. Every case below mirrors a behavior measured
// against a real bd binary; if bd's policy changes, these are the assertions
// that should fail first.
func TestStrictCloseStoreMirrorsBdClosePolicy(t *testing.T) {
	t.Parallel()

	closed := "closed"
	newBead := func(t *testing.T, store *strictCloseStore, title string) beads.Bead {
		t.Helper()
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: title, Type: "task"})
	}
	newChild := func(t *testing.T, store *strictCloseStore, title, parentID string) beads.Bead {
		t.Helper()
		return mustCreateWorkflowBead(t, store, beads.Bead{Title: title, Type: "task", ParentID: parentID})
	}
	setStatus := func(t *testing.T, store *strictCloseStore, id, status string) {
		t.Helper()
		// Go through MemStore so setting up a fixture never trips the policy
		// under test.
		if err := store.MemStore.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("seed status %s=%s: %v", id, status, err)
		}
	}

	tests := []struct {
		name    string
		build   func(t *testing.T, store *strictCloseStore) string
		wantErr string
	}{
		{
			name: "open blocker refuses",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				mustDepAdd(t, store, subject.ID, blocker.ID, "blocks")
				return subject.ID
			},
			wantErr: "cannot close blocked issue",
		},
		{
			name: "in_progress blocker refuses",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				mustDepAdd(t, store, subject.ID, blocker.ID, "blocks")
				setStatus(t, store, blocker.ID, "in_progress")
				return subject.ID
			},
			wantErr: "cannot close blocked issue",
		},
		{
			name: "closed blocker allows",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				mustDepAdd(t, store, subject.ID, blocker.ID, "blocks")
				setStatus(t, store, blocker.ID, "closed")
				return subject.ID
			},
		},
		{
			name: "pinned blocker allows",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				mustDepAdd(t, store, subject.ID, blocker.ID, "blocks")
				setStatus(t, store, blocker.ID, "pinned")
				return subject.ID
			},
		},
		{
			name: "untyped dep refuses because MemStore stores it verbatim",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				mustDepAdd(t, store, subject.ID, blocker.ID, "")
				return subject.ID
			},
			wantErr: "cannot close blocked issue",
		},
		{
			name: "non-blocking dep type allows",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, related := newBead(t, store, "subject"), newBead(t, store, "related")
				mustDepAdd(t, store, subject.ID, related.ID, "related")
				return subject.ID
			},
		},
		{
			name: "open child refuses with its own rule",
			build: func(t *testing.T, store *strictCloseStore) string {
				parent := newBead(t, store, "parent")
				newChild(t, store, "child", parent.ID)
				return parent.ID
			},
			wantErr: "open child issue(s)",
		},
		{
			name: "closed child allows",
			build: func(t *testing.T, store *strictCloseStore) string {
				parent := newBead(t, store, "parent")
				child := newChild(t, store, "child", parent.ID)
				setStatus(t, store, child.ID, "closed")
				return parent.ID
			},
		},
		{
			name: "open child outranks open blocker",
			build: func(t *testing.T, store *strictCloseStore) string {
				parent, blocker := newBead(t, store, "parent"), newBead(t, store, "blocker")
				newChild(t, store, "child", parent.ID)
				mustDepAdd(t, store, parent.ID, blocker.ID, "blocks")
				return parent.ID
			},
			wantErr: "open child issue(s)",
		},
		{
			name: "re-closing an already-closed bead is exempt from both rules",
			build: func(t *testing.T, store *strictCloseStore) string {
				subject, blocker := newBead(t, store, "subject"), newBead(t, store, "blocker")
				newChild(t, store, "child", subject.ID)
				mustDepAdd(t, store, subject.ID, blocker.ID, "blocks")
				setStatus(t, store, subject.ID, "closed")
				return subject.ID
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newStrictCloseStore()
			subjectID := tc.build(t, store)

			err := store.Update(subjectID, beads.UpdateOpts{Status: &closed})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Update(%s, closed) = %v, want success", subjectID, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Update(%s, closed) succeeded, want refusal containing %q", subjectID, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Update(%s, closed) = %v, want refusal containing %q", subjectID, err, tc.wantErr)
			}

			// UpdateAll must agree with Update or the guard is only half-armed.
			// No dispatch path routes a close through it today — the scope-skip
			// path closes via CloseAll — but BdStore.UpdateAll still exists, and
			// the day something reaches for it the batch verb has to refuse the
			// same shapes the single verb refuses.
			store2 := newStrictCloseStore()
			subjectID2 := tc.build(t, store2)
			_, batchErr := store2.UpdateAll([]string{subjectID2}, beads.UpdateOpts{Status: &closed})
			if (batchErr != nil) != (err != nil) {
				t.Fatalf("UpdateAll disagrees with Update: UpdateAll=%v, Update=%v", batchErr, err)
			}
		})
	}
}

func TestProcessWorkflowFinalizeClosesWorkflow(t *testing.T) {
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
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "cleanup",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "fail",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}

	rootAfter, err := store.Get(workflow.ID)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if rootAfter.Status != "closed" {
		t.Fatalf("workflow status = %q, want closed", rootAfter.Status)
	}
	if got := rootAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("workflow outcome = %q, want fail", got)
	}
}

// Regression test for gastownhall/gascity#1657, finalize sibling site: a
// workflow-finalize blocker with gc.on_fail=abort_scope that closed bare (no
// gc.outcome) must fail the workflow instead of finalizing it green.
func TestProcessWorkflowFinalizeAbortScopeBareCloseFailsWorkflow(t *testing.T) {
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
	sink := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "sink step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.on_fail":      "abort_scope",
			"close_reason":    "FAIL: subagent returned non-zero",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, sink.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}
	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("workflow = status %q outcome %q, want closed/fail", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

func TestProcessWorkflowFinalizeFailsOnHardFailedAbortScopeDescendant(t *testing.T) {
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
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "body",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
			"gc.outcome":      "pass",
		},
	})
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":  workflow.ID,
			"gc.scope_ref":     "body",
			"gc.scope_role":    "member",
			"gc.outcome":       "fail",
			"gc.failure_class": "hard",
			"gc.on_fail":       "abort_scope",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "cleanup",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.kind":         "cleanup",
			"gc.outcome":      "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}
	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("workflow = status %q outcome %q, want closed/fail", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

func TestProcessWorkflowFinalizeIgnoresTransientRetryDescendant(t *testing.T) {
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
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "retry attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":          "retry-run",
			"gc.root_bead_id":  workflow.ID,
			"gc.scope_ref":     "body",
			"gc.scope_role":    "member",
			"gc.outcome":       "fail",
			"gc.failure_class": "transient",
			"gc.on_fail":       "abort_scope",
			"gc.attempt":       "1",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "cleanup",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.kind":         "cleanup",
			"gc.outcome":      "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}
	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("workflow = status %q outcome %q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

// TestTerminalAbortScopeFailureSupersededHardAttemptIsNotTerminal proves FIX 2:
// the supersession guard applies to the hard failure class too. A closed
// abort_scope bead that carries gc.attempt + gc.logical_bead_id is one attempt
// among many, so it must not count as a terminal abort even when it closed
// hard; a genuinely terminal, non-superseded hard failure still counts.
func TestTerminalAbortScopeFailureSupersededHardAttemptIsNotTerminal(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"gc.on_fail":       "abort_scope",
		"gc.outcome":       "fail",
		"gc.failure_class": "hard",
	}
	clone := func(extra map[string]string) beads.Bead {
		meta := map[string]string{}
		for k, v := range base {
			meta[k] = v
		}
		for k, v := range extra {
			meta[k] = v
		}
		return beads.Bead{Status: "closed", Metadata: meta}
	}

	// v1 pattern: a cloned retry-run attempt of a logical bead that later passed.
	superseded := clone(map[string]string{
		"gc.kind":            "retry-run",
		"gc.attempt":         "3",
		"gc.logical_bead_id": "logical-1",
	})
	if terminalAbortScopeFailure(superseded) {
		t.Fatalf("superseded (v1) hard abort_scope attempt must not be terminal")
	}

	// v2 pattern: original kind, distinguished by gc.attempt + gc.logical_bead_id.
	supersededV2 := clone(map[string]string{
		"gc.attempt":         "5",
		"gc.logical_bead_id": "logical-2",
	})
	if terminalAbortScopeFailure(supersededV2) {
		t.Fatalf("superseded (v2) hard abort_scope attempt must not be terminal")
	}

	// Non-superseded hard abort_scope failure is still terminal.
	if !terminalAbortScopeFailure(clone(nil)) {
		t.Fatalf("non-superseded hard abort_scope failure must be terminal")
	}
}

// TestProcessWorkflowFinalizeIgnoresSupersededHardRetryDescendant is the FIX 2
// integration guard for #4008: a review loop whose iteration.3 attempt closed
// control_dispatch_error/hard but whose later iterations passed must finalize
// as pass, not fail. The superseded hard attempt must not outvote the passing
// later iterations at finalize.
func TestProcessWorkflowFinalizeIgnoresSupersededHardRetryDescendant(t *testing.T) {
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
	// Logical review step that ultimately PASSED on a later iteration.
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review-codex logical",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": workflow.ID,
			"gc.outcome":      "pass",
		},
	})
	// Superseded attempt.3 that closed control_dispatch_error/hard before the
	// later iterations recovered. Carries gc.attempt + gc.logical_bead_id.
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review-codex attempt 3 (superseded)",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    workflow.ID,
			"gc.scope_ref":       "body",
			"gc.scope_role":      "member",
			"gc.outcome":         "fail",
			"gc.failure_class":   "hard",
			"gc.failure_reason":  "control_dispatch_error",
			"gc.on_fail":         "abort_scope",
			"gc.attempt":         "3",
			"gc.logical_bead_id": logical.ID,
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "cleanup",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.kind":         "cleanup",
			"gc.outcome":      "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}
	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("workflow = status %q outcome %q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

func TestProcessWorkflowFinalizeUsesCloseOperationForTerminalBeads(t *testing.T) {
	t.Parallel()

	store := statusUpdateNoopCloseStore{Store: beads.NewMemStore()}
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}
	for _, beadID := range []string{workflow.ID, finalizer.ID} {
		after := mustGetBead(t, store, beadID)
		if after.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, after.Status)
		}
		if got := after.Metadata["gc.outcome"]; got != "pass" {
			t.Fatalf("%s gc.outcome = %q, want pass", beadID, got)
		}
	}
}

func TestProcessWorkflowFinalizeClosesOpenSpecSidecars(t *testing.T) {
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
	spec := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": workflow.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}

	specAfter := mustGetBead(t, store, spec.ID)
	if specAfter.Status != "closed" {
		t.Fatalf("spec status = %q, want closed", specAfter.Status)
	}
	if got := specAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("spec gc.outcome = %q, want pass", got)
	}
	if got := specAfter.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
	}
}

func TestProcessWorkflowFinalizeClosesRemainingGeneratedMembers(t *testing.T) {
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
	remaining := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "partially materialized step that can no longer execute",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "unused",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}

	remainingAfter := mustGetBead(t, store, remaining.ID)
	if remainingAfter.Status != "closed" {
		t.Fatalf("remaining generated member status = %q, want closed", remainingAfter.Status)
	}
	if got := remainingAfter.Metadata["gc.outcome"]; got != "skipped" {
		t.Fatalf("remaining generated member gc.outcome = %q, want skipped", got)
	}
	finalizerAfter := mustGetBead(t, store, finalizer.ID)
	if got := finalizerAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("finalizer gc.outcome = %q, want pass", got)
	}
}

func TestProcessWorkflowFinalizeTreatsQuarantinedControlAsFailure(t *testing.T) {
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
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "quarantined control",
		Type:   "task",
		Status: "closed",
		Labels: []string{"gc:control-quarantined"},
		Metadata: map[string]string{
			"gc.outcome":             "fail",
			"gc.control_quarantined": "true",
			"gc.failure_reason":      "malformed_control_graph",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, control.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}
	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("workflow = status %q outcome %q, want closed/fail", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

func TestProcessWorkflowFinalizeOrphanedRootClosesFinalizerWithoutError(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow-finalize",
			"gc.root_bead_id":   "missing-root-id",
			"gc.root_store_ref": "rig:gascity",
		},
	})

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(orphan finalize): %v", err)
	}
	if !result.Processed {
		t.Fatalf("result = %+v, want processed", result)
	}
	if result.Action != "workflow-missing_root" {
		t.Fatalf("result.Action = %q, want workflow-missing_root", result.Action)
	}

	finalizerAfter, err := store.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed", finalizerAfter.Status)
	}
	if got := finalizerAfter.Metadata["gc.outcome"]; got != "missing_root" {
		t.Fatalf("finalizer outcome = %q, want missing_root", got)
	}
}

func TestProcessWorkflowFinalizeOrphanedRootReportsFinalizerCloseFailure(t *testing.T) {
	t.Parallel()

	mem := beads.NewMemStore()
	finalizer := mustCreateWorkflowBead(t, mem, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": "missing-root-id",
		},
	})
	store := &workflowFinalizeCloseFailStore{
		Store:       mem,
		finalizerID: finalizer.ID,
	}

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err == nil {
		t.Fatalf("ProcessControl(orphan finalize) error = nil, want finalizer close failure")
	}
	if result.Processed {
		t.Fatalf("result = %+v, want not processed when finalizer close fails", result)
	}
	if !strings.Contains(err.Error(), "closing orphaned finalizer") {
		t.Fatalf("error = %q, want orphaned finalizer context", err)
	}
	if !strings.Contains(err.Error(), "missing-root-id") {
		t.Fatalf("error = %q, want missing root ID context", err)
	}
}

// TestProcessWorkflowFinalizeClosesCrossStoreSourceBead verifies that when a
// graph workflow finalizes successfully, the engine closes any source bead
// chain that crosses store boundaries. This is the PR-review case: the city
// scope holds the human-visible "Adopt PR" source bead, and the rig scope
// holds the launch bead + workflow root that the operator drives. Without
// this propagation, the city source bead stays open forever even after the
// PR is merged and the rig workflow is fully closed - the only way to know
// the request finished is to read metadata, not list status.
//
// Wiring under test:
//   - city store: city source bead (the original "Adopt PR" request)
//   - rig store:  rig launch bead     gc.source_bead_id=<city-source>, gc.source_store_ref=city:test
//     workflow root       gc.source_bead_id=<rig-launch>,  gc.source_store_ref=rig:test
//     cleanup, finalizer
//
// On a successful (outcome=pass) finalize, the engine should close BOTH the
// rig-store workflow root AND the city-store source bead.
func TestProcessWorkflowFinalizeClosesCrossStoreSourceBead(t *testing.T) {
	t.Parallel()

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	citySource := mustCreateWorkflowBead(t, cityStore, beads.Bead{
		Title: "Adopt PR: gastownhall/example#1",
		Type:  "task",
		Metadata: map[string]string{
			"pr_review.pr_number":       "1",
			"pr_review.repo_slug":       "gastownhall/example",
			"pr_review.workflow_status": "running",
		},
	})

	rigLaunch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Adopt PR workflow: gastownhall/example#1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":         citySource.ID,
			"gc.source_store_ref":       "city:test",
			"pr_review.final_pr_url":    "https://github.com/gastownhall/example/pull/1",
			"pr_review.workflow_status": "completed",
			"workflow_id":               "wf-1",
		},
	})

	workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:test",
		},
	})

	cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})

	finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")

	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return cityStore, nil
		case "rig:test":
			return rigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}

	result, err := ProcessControl(rigStore, finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
	})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}

	rigRootAfter, err := rigStore.Get(workflow.ID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if rigRootAfter.Status != "closed" {
		t.Fatalf("workflow root status = %q, want closed", rigRootAfter.Status)
	}
	rigLaunchAfter, err := rigStore.Get(rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch bead: %v", err)
	}
	if rigLaunchAfter.Status != "closed" {
		t.Fatalf("rig launch bead status = %q, want closed", rigLaunchAfter.Status)
	}
	if got := rigLaunchAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Errorf("rig launch bead gc.outcome = %q, want %q", got, "pass")
	}

	citySourceAfter, err := cityStore.Get(citySource.ID)
	if err != nil {
		t.Fatalf("get city source bead: %v", err)
	}
	if citySourceAfter.Status != "closed" {
		t.Fatalf("city source bead status = %q, want closed (cross-store closure on successful finalize)", citySourceAfter.Status)
	}
	if got := citySourceAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Errorf("city source bead gc.outcome = %q, want %q", got, "pass")
	}
	if got := citySourceAfter.Metadata["pr_review.workflow_status"]; got != "completed" {
		t.Errorf("city source bead pr_review.workflow_status = %q, want completed", got)
	}
	if got := citySourceAfter.Metadata["pr_review.final_pr_url"]; got != "https://github.com/gastownhall/example/pull/1" {
		t.Errorf("city source bead final PR URL = %q, want propagated final PR URL", got)
	}
	if got := citySourceAfter.Metadata["workflow_id"]; got != "wf-1" {
		t.Errorf("city source bead workflow_id = %q, want propagated workflow id", got)
	}
}

type sourceChainFinalizeFixture struct {
	cityStore  *beads.MemStore
	rigStore   *beads.MemStore
	citySource beads.Bead
	rigLaunch  beads.Bead
	workflow   beads.Bead
	finalizer  beads.Bead
}

func newSourceChainFinalizeFixture(t *testing.T) sourceChainFinalizeFixture {
	t.Helper()

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	citySource := mustCreateWorkflowBead(t, cityStore, beads.Bead{
		Title: "Adopt PR: gastownhall/example#3",
		Type:  "task",
	})
	rigLaunch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Adopt PR workflow: gastownhall/example#3",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":   citySource.ID,
			"gc.source_store_ref": "city:test",
		},
	})
	workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:test",
		},
	})
	cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")

	return sourceChainFinalizeFixture{
		cityStore:  cityStore,
		rigStore:   rigStore,
		citySource: citySource,
		rigLaunch:  rigLaunch,
		workflow:   workflow,
		finalizer:  finalizer,
	}
}

func (f sourceChainFinalizeFixture) resolver(ref string) (beads.Store, error) {
	switch ref {
	case "city:test":
		return f.cityStore, nil
	case "rig:test":
		return f.rigStore, nil
	default:
		return nil, fmt.Errorf("unknown store ref: %s", ref)
	}
}

func sourceChainFixtureStores(f sourceChainFinalizeFixture) func() ([]SourceWorkflowStore, error) {
	return func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{
			{Store: f.cityStore, StoreRef: "city:test"},
			{Store: f.rigStore, StoreRef: "rig:test"},
		}, nil
	}
}

func TestProcessWorkflowFinalizeRetriesWhenSourceStoreResolverFails(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	resolver := func(ref string) (beads.Store, error) {
		if ref == "city:test" {
			return nil, errors.New("city store unavailable")
		}
		return f.resolver(ref)
	}

	_, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) err = nil, want retryable resolver error")
	}
	if !strings.Contains(err.Error(), "city store unavailable") {
		t.Fatalf("ProcessControl error = %v, want city store resolver failure", err)
	}
	finalizerAfter, err := f.rigStore.Get(f.finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status == "closed" {
		t.Fatal("finalizer status = closed; want open so source-chain resolver failure is retryable")
	}
	citySourceAfter, err := f.cityStore.Get(f.citySource.ID)
	if err != nil {
		t.Fatalf("get city source: %v", err)
	}
	if citySourceAfter.Status == "closed" {
		t.Fatal("city source status = closed; want open after failed source-chain propagation")
	}
}

type getErrorStore struct {
	beads.Store
	failID string
	err    error
}

func (s getErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func TestProcessWorkflowFinalizeRetriesWhenSourceBeadLookupFails(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	lookupErr := errors.New("city source lookup failed")
	resolver := func(ref string) (beads.Store, error) {
		if ref == "city:test" {
			return getErrorStore{Store: f.cityStore, failID: f.citySource.ID, err: lookupErr}, nil
		}
		return f.resolver(ref)
	}

	_, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) err = nil, want retryable parent lookup error")
	}
	if !strings.Contains(err.Error(), lookupErr.Error()) {
		t.Fatalf("ProcessControl error = %v, want parent lookup failure", err)
	}
	finalizerAfter, err := f.rigStore.Get(f.finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status == "closed" {
		t.Fatal("finalizer status = closed; want open so parent lookup failure is retryable")
	}
}

func TestProcessWorkflowFinalizeClosesSourcesUnderProvidedLock(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	var locked []string
	locker := func(storeRef, sourceBeadID string, fn func() error) error {
		locked = append(locked, storeRef+"\x00"+sourceBeadID)
		return fn()
	}

	if _, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef:    f.resolver,
		SourceWorkflowLock: locker,
	}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	want := []string{
		"rig:test\x00" + f.rigLaunch.ID,
		"city:test\x00" + f.citySource.ID,
	}
	if !slices.Equal(locked, want) {
		t.Fatalf("locked source beads = %q, want %q", locked, want)
	}
}

func TestProcessWorkflowFinalizeConvergesUnderConcurrentSharedAncestor(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	citySource := mustCreateWorkflowBead(t, cityStore, beads.Bead{
		Title: "Adopt PR: gastownhall/example#shared",
		Type:  "task",
	})
	newRigWorkflow := func(name string) (beads.Bead, beads.Bead, beads.Bead) {
		t.Helper()
		launch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
			Title: "Adopt PR workflow: " + name,
			Type:  "task",
			Metadata: map[string]string{
				"gc.source_bead_id":   citySource.ID,
				"gc.source_store_ref": "city:test",
			},
		})
		workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
			Title: "mol-adopt-pr-v2 " + name,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
				"gc.source_bead_id":   launch.ID,
				"gc.source_store_ref": "rig:test",
			},
		})
		cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
			Title:  "cleanup " + name,
			Type:   "task",
			Status: "closed",
			Metadata: map[string]string{
				"gc.outcome": "pass",
			},
		})
		finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
			Title: "Finalize workflow " + name,
			Type:  "task",
			Metadata: map[string]string{
				"gc.kind":         "workflow-finalize",
				"gc.root_bead_id": workflow.ID,
			},
		})
		mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
		mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")
		return launch, workflow, finalizer
	}
	launchA, workflowA, finalizerA := newRigWorkflow("a")
	launchB, workflowB, finalizerB := newRigWorkflow("b")

	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return cityStore, nil
		case "rig:test":
			return rigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}
	locker := func(storeRef, sourceBeadID string, fn func() error) error {
		return sourceworkflow.WithLock(context.Background(), cityPath, storeRef, sourceBeadID, fn)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, finalizer := range []beads.Bead{finalizerA, finalizerB} {
		wg.Add(1)
		go func(finalizer beads.Bead) {
			defer wg.Done()
			<-start
			result, err := ProcessControl(rigStore, finalizer, ProcessOptions{
				ResolveStoreRef: resolver,
				SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
					return []SourceWorkflowStore{
						{Store: cityStore, StoreRef: "city:test"},
						{Store: rigStore, StoreRef: "rig:test"},
					}, nil
				},
				SourceWorkflowLock: locker,
			})
			if err != nil {
				errs <- err
				return
			}
			if !result.Processed || result.Action != "workflow-pass" {
				errs <- fmt.Errorf("ProcessControl(%s) = %+v, want workflow-pass", finalizer.ID, result)
				return
			}
			errs <- nil
		}(finalizer)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ProcessControl: %v", err)
		}
	}

	for _, bead := range []beads.Bead{launchA, launchB, workflowA, workflowB, finalizerA, finalizerB} {
		after := mustGetBead(t, rigStore, bead.ID)
		if after.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", bead.ID, after.Status)
		}
		if strings.TrimSpace(after.Metadata[workflowFinalizeErrorMetadataKey]) != "" {
			t.Fatalf("%s has %s=%q, want none", bead.ID, workflowFinalizeErrorMetadataKey, after.Metadata[workflowFinalizeErrorMetadataKey])
		}
	}
	citySourceAfter := mustGetBead(t, cityStore, citySource.ID)
	if citySourceAfter.Status != "closed" {
		t.Fatalf("city source status = %q, want closed after both shared descendants finalize", citySourceAfter.Status)
	}
	if got := citySourceAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("city source gc.outcome = %q, want pass", got)
	}
}

func TestProcessWorkflowFinalizePreservesExistingParentOutcome(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	if err := f.cityStore.SetMetadata(f.citySource.ID, "gc.outcome", "quarantined"); err != nil {
		t.Fatalf("SetMetadata(city outcome): %v", err)
	}

	if _, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: f.resolver,
	}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	citySourceAfter, err := f.cityStore.Get(f.citySource.ID)
	if err != nil {
		t.Fatalf("get city source: %v", err)
	}
	if citySourceAfter.Status != "closed" {
		t.Fatalf("city source status = %q, want closed", citySourceAfter.Status)
	}
	if got := citySourceAfter.Metadata["gc.outcome"]; got != "quarantined" {
		t.Fatalf("city source gc.outcome = %q, want preexisting outcome %q", got, "quarantined")
	}
}

func TestProcessWorkflowFinalizeDoesNotCloseSourcesWhenRootCloseFails(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	rootCloseErr := errors.New("root close failed")
	rigStore := closeErrorStore{Store: f.rigStore, failID: f.workflow.ID, err: rootCloseErr}
	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return f.cityStore, nil
		case "rig:test":
			return f.rigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}

	_, err := ProcessControl(rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef:      resolver,
		SourceWorkflowStores: sourceChainFixtureStores(f),
		SourceWorkflowLock:   func(_ string, _ string, fn func() error) error { return fn() },
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) err = nil, want root close failure")
	}
	if !strings.Contains(err.Error(), rootCloseErr.Error()) {
		t.Fatalf("ProcessControl error = %v, want root close failure", err)
	}
	for _, check := range []struct {
		name  string
		store beads.Store
		id    string
	}{
		{name: "workflow root", store: f.rigStore, id: f.workflow.ID},
		{name: "finalizer", store: f.rigStore, id: f.finalizer.ID},
		{name: "rig launch", store: f.rigStore, id: f.rigLaunch.ID},
		{name: "city source", store: f.cityStore, id: f.citySource.ID},
	} {
		got, getErr := check.store.Get(check.id)
		if getErr != nil {
			t.Fatalf("get %s: %v", check.name, getErr)
		}
		if got.Status == "closed" {
			t.Fatalf("%s status = closed; want open after root close failure", check.name)
		}
	}
	finalizerAfter, err := f.rigStore.Get(f.finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if got := finalizerAfter.Metadata["gc.last_finalize_error"]; !strings.Contains(got, rootCloseErr.Error()) {
		t.Fatalf("finalizer gc.last_finalize_error = %q, want root close failure", got)
	}
}

func TestProcessWorkflowFinalizeRecordsSourceWorkflowStoreScanFailure(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	scanErr := errors.New("skipped source-workflow store: rigs/broken")

	_, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: f.resolver,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return nil, scanErr
		},
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) err = nil, want source-workflow store scan failure")
	}
	if !strings.Contains(err.Error(), scanErr.Error()) {
		t.Fatalf("ProcessControl error = %v, want scan failure", err)
	}
	workflowAfter, err := f.rigStore.Get(f.workflow.ID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if workflowAfter.Status == "closed" {
		t.Fatal("workflow root status = closed; want open when source-workflow scan preflight fails")
	}
	finalizerAfter, err := f.rigStore.Get(f.finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status == "closed" {
		t.Fatal("finalizer status = closed; want open when source-workflow scan preflight fails")
	}
	if got := finalizerAfter.Metadata["gc.last_finalize_error"]; !strings.Contains(got, scanErr.Error()) {
		t.Fatalf("finalizer gc.last_finalize_error = %q, want scan failure", got)
	}
}

func TestRecordWorkflowFinalizeErrorTruncatesAtUTF8Boundary(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
	})
	reason := strings.Repeat("a", maxWorkflowFinalizeErrorMetadata-1) + "é tail"

	err := recordWorkflowFinalizeError(store, finalizer.ID, errors.New(reason))
	if err == nil {
		t.Fatal("recordWorkflowFinalizeError err = nil, want original error returned")
	}
	finalizerAfter := mustGetBead(t, store, finalizer.ID)
	got := finalizerAfter.Metadata[workflowFinalizeErrorMetadataKey]
	if len(got) > maxWorkflowFinalizeErrorMetadata {
		t.Fatalf("recorded reason length = %d, want <= %d", len(got), maxWorkflowFinalizeErrorMetadata)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("recorded reason is invalid UTF-8: %q", got)
	}
}

func TestProcessWorkflowFinalizeLeavesAncestorOpenWhenLiveRootExistsInAnotherStore(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	otherRoot := mustCreateWorkflowBead(t, f.rigStore, beads.Bead{
		Title: "second live workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   f.citySource.ID,
			"gc.source_store_ref": "city:test",
		},
	})
	var trace bytes.Buffer

	if _, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: f.resolver,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: f.cityStore, StoreRef: "city:test"},
				{Store: f.rigStore, StoreRef: "rig:test"},
			}, nil
		},
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}

	rigLaunchAfter, err := f.rigStore.Get(f.rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch: %v", err)
	}
	if rigLaunchAfter.Status != "closed" {
		t.Fatalf("rig launch status = %q, want closed", rigLaunchAfter.Status)
	}
	citySourceAfter, err := f.cityStore.Get(f.citySource.ID)
	if err != nil {
		t.Fatalf("get city source: %v", err)
	}
	if citySourceAfter.Status != "open" {
		t.Fatalf("city source status = %q, want open while %s is live", citySourceAfter.Status, otherRoot.ID)
	}
	traceText := trace.String()
	for _, want := range []string{
		"reason=live_child_workflow",
		"source=" + f.citySource.ID,
		"live_roots=" + otherRoot.ID,
	} {
		if !strings.Contains(traceText, want) {
			t.Fatalf("trace missing %q:\n%s", want, traceText)
		}
	}
}

func TestProcessWorkflowFinalizeLeavesSharedAncestorOpenForIndirectLiveRoot(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	otherRigStore := beads.NewMemStore()
	otherLaunch := mustCreateWorkflowBead(t, otherRigStore, beads.Bead{
		Title: "Second Adopt PR workflow launch",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":   f.citySource.ID,
			"gc.source_store_ref": "city:test",
		},
	})
	otherRoot := mustCreateWorkflowBead(t, otherRigStore, beads.Bead{
		Title: "second live workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   otherLaunch.ID,
			"gc.source_store_ref": "rig:other",
		},
	})
	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return f.cityStore, nil
		case "rig:test":
			return f.rigStore, nil
		case "rig:other":
			return otherRigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}
	var trace bytes.Buffer

	if _, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: f.cityStore, StoreRef: "city:test"},
				{Store: f.rigStore, StoreRef: "rig:test"},
				{Store: otherRigStore, StoreRef: "rig:other"},
			}, nil
		},
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}

	rigLaunchAfter, err := f.rigStore.Get(f.rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch: %v", err)
	}
	if rigLaunchAfter.Status != "closed" {
		t.Fatalf("rig launch status = %q, want closed", rigLaunchAfter.Status)
	}
	citySourceAfter, err := f.cityStore.Get(f.citySource.ID)
	if err != nil {
		t.Fatalf("get city source: %v", err)
	}
	if citySourceAfter.Status != "open" {
		t.Fatalf("city source status = %q, want open while indirect live root %s is running", citySourceAfter.Status, otherRoot.ID)
	}
	traceText := trace.String()
	for _, want := range []string{
		"reason=live_child_workflow",
		"source=" + f.citySource.ID,
		"live_roots=" + otherRoot.ID,
	} {
		if !strings.Contains(traceText, want) {
			t.Fatalf("trace missing %q:\n%s", want, traceText)
		}
	}
}

func TestProcessWorkflowFinalizeClosesIntraStoreSourceBeadWithoutResolver(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Same-store source",
		Type:  "task",
	})
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Same-store workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	if _, err := ProcessControl(store, finalizer, ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	sourceAfter, err := store.Get(source.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if sourceAfter.Status != "closed" {
		t.Fatalf("source status = %q, want closed", sourceAfter.Status)
	}
	if got := sourceAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("source gc.outcome = %q, want pass", got)
	}
}

func TestProcessWorkflowFinalizeStopsOnSourceChainCycle(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Cyclic workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	parent := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Cyclic parent",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id": workflow.ID,
		},
	})
	if err := store.SetMetadata(workflow.ID, "gc.source_bead_id", parent.ID); err != nil {
		t.Fatalf("SetMetadata(workflow source): %v", err)
	}
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	var trace bytes.Buffer
	if _, err := ProcessControl(store, finalizer, ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	finalizerAfter, err := store.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed", finalizerAfter.Status)
	}
	parentAfter, err := store.Get(parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parentAfter.Status != "closed" {
		t.Fatalf("parent status = %q, want closed before cycle stop", parentAfter.Status)
	}
	if got := strings.Count(trace.String(), "reason=cycle"); got != 2 {
		t.Fatalf("cycle trace count = %d, want 2:\n%s", got, trace.String())
	}
}

func TestPreflightSourceBeadChainReportsDepthLimitBeforeMutation(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id": "pending",
		},
	})
	previousID := root.ID
	sourceIDs := make([]string, 0, maxSourceChainHops+2)
	for i := 0; i < 34; i++ {
		source := mustCreateWorkflowBead(t, store, beads.Bead{
			Title: fmt.Sprintf("Source %d", i),
			Type:  "task",
		})
		sourceIDs = append(sourceIDs, source.ID)
		if err := store.SetMetadata(previousID, "gc.source_bead_id", source.ID); err != nil {
			t.Fatalf("SetMetadata(source %d): %v", i, err)
		}
		previousID = source.ID
	}

	var trace bytes.Buffer
	err := preflightSourceBeadChain(store, root.ID, ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	})
	if err == nil {
		t.Fatal("preflightSourceBeadChain err = nil, want depth-limit error")
	}
	if !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("preflightSourceBeadChain error = %v, want depth-limit error", err)
	}
	closed := 0
	for _, sourceID := range sourceIDs {
		source, err := store.Get(sourceID)
		if err != nil {
			t.Fatalf("get source %s: %v", sourceID, err)
		}
		if source.Status == "closed" {
			closed++
		}
	}
	if closed != 0 {
		t.Fatalf("closed source count = %d, want 0 before source-chain mutation", closed)
	}
	if !strings.Contains(trace.String(), "reason=depth_limit") {
		t.Fatalf("trace missing depth_limit:\n%s", trace.String())
	}
}

func TestWithoutSourceWorkflowRootLegacyFallbackExcludesMatchingIDOnly(t *testing.T) {
	t.Parallel()

	roots := []beads.Bead{
		{
			ID: "shared-root-id",
			Metadata: map[string]string{
				sourceworkflow.SourceStoreRefMetadataKey: "rig:other",
			},
		},
		{ID: "other-root"},
	}

	got := withoutSourceWorkflowRoot(roots, "shared-root-id", "")
	if len(got) != 1 || got[0].ID != "other-root" {
		t.Fatalf("withoutSourceWorkflowRoot legacy fallback = %#v, want only other-root retained", got)
	}
}

// TestProcessWorkflowFinalizeLeavesCrossStoreSourceBeadOpenOnFailure pins the
// failure-side contract: a failed workflow should leave the city source bead
// open so a human can see and act on the failure. Closure propagation only
// happens on success.
func TestProcessWorkflowFinalizeLeavesCrossStoreSourceBeadOpenOnFailure(t *testing.T) {
	t.Parallel()

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	citySource := mustCreateWorkflowBead(t, cityStore, beads.Bead{
		Title: "Adopt PR: gastownhall/example#2",
		Type:  "task",
	})

	rigLaunch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Adopt PR workflow: gastownhall/example#2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":   citySource.ID,
			"gc.source_store_ref": "city:test",
		},
	})

	workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:test",
		},
	})

	cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "fail",
		},
	})

	finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")

	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return cityStore, nil
		case "rig:test":
			return rigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}

	result, err := ProcessControl(rigStore, finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
	})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}

	rigRootAfter, err := rigStore.Get(workflow.ID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if rigRootAfter.Status != "closed" {
		t.Fatalf("workflow root status = %q, want closed", rigRootAfter.Status)
	}
	if got := rigRootAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Errorf("workflow root gc.outcome = %q, want %q", got, "fail")
	}

	finalizerAfter, err := rigStore.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get workflow finalizer: %v", err)
	}
	if finalizerAfter.Status != "closed" {
		t.Fatalf("workflow finalizer status = %q, want closed", finalizerAfter.Status)
	}
	if got := finalizerAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Errorf("workflow finalizer gc.outcome = %q, want %q", got, "pass")
	}

	rigLaunchAfter, err := rigStore.Get(rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch bead: %v", err)
	}
	if rigLaunchAfter.Status == "closed" {
		t.Fatalf("rig launch bead status = closed; want still open on failed workflow")
	}

	citySourceAfter, err := cityStore.Get(citySource.ID)
	if err != nil {
		t.Fatalf("get city source bead: %v", err)
	}
	if citySourceAfter.Status == "closed" {
		t.Fatalf("city source bead status = closed; want still open on failed workflow so the human can act on the failure")
	}
}

func TestProcessWorkflowFinalizeKeepsFinalizerOpenWhenSourceResolverFails(t *testing.T) {
	t.Parallel()

	rigStore := beads.NewMemStore()

	rigLaunch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Adopt PR workflow: gastownhall/example#3",
		Type:  "task",
	})

	workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:test",
		},
	})

	cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})

	finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")

	_, err := ProcessControl(rigStore, finalizer, ProcessOptions{
		ResolveStoreRef: func(ref string) (beads.Store, error) {
			return nil, fmt.Errorf("resolver offline for %s", ref)
		},
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) error = nil, want resolver failure")
	}
	if !strings.Contains(err.Error(), "resolver offline for rig:test") {
		t.Fatalf("ProcessControl(workflow-finalize) error = %v, want resolver failure context", err)
	}

	finalizerAfter, err := rigStore.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get workflow finalizer: %v", err)
	}
	if finalizerAfter.Status != "open" {
		t.Fatalf("workflow finalizer status = %q, want open so source-chain closure can retry", finalizerAfter.Status)
	}

	rigLaunchAfter, err := rigStore.Get(rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch bead: %v", err)
	}
	if rigLaunchAfter.Status != "open" {
		t.Fatalf("rig launch bead status = %q, want open after resolver failure", rigLaunchAfter.Status)
	}
}

func TestProcessWorkflowFinalizeKeepsFinalizerOpenWhenSourceStoreReadFails(t *testing.T) {
	t.Parallel()

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	citySource := mustCreateWorkflowBead(t, cityStore, beads.Bead{
		Title: "Adopt PR: gastownhall/example#4",
		Type:  "task",
	})

	rigLaunch := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Adopt PR workflow: gastownhall/example#4",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id":   citySource.ID,
			"gc.source_store_ref": "city:test",
		},
	})

	workflow := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "mol-adopt-pr-v2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   rigLaunch.ID,
			"gc.source_store_ref": "rig:test",
		},
	})

	cleanup := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title:  "Clean up worktree",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "pass",
		},
	})

	finalizer := mustCreateWorkflowBead(t, rigStore, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, rigStore, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, rigStore, workflow.ID, finalizer.ID, "blocks")

	resolver := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:test":
			return getFailStore{
				Store:  cityStore,
				failID: citySource.ID,
				err:    fmt.Errorf("city store read failed"),
			}, nil
		case "rig:test":
			return rigStore, nil
		default:
			return nil, fmt.Errorf("unknown store ref: %s", ref)
		}
	}

	_, err := ProcessControl(rigStore, finalizer, ProcessOptions{
		ResolveStoreRef: resolver,
	})
	if err == nil {
		t.Fatal("ProcessControl(workflow-finalize) error = nil, want source-store read failure")
	}
	if !strings.Contains(err.Error(), "city store read failed") {
		t.Fatalf("ProcessControl(workflow-finalize) error = %v, want source-store read failure context", err)
	}

	finalizerAfter, err := rigStore.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get workflow finalizer: %v", err)
	}
	if finalizerAfter.Status != "open" {
		t.Fatalf("workflow finalizer status = %q, want open so source-chain closure can retry", finalizerAfter.Status)
	}

	workflowAfter, err := rigStore.Get(workflow.ID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if workflowAfter.Status != "open" {
		t.Fatalf("workflow root status = %q, want open because source-chain preflight failed", workflowAfter.Status)
	}

	rigLaunchAfter, err := rigStore.Get(rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch bead: %v", err)
	}
	if rigLaunchAfter.Status != "open" {
		t.Fatalf("rig launch bead status = %q, want open because source-chain preflight failed", rigLaunchAfter.Status)
	}

	citySourceAfter, err := cityStore.Get(citySource.ID)
	if err != nil {
		t.Fatalf("get city source bead: %v", err)
	}
	if citySourceAfter.Status != "open" {
		t.Fatalf("city source bead status = %q, want open after source-store read failure", citySourceAfter.Status)
	}
}

type getFailStore struct {
	beads.Store
	failID string
	err    error
}

func (s getFailStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func TestProcessRalphCheckRetriesThenPasses(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "retry-check.sh", "#!/bin/bash\nset -euo pipefail\nTARGET=\"$GC_CITY_PATH/retry-demo.txt\"\n[ -f \"$TARGET\" ]\ngrep -qx \"pass\" \"$TARGET\"\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)

	if err := os.WriteFile(filepath.Join(cityPath, "retry-demo.txt"), []byte("fail\n"), 0o644); err != nil {
		t.Fatalf("write failing artifact: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result1, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result1.Processed || result1.Action != "retry" {
		t.Fatalf("result1 = %+v, want processed retry", result1)
	}

	run2, check2 := nextSimpleAttempt(t, store, logical.ID)
	if err := os.WriteFile(filepath.Join(cityPath, "retry-demo.txt"), []byte("pass\n"), 0o644); err != nil {
		t.Fatalf("write passing artifact: %v", err)
	}
	if err := store.Close(run2.ID); err != nil {
		t.Fatalf("close run2: %v", err)
	}

	result2, err := ProcessControl(store, check2, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check2): %v", err)
	}
	if !result2.Processed || result2.Action != "pass" {
		t.Fatalf("result2 = %+v, want processed pass", result2)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
}

func TestProcessRalphCheckTransientAppendErrorStaysOpenForRetry(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "retry-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")
	base, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)
	if err := base.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	store := &failOnceCreateStore{
		Store: base,
		err:   errors.New("creating ralph retry bead: invalid connection: i/o timeout"),
	}
	_, err := ProcessControl(store, mustGetBead(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(ralph check append) error = %v, want %v", err, ErrControlPending)
	}

	after := mustGetBead(t, store, check1.ID)
	if after.Status != "open" {
		t.Fatalf("check status = %q, want open", after.Status)
	}
	if after.Metadata["gc.controller_error_class"] != "transient" || after.Metadata["gc.controller_retryable"] != "true" {
		t.Fatalf("controller retry metadata = %v, want transient retryable", after.Metadata)
	}
}

func TestProcessRalphCheckPassClosesCheckBeforeLogicalAndPropagatesOutputJSON(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "pass-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 0\n")

	store := &ralphPassOrderStore{MemStore: beads.NewMemStore()}
	logical, run1, check1 := newSimpleRalphLoopInStore(t, store, "implement", checkPath, 1)
	store.logicalID = logical.ID
	store.checkID = check1.ID

	if err := store.SetMetadata(run1.ID, "gc.output_json", `{"items":[{"name":"claude"}]}`); err != nil {
		t.Fatalf("set run output json: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	checkAfter := mustGetBead(t, store, check1.ID)
	if checkAfter.Status != "closed" || checkAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("check = status %q outcome %q, want closed/pass", checkAfter.Status, checkAfter.Metadata["gc.outcome"])
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if got := logicalAfter.Metadata["gc.output_json"]; got != `{"items":[{"name":"claude"}]}` {
		t.Fatalf("logical gc.output_json = %q, want propagated run output", got)
	}
}

func TestNestedRalphScopePassPropagatesOutputJSONToLogical(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "nested-pass-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 0\n")

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review-loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "1",
			"gc.root_bead_id": workflow.ID,
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review-loop.run.1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                 "scope",
			"gc.scope_role":           "body",
			"gc.scope_name":           "review-loop",
			"gc.step_ref":             "review-loop.run.1",
			"gc.step_id":              "review-loop",
			"gc.ralph_step_id":        "review-loop",
			"gc.attempt":              "1",
			"gc.root_bead_id":         workflow.ID,
			"gc.logical_bead_id":      logical.ID,
			"gc.output_json_required": "true",
		},
	})
	member := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review-loop.run.1.synthesize",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.scope_ref":            "review-loop.run.1",
			"gc.scope_role":           "member",
			"gc.output_json_required": "true",
			"gc.output_json":          `{"items":[{"name":"claude"}]}`,
			"gc.outcome":              "pass",
			"gc.step_id":              "review-loop",
			"gc.ralph_step_id":        "review-loop",
			"gc.attempt":              "1",
			"gc.root_bead_id":         workflow.ID,
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for review-loop.run.1.synthesize",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "scope-check",
			"gc.scope_ref":     "review-loop.run.1",
			"gc.scope_role":    "control",
			"gc.control_for":   "review-loop.run.1.synthesize",
			"gc.step_id":       "review-loop",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.on_fail":       "abort_scope",
			"gc.root_bead_id":  workflow.ID,
		},
	})
	check1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.step_ref":        "review-loop.check.1",
			"gc.check_mode":      "exec",
			"gc.check_path":      checkPath,
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})

	mustDepAdd(t, store, control.ID, member.ID, "blocks")
	mustDepAdd(t, store, run1.ID, control.ID, "blocks")
	mustDepAdd(t, store, check1.ID, run1.ID, "blocks")
	mustDepAdd(t, store, logical.ID, check1.ID, "blocks")

	scopeResult, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if !scopeResult.Processed || scopeResult.Action != "scope-pass" {
		t.Fatalf("scopeResult = %+v, want processed scope-pass", scopeResult)
	}

	runAfter := mustGetBead(t, store, run1.ID)
	if runAfter.Status != "closed" || runAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("run1 = status %q outcome %q, want closed/pass", runAfter.Status, runAfter.Metadata["gc.outcome"])
	}
	if got := runAfter.Metadata["gc.output_json"]; got != `{"items":[{"name":"claude"}]}` {
		t.Fatalf("run1 gc.output_json = %q, want propagated body output", got)
	}

	checkResult, err := ProcessControl(store, check1, ProcessOptions{
		CityPath: cityPath,
		// Surface ralph check-result trace fields in test logs so a
		// recurrence of the flake here is diagnosable without re-instrumenting.
		Tracef: func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !checkResult.Processed || checkResult.Action != "pass" {
		t.Fatalf("checkResult = %+v, want processed pass", checkResult)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if got := logicalAfter.Metadata["gc.output_json"]; got != `{"items":[{"name":"claude"}]}` {
		t.Fatalf("logical gc.output_json = %q, want propagated nested output", got)
	}
}

func TestProcessRalphCheckResumesExistingRetryAttemptWithoutDuplicates(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "always-fail-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	if _, err := appendRalphRetry(store, logical.ID, run1, check1, 2, ProcessOptions{CityPath: cityPath}); err != nil {
		t.Fatalf("appendRalphRetry: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	attempt2 := 0
	for _, bead := range all {
		if bead.Metadata["gc.attempt"] == "2" {
			attempt2++
		}
	}
	if attempt2 != 2 {
		t.Fatalf("attempt 2 bead count = %d, want 2", attempt2)
	}

	check1After := mustGetBead(t, store, check1.ID)
	if check1After.Status != "closed" || check1After.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("check1 = status %q outcome %q, want closed/fail", check1After.Status, check1After.Metadata["gc.outcome"])
	}
	if got := check1After.Metadata["gc.retry_state"]; got != "spawned" {
		t.Fatalf("check1 retry_state = %q, want spawned", got)
	}
}

func TestAppendRalphRetryDefersAssigneesUntilDepsAreWired(t *testing.T) {
	t.Parallel()

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", "unused", 3)
	inspect := &assigneeVisibilityOnCreateStore{MemStore: store}
	runner := "runner"
	checker := "checker"
	if err := inspect.Update(run1.ID, beads.UpdateOpts{Assignee: &runner}); err != nil {
		t.Fatalf("assign run1: %v", err)
	}
	if err := inspect.Update(check1.ID, beads.UpdateOpts{Assignee: &checker}); err != nil {
		t.Fatalf("assign check1: %v", err)
	}
	run1 = mustGetBead(t, inspect, run1.ID)
	check1 = mustGetBead(t, inspect, check1.ID)

	mapping, err := appendRalphRetry(inspect, logical.ID, run1, check1, 2, ProcessOptions{})
	if err != nil {
		t.Fatalf("appendRalphRetry: %v", err)
	}
	if len(inspect.visibleOnCreate) > 0 {
		t.Fatalf("retry beads became visible in assignee queues during creation before wiring completed: %v", inspect.visibleOnCreate)
	}

	run2 := mustGetBead(t, inspect, mapping[run1.ID])
	check2 := mustGetBead(t, inspect, mapping[check1.ID])
	if run2.Assignee != runner {
		t.Fatalf("run2 assignee = %q, want %q", run2.Assignee, runner)
	}
	if check2.Assignee != checker {
		t.Fatalf("check2 assignee = %q, want %q", check2.Assignee, checker)
	}
}

func TestAppendRalphRetryGraphEdgesSkipsParentChildDeps(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	parent := mustCreateWorkflowBead(t, store, beads.Bead{Title: "parent", Type: "task"})
	child := mustCreateWorkflowBead(t, store, beads.Bead{Title: "child", Type: "task", ParentID: parent.ID})
	blocker := mustCreateWorkflowBead(t, store, beads.Bead{Title: "blocker", Type: "task"})
	mustDepAdd(t, store, child.ID, parent.ID, "parent-child")
	mustDepAdd(t, store, child.ID, blocker.ID, "blocks")

	plan := &beads.GraphApplyPlan{}
	if err := appendRalphRetryGraphEdges(plan, store, child.ID, map[string]bool{
		parent.ID:  true,
		blocker.ID: true,
	}); err != nil {
		t.Fatalf("appendRalphRetryGraphEdges: %v", err)
	}

	if len(plan.Edges) != 1 {
		t.Fatalf("edges = %+v, want only the blocking edge", plan.Edges)
	}
	edge := plan.Edges[0]
	if edge.Type != "blocks" || edge.FromKey != child.ID || edge.ToKey != blocker.ID {
		t.Fatalf("edge = %+v, want blocks edge to blocker", edge)
	}
}

func TestAppendRalphRetryClearsPoolAssignee(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
name = "test-city"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[agent]]
name = "polecat"

[agent.pool]
min = 0
max = -1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", "unused", 3)
	poolSlot := "polecat-2"
	if err := store.Update(run1.ID, beads.UpdateOpts{
		Assignee: &poolSlot,
		Metadata: map[string]string{
			"gc.continuation_group": "main",
			"gc.routed_to":          "polecat",
			"gc.session_affinity":   "require",
		},
	}); err != nil {
		t.Fatalf("assign pooled run1: %v", err)
	}
	run1 = mustGetBead(t, store, run1.ID)
	check1 = mustGetBead(t, store, check1.ID)

	mapping, err := appendRalphRetry(store, logical.ID, run1, check1, 2, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("appendRalphRetry: %v", err)
	}

	run2 := mustGetBead(t, store, mapping[run1.ID])
	if run2.Assignee != "" {
		t.Fatalf("run2 assignee = %q, want empty for pooled retry task", run2.Assignee)
	}
	if run2.Metadata["gc.session_affinity"] != "" {
		t.Fatalf("run2 gc.session_affinity = %q, want cleared with pooled retry assignee", run2.Metadata["gc.session_affinity"])
	}
	if run2.Metadata["gc.continuation_group"] != "" {
		t.Fatalf("run2 gc.continuation_group = %q, want cleared with pooled retry assignee", run2.Metadata["gc.continuation_group"])
	}
}

func TestAppendRalphRetryRemapsNestedRetryLogicalRefs(t *testing.T) {
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
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "2",
			"gc.root_bead_id": workflow.ID,
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "attempt 1 body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "scope",
			"gc.scope_role":      "body",
			"gc.scope_name":      "review-loop",
			"gc.step_ref":        "demo.review-loop.run.1",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	check1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.max_attempts":    "2",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review-loop.check.1",
		},
	})
	nestedLogical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.scope_ref":    "demo.review-loop.run.1",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.review-loop.run.1.review-claude",
			"gc.max_attempts": "3",
			"gc.root_bead_id": workflow.ID,
		},
	})
	nestedRun := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review claude run 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.scope_ref":       "demo.review-loop.run.1",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.review-loop.run.1.review-claude.run.1",
			"gc.attempt":         "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": nestedLogical.ID,
		},
	})
	nestedEval := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review claude eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.scope_ref":       "demo.review-loop.run.1",
			"gc.scope_role":      "member",
			"gc.step_ref":        "demo.review-loop.run.1.review-claude.eval.1",
			"gc.control_for":     "demo.review-loop.run.1.review-claude.eval.1",
			"gc.attempt":         "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": nestedLogical.ID,
		},
	})

	mustDepAdd(t, store, nestedLogical.ID, nestedEval.ID, "blocks")
	mustDepAdd(t, store, nestedEval.ID, nestedRun.ID, "blocks")
	mustDepAdd(t, store, check1.ID, run1.ID, "blocks")
	mustDepAdd(t, store, logical.ID, check1.ID, "blocks")

	mapping, err := appendRalphRetry(store, logical.ID, run1, check1, 2, ProcessOptions{})
	if err != nil {
		t.Fatalf("appendRalphRetry: %v", err)
	}

	nestedLogical2 := mustGetBead(t, store, mapping[nestedLogical.ID])
	nestedRun2 := mustGetBead(t, store, mapping[nestedRun.ID])
	nestedEval2 := mustGetBead(t, store, mapping[nestedEval.ID])

	if got := nestedRun2.Metadata["gc.logical_bead_id"]; got != nestedLogical2.ID {
		t.Fatalf("nested run gc.logical_bead_id = %q, want %q", got, nestedLogical2.ID)
	}
	if got := nestedEval2.Metadata["gc.logical_bead_id"]; got != nestedLogical2.ID {
		t.Fatalf("nested eval gc.logical_bead_id = %q, want %q", got, nestedLogical2.ID)
	}
	if got := nestedEval2.Metadata["gc.control_for"]; got != "demo.review-loop.run.2.review-claude.eval.1" {
		t.Fatalf("nested eval gc.control_for = %q, want demo.review-loop.run.2.review-claude.eval.1", got)
	}
}

func TestBuildRalphRetryGraphNodeRemapsNestedRetryLogicalRef(t *testing.T) {
	t.Parallel()

	node := buildRalphRetryGraphNode(beads.Bead{
		ID:    "old-eval",
		Title: "eval",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.attempt":         "1",
			"gc.scope_ref":       "demo.review-loop.run.1",
			"gc.step_ref":        "demo.review-loop.run.1.review-claude.eval.1",
			"gc.control_for":     "demo.review-loop.run.1.review-claude.eval.1",
			"gc.logical_bead_id": "old-logical",
		},
	}, "top-logical", "demo.review-loop.run.1", "demo.review-loop.run.2", 1, 2, map[string]bool{
		"old-eval":    true,
		"old-logical": true,
	}, (*config.City)(nil))

	if got := node.Metadata["gc.step_ref"]; got != "demo.review-loop.run.2.review-claude.eval.1" {
		t.Fatalf("node gc.step_ref = %q, want demo.review-loop.run.2.review-claude.eval.1", got)
	}
	if got := node.Metadata["gc.control_for"]; got != "demo.review-loop.run.2.review-claude.eval.1" {
		t.Fatalf("node gc.control_for = %q, want demo.review-loop.run.2.review-claude.eval.1", got)
	}
	if got := node.MetadataRefs["gc.logical_bead_id"]; got != "old-logical" {
		t.Fatalf("node gc.logical_bead_id ref = %q, want old-logical", got)
	}
	if got := node.Metadata["gc.logical_bead_id"]; got != "" {
		t.Fatalf("node gc.logical_bead_id = %q, want empty metadata when ref remap is used", got)
	}
}

func TestBuildRalphRetryGraphNodeRemapsNestedScopeCheckControlForFromStepRef(t *testing.T) {
	t.Parallel()

	node := buildRalphRetryGraphNode(beads.Bead{
		ID:    "old-scope-check",
		Title: "scope-check",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "scope-check",
			"gc.attempt":     "3",
			"gc.scope_ref":   "mol-adopt-pr-v2.review-loop.run.3",
			"gc.step_ref":    "mol-adopt-pr-v2.review-loop.run.3.review-pipeline.review-codex.run.1-scope-check",
			"gc.control_for": "review-loop.run.3.review-pipeline.review-codex.run.1",
		},
	}, "top-logical", "mol-adopt-pr-v2.review-loop.run.3", "mol-adopt-pr-v2.review-loop.run.4", 3, 4, nil, (*config.City)(nil))

	if got := node.Metadata["gc.step_ref"]; got != "mol-adopt-pr-v2.review-loop.run.4.review-pipeline.review-codex.run.1-scope-check" {
		t.Fatalf("node gc.step_ref = %q, want rewritten outer Ralph scope with nested retry attempt unchanged", got)
	}
	if got := node.Metadata["gc.control_for"]; got != "mol-adopt-pr-v2.review-loop.run.4.review-pipeline.review-codex.run.1" {
		t.Fatalf("node gc.control_for = %q, want scope-check control_for derived from rewritten step_ref", got)
	}
}

func TestLogicalStepRefForAttemptBeadPrefersNestedAttemptOverOuterRalphScope(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.attempt":  "3",
			"gc.step_ref": "mol-adopt-pr-v2.review-loop.run.3.review-pipeline.review-codex.eval.1-scope-check",
		},
	}

	if got := logicalStepRefForAttemptBead(bead); got != "mol-adopt-pr-v2.review-loop.run.3.review-pipeline.review-codex" {
		t.Fatalf("logicalStepRefForAttemptBead(scope-check) = %q, want nested retry logical step", got)
	}
}

func TestLogicalStepRefForAttemptBeadMapsFlatScopeCheckToControlStep(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.step_ref": "demo.review-scope-check",
		},
	}

	if got := logicalStepRefForAttemptBead(bead); got != "demo.review" {
		t.Fatalf("logicalStepRefForAttemptBead(flat scope-check) = %q, want demo.review", got)
	}
}

func TestResolveLogicalBeadIDPrefersExactScopeCheckTargetStep(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		ID:    "parent",
		Title: "loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.loop",
		},
	})
	child := mustCreateWorkflowBead(t, store, beads.Bead{
		ID:    "child",
		Title: "child",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.loop.run.1.child",
		},
	})
	scopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize child scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.attempt":      "1",
			"gc.step_ref":     "demo.loop.run.1.child-scope-check",
		},
	})

	if got := resolveLogicalBeadID(store, scopeCheck); got != child.ID {
		t.Fatalf("resolveLogicalBeadID(child scope-check) = %q, want %q", got, child.ID)
	}
}

func TestProcessRalphCheckExhaustsRetries(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "always-fail-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)

	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	result1, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result1.Processed || result1.Action != "retry" {
		t.Fatalf("result1 = %+v, want processed retry", result1)
	}

	run2, check2 := nextSimpleAttempt(t, store, logical.ID)
	if err := store.Close(run2.ID); err != nil {
		t.Fatalf("close run2: %v", err)
	}
	result2, err := ProcessControl(store, check2, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check2): %v", err)
	}
	if !result2.Processed || result2.Action != "fail" {
		t.Fatalf("result2 = %+v, want processed fail", result2)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("logical = status %q outcome %q, want closed/fail", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if got := logicalAfter.Metadata["gc.failed_attempt"]; got != "2" {
		t.Fatalf("logical failed attempt = %q, want 2", got)
	}
}

// TestProcessRalphCheckTraceCapturesGateResultFieldsOnFailure locks in that
// the ralph check-result trace line surfaces every field a future
// investigator needs to diagnose a non-pass check without rerunning the
// scenario: outcome, numeric exit code, duration, truncation flag, and
// both captured streams. Without these in the trace, a failing check
// surfaces to callers as only {Processed:true Action:fail} — see the test
// scenario below where stderr is the only path to the cause.
func TestProcessRalphCheckTraceCapturesGateResultFieldsOnFailure(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	// Script writes a known marker to both stdout and stderr, then exits
	// non-zero, so we can assert each stream surfaces in the trace.
	const stderrMarker = "sentinel-stderr-marker"
	const stdoutMarker = "sentinel-stdout-marker"
	checkPath := writeCheckScript(t, cityPath, "trace-fail-check.sh",
		"#!/bin/bash\nset -euo pipefail\necho \""+stdoutMarker+"\"\necho \""+stderrMarker+"\" 1>&2\nexit 1\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 1)
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	var trace bytes.Buffer
	result, err := ProcessControl(store, check1, ProcessOptions{
		CityPath: cityPath,
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "fail" {
		t.Fatalf("result = %+v, want processed fail", result)
	}

	traceText := trace.String()
	for _, want := range []string{
		"ralph check-result",
		"outcome=fail",
		"exit=1", // numeric, not a pointer address — see formatGateExitCode
		stderrMarker,
		stdoutMarker,
		"dur=",
		"truncated=",
	} {
		if !strings.Contains(traceText, want) {
			t.Errorf("trace missing %q\nfull trace:\n%s", want, traceText)
		}
	}
}

// TestProcessRalphCheckTraceClipsLargeOutputs guards traceCheckOutputCap: a
// runaway script that writes more than the cap must not produce an
// unbounded trace line. The clip marker must appear, and the captured
// length must be near the cap (not the script's full output).
func TestProcessRalphCheckTraceClipsLargeOutputs(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	// Emit ~4 KiB of stderr — well above traceCheckOutputCap (512).
	checkPath := writeCheckScript(t, cityPath, "trace-loud-check.sh",
		"#!/bin/bash\nset -euo pipefail\nhead -c 4096 /dev/zero | tr '\\0' 'x' 1>&2\nexit 1\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 1)
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	var trace bytes.Buffer
	if _, err := ProcessControl(store, check1, ProcessOptions{
		CityPath: cityPath,
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
		},
	}); err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	traceText := trace.String()
	if !strings.Contains(traceText, "...[clipped]") {
		t.Errorf("trace missing clip marker for oversize stderr\nfull trace:\n%s", traceText)
	}
	// Stderr was 4096 'x'. The trace line should be far smaller than that
	// because of the clip; guard with a generous upper bound that still
	// catches an unbounded regression.
	if len(traceText) > 2048 {
		t.Errorf("trace line length = %d, want <= 2048 (clip cap is %d)", len(traceText), traceCheckOutputCap)
	}
}

func TestProcessRalphCheckRetriesNestedAttemptScope(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "nested-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 0\n")

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "2",
			"gc.root_bead_id": workflow.ID,
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "attempt 1 body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "scope",
			"gc.scope_role":      "body",
			"gc.scope_name":      "review-loop",
			"gc.step_ref":        "mol-adopt-pr-v2.review-loop.run.1",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	member1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.scope_ref":     "review-loop.run.1",
			"gc.scope_role":    "member",
			"gc.on_fail":       "abort_scope",
			"gc.step_id":       "review-loop",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.root_bead_id":  workflow.ID,
		},
	})
	control1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "scope-check",
			"gc.scope_ref":     "review-loop.run.1",
			"gc.scope_role":    "control",
			"gc.control_for":   "review-loop.run.1.review-claude",
			"gc.step_id":       "review-loop",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.on_fail":       "abort_scope",
			"gc.root_bead_id":  workflow.ID,
		},
	})
	check1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.check_mode":      "exec",
			"gc.check_path":      checkPath,
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    "2",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})

	mustDepAdd(t, store, control1.ID, member1.ID, "blocks")
	mustDepAdd(t, store, run1.ID, control1.ID, "blocks")
	mustDepAdd(t, store, check1.ID, run1.ID, "blocks")
	mustDepAdd(t, store, logical.ID, check1.ID, "blocks")

	if err := store.SetMetadataBatch(member1.ID, map[string]string{"gc.outcome": "fail"}); err != nil {
		t.Fatalf("mark member fail: %v", err)
	}
	if err := store.Close(member1.ID); err != nil {
		t.Fatalf("close member1: %v", err)
	}
	if _, err := ProcessControl(store, control1, ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}

	run1After := mustGetBead(t, store, run1.ID)
	if run1After.Status != "closed" || run1After.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("run1/body = status %q outcome %q, want closed/fail", run1After.Status, run1After.Metadata["gc.outcome"])
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	logicalDeps, err := store.DepList(logical.ID, "down")
	if err != nil {
		t.Fatalf("logical deps: %v", err)
	}
	if len(logicalDeps) != 1 {
		t.Fatalf("logical deps = %+v, want exactly one current blocker", logicalDeps)
	}
	check2 := mustGetBead(t, store, logicalDeps[0].DependsOnID)
	run2ID, err := resolveBlockingSubjectID(store, check2.ID)
	if err != nil {
		t.Fatalf("resolve run2 subject: %v", err)
	}
	run2 := mustGetBead(t, store, run2ID)
	if run2.Metadata["gc.kind"] != "scope" || run2.Metadata["gc.attempt"] != "2" {
		t.Fatalf("run2 metadata = %+v, want scope attempt 2", run2.Metadata)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var clonedMember beads.Bead
	var clonedControl beads.Bead
	for _, bead := range all {
		if bead.ID == member1.ID || bead.ID == control1.ID {
			continue
		}
		if bead.Metadata["gc.attempt"] != "2" {
			continue
		}
		if bead.Metadata["gc.scope_ref"] != run2.Metadata["gc.step_ref"] {
			continue
		}
		switch bead.Metadata["gc.kind"] {
		case "scope-check":
			clonedControl = bead
		default:
			clonedMember = bead
		}
	}
	if clonedMember.ID == "" {
		t.Fatal("missing cloned nested member for attempt 2")
	}
	if clonedControl.ID == "" {
		t.Fatal("missing cloned nested scope-check for attempt 2")
	}
	if clonedMember.Status != "open" {
		t.Fatalf("cloned member status = %q, want open", clonedMember.Status)
	}
	if clonedControl.Metadata["gc.scope_ref"] != run2.Metadata["gc.step_ref"] {
		t.Fatalf("cloned control scope_ref = %q, want %q", clonedControl.Metadata["gc.scope_ref"], run2.Metadata["gc.step_ref"])
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	foundMemberReady := false
	for _, bead := range ready {
		if bead.ID == clonedMember.ID {
			foundMemberReady = true
		}
	}
	if !foundMemberReady {
		t.Fatalf("expected cloned nested member %s to be ready; ready=%+v", clonedMember.ID, ready)
	}
}

func TestAppendRalphRetryClonesIterationFanoutControls(t *testing.T) {
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
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      "review-loop",
			"gc.step_ref":     "mol-review.review-loop",
			"gc.max_attempts": "2",
			"gc.root_bead_id": workflow.ID,
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "scope",
			"gc.scope_role":      "body",
			"gc.scope_name":      "review-loop",
			"gc.step_ref":        "mol-review.review-loop.iteration.1",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "List design council members",
		Type:  "task",
		Metadata: map[string]string{
			"gc.scope_ref":            "mol-review.review-loop.iteration.1",
			"gc.scope_role":           "member",
			"gc.step_ref":             "mol-review.review-loop.iteration.1.dc-members",
			"gc.step_id":              "dc-members",
			"gc.ralph_step_id":        "review-loop",
			"gc.attempt":              "1",
			"gc.root_bead_id":         workflow.ID,
			"gc.output_json_required": "true",
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for List design council members",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "fanout",
			"gc.scope_ref":     "mol-review.review-loop.iteration.1",
			"gc.scope_role":    "member",
			"gc.step_ref":      "mol-review.review-loop.iteration.1.dc-members-fanout",
			"gc.control_for":   "mol-review.review-loop.iteration.1.dc-members",
			"gc.for_each":      "output.members",
			"gc.bond":          "review-member",
			"gc.fanout_mode":   "parallel",
			"gc.step_id":       "dc-members",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.root_bead_id":  workflow.ID,
		},
	})
	check1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "review-loop",
			"gc.ralph_step_id":   "review-loop",
			"gc.attempt":         "1",
			"gc.step_ref":        "mol-review.review-loop.check.1",
			"gc.check_mode":      "exec",
			"gc.check_path":      ".gc/scripts/check.sh",
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    "2",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})

	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
	mustDepAdd(t, store, run1.ID, fanout.ID, "blocks")
	mustDepAdd(t, store, check1.ID, run1.ID, "blocks")
	mustDepAdd(t, store, logical.ID, check1.ID, "blocks")

	mapping, err := appendRalphRetry(store, logical.ID, run1, check1, 2, ProcessOptions{})
	if err != nil {
		t.Fatalf("appendRalphRetry: %v", err)
	}
	run2 := mustGetBead(t, store, mapping[run1.ID])
	source2 := mustGetBead(t, store, mapping[source.ID])
	fanout2 := mustGetBead(t, store, mapping[fanout.ID])

	if got := run2.Metadata["gc.step_ref"]; got != "mol-review.review-loop.iteration.2" {
		t.Fatalf("run2 gc.step_ref = %q, want mol-review.review-loop.iteration.2", got)
	}
	if got := source2.Metadata["gc.scope_ref"]; got != run2.Metadata["gc.step_ref"] {
		t.Fatalf("source2 gc.scope_ref = %q, want %q", got, run2.Metadata["gc.step_ref"])
	}
	if got := source2.Metadata["gc.step_ref"]; got != "mol-review.review-loop.iteration.2.dc-members" {
		t.Fatalf("source2 gc.step_ref = %q, want mol-review.review-loop.iteration.2.dc-members", got)
	}
	if got := source2.Metadata["gc.output_json_required"]; got != "true" {
		t.Fatalf("source2 gc.output_json_required = %q, want true", got)
	}
	if got := fanout2.Metadata["gc.scope_ref"]; got != run2.Metadata["gc.step_ref"] {
		t.Fatalf("fanout2 gc.scope_ref = %q, want %q", got, run2.Metadata["gc.step_ref"])
	}
	if got := fanout2.Metadata["gc.control_for"]; got != source2.Metadata["gc.step_ref"] {
		t.Fatalf("fanout2 gc.control_for = %q, want %q", got, source2.Metadata["gc.step_ref"])
	}
	if got := fanout2.Metadata["gc.step_ref"]; got != "mol-review.review-loop.iteration.2.dc-members-fanout" {
		t.Fatalf("fanout2 gc.step_ref = %q, want mol-review.review-loop.iteration.2.dc-members-fanout", got)
	}
	if got := fanout2.Metadata["gc.attempt"]; got != "2" {
		t.Fatalf("fanout2 gc.attempt = %q, want 2", got)
	}

	deps, err := store.DepList(fanout2.ID, "down")
	if err != nil {
		t.Fatalf("fanout2 deps: %v", err)
	}
	foundSourceDep := false
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == source2.ID {
			foundSourceDep = true
			break
		}
	}
	if !foundSourceDep {
		t.Fatalf("fanout2 missing blocks dependency on source2; deps = %+v", deps)
	}
}

func TestProcessRalphCheckRecoversPartialRetryAttempt(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "always-fail-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	partialRun2 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "run 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "2",
			"gc.step_ref":        "implement.run.2",
			"gc.retry_from":      run1.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.root_bead_id":    logical.Metadata["gc.root_bead_id"],
		},
	})
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	partialAfter := mustGetBead(t, store, partialRun2.ID)
	if partialAfter.Status != "closed" || partialAfter.Metadata["gc.partial_retry"] != "true" || partialAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("partial run2 = status %q partial_retry=%q outcome=%q, want closed/true/skipped", partialAfter.Status, partialAfter.Metadata["gc.partial_retry"], partialAfter.Metadata["gc.outcome"])
	}

	run2, check2 := nextSimpleAttempt(t, store, logical.ID)
	if run2.ID == partialRun2.ID {
		t.Fatal("expected recreated run2, got preserved partial bead")
	}
	if check2.Metadata["gc.attempt"] != "2" {
		t.Fatalf("check2 attempt = %q, want 2", check2.Metadata["gc.attempt"])
	}
}

func TestProcessRalphCheckRecoversIncompletelyWiredRetryAttempt(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "always-fail-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	partialRun2 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "run 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "2",
			"gc.step_ref":        "implement.run.2",
			"gc.retry_from":      run1.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.root_bead_id":    logical.Metadata["gc.root_bead_id"],
		},
	})
	partialCheck2 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "2",
			"gc.step_ref":        "implement.check.2",
			"gc.retry_from":      check1.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.root_bead_id":    logical.Metadata["gc.root_bead_id"],
		},
	})
	if err := store.DepAdd(logical.ID, partialCheck2.ID, "blocks"); err != nil {
		t.Fatalf("seed logical -> partial check2: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	for _, partialID := range []string{partialRun2.ID, partialCheck2.ID} {
		partial := mustGetBead(t, store, partialID)
		if partial.Status != "closed" || partial.Metadata["gc.partial_retry"] != "true" || partial.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("partial bead %s = status %q partial_retry=%q outcome=%q, want closed/true/skipped", partialID, partial.Status, partial.Metadata["gc.partial_retry"], partial.Metadata["gc.outcome"])
		}
	}

	run2, check2 := nextSimpleAttempt(t, store, logical.ID)
	if run2.ID == partialRun2.ID || check2.ID == partialCheck2.ID {
		t.Fatal("expected recreated retry attempt, got preserved incompletely wired beads")
	}
}

func TestProcessRalphCheckRecoversRetryAttemptMissingFinalAssigneePass(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "always-fail-check.sh", "#!/bin/bash\nset -euo pipefail\nexit 1\n")

	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	runner := "runner"
	checker := "checker"
	if err := store.Update(run1.ID, beads.UpdateOpts{Assignee: &runner}); err != nil {
		t.Fatalf("assign run1: %v", err)
	}
	if err := store.Update(check1.ID, beads.UpdateOpts{Assignee: &checker}); err != nil {
		t.Fatalf("assign check1: %v", err)
	}
	run1 = mustGetBead(t, store, run1.ID)
	check1 = mustGetBead(t, store, check1.ID)

	partialRun2 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "run 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "2",
			"gc.step_ref":        "implement.run.2",
			"gc.retry_from":      run1.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.root_bead_id":    logical.Metadata["gc.root_bead_id"],
		},
	})
	partialCheck2 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         "implement",
			"gc.ralph_step_id":   "implement",
			"gc.attempt":         "2",
			"gc.step_ref":        "implement.check.2",
			"gc.retry_from":      check1.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.root_bead_id":    logical.Metadata["gc.root_bead_id"],
		},
	})
	mustDepAdd(t, store, partialCheck2.ID, partialRun2.ID, "blocks")
	mustDepAdd(t, store, logical.ID, partialCheck2.ID, "blocks")
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(check1): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	for _, partialID := range []string{partialRun2.ID, partialCheck2.ID} {
		partial := mustGetBead(t, store, partialID)
		if partial.Status != "closed" || partial.Metadata["gc.partial_retry"] != "true" || partial.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("partial bead %s = status %q partial_retry=%q outcome=%q, want closed/true/skipped", partialID, partial.Status, partial.Metadata["gc.partial_retry"], partial.Metadata["gc.outcome"])
		}
	}

	run2, check2 := nextSimpleAttempt(t, store, logical.ID)
	if run2.ID == partialRun2.ID || check2.ID == partialCheck2.ID {
		t.Fatal("expected recreated retry attempt after missing assignee finalization")
	}
	if run2.Assignee != runner {
		t.Fatalf("run2 assignee = %q, want %q", run2.Assignee, runner)
	}
	if check2.Assignee != checker {
		t.Fatalf("check2 assignee = %q, want %q", check2.Assignee, checker)
	}
}

func TestProcessFanoutSpawnsFragmentsAndClosesOnSecondPass(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.scope_ref]
default = ""

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.scope_ref" = "{scope_ref}" }

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"},{"name":"codex"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                   "fanout",
			"gc.root_bead_id":           workflow.ID,
			"gc.control_for":            "demo.survey",
			"gc.for_each":               "output.items",
			"gc.bond":                   "expansion-review",
			"gc.bond_vars":              `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":            "parallel",
			"gc.controller_error":       "previous invalid connection",
			"gc.controller_error_class": "transient",
			"gc.controller_retryable":   "true",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result1, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result1.Processed || result1.Action != "fanout-spawn" {
		t.Fatalf("result1 = %+v, want processed fanout-spawn", result1)
	}

	fanoutAfterSpawn := mustGetBead(t, store, fanout.ID)
	if fanoutAfterSpawn.Status != "open" {
		t.Fatalf("fanout status after spawn = %q, want open", fanoutAfterSpawn.Status)
	}
	if got := fanoutAfterSpawn.Metadata["gc.fanout_state"]; got != "spawned" {
		t.Fatalf("fanout state = %q, want spawned", got)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var sinkIDs []string
	reviewCount := 0
	synthCount := 0
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != workflow.ID {
			continue
		}
		switch {
		case strings.HasSuffix(bead.Metadata["gc.step_ref"], ".review"):
			reviewCount++
			if !strings.Contains(bead.Title, "claude") && !strings.Contains(bead.Title, "codex") {
				t.Fatalf("unexpected review title %q", bead.Title)
			}
			if err := store.SetMetadataBatch(bead.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
				t.Fatalf("mark review pass: %v", err)
			}
			if err := store.Close(bead.ID); err != nil {
				t.Fatalf("close review: %v", err)
			}
		case strings.HasSuffix(bead.Metadata["gc.step_ref"], ".synth"):
			synthCount++
			sinkIDs = append(sinkIDs, bead.ID)
		}
	}
	if reviewCount != 2 || synthCount != 2 {
		t.Fatalf("spawned reviews=%d synths=%d, want 2/2", reviewCount, synthCount)
	}

	for _, sinkID := range sinkIDs {
		if err := store.SetMetadataBatch(sinkID, map[string]string{"gc.outcome": "pass"}); err != nil {
			t.Fatalf("mark sink pass: %v", err)
		}
		if err := store.Close(sinkID); err != nil {
			t.Fatalf("close sink: %v", err)
		}
	}

	result2, err := ProcessControl(store, mustGetBead(t, store, fanout.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout finalize): %v", err)
	}
	if !result2.Processed || result2.Action != "fanout-pass" {
		t.Fatalf("result2 = %+v, want processed fanout-pass", result2)
	}

	fanoutClosed := mustGetBead(t, store, fanout.ID)
	if fanoutClosed.Status != "closed" || fanoutClosed.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("fanout = status %q outcome %q, want closed/pass", fanoutClosed.Status, fanoutClosed.Metadata["gc.outcome"])
	}
	if fanoutClosed.Metadata["gc.controller_error"] != "" ||
		fanoutClosed.Metadata["gc.controller_error_class"] != "" ||
		fanoutClosed.Metadata["gc.controller_retryable"] != "" {
		t.Fatalf("controller error metadata = %#v, want cleared", fanoutClosed.Metadata)
	}
}

func TestProcessFanoutTransientFragmentInstantiationStaysOpenForRetry(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	base := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, base, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, base, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, base, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, base, fanout.ID, source.ID, "blocks")

	store := &failOnceCreateStore{
		Store: base,
		err:   errors.New("creating fragment bead: invalid connection: i/o timeout"),
	}
	_, err := ProcessControl(store, mustGetBead(t, store, fanout.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(fanout transient instantiate) error = %v, want %v", err, ErrControlPending)
	}

	after := mustGetBead(t, store, fanout.ID)
	if after.Status != "open" {
		t.Fatalf("fanout status = %q, want open", after.Status)
	}
	if after.Metadata["gc.controller_error_class"] != "transient" || after.Metadata["gc.controller_retryable"] != "true" {
		t.Fatalf("controller retry metadata = %v, want transient retryable", after.Metadata)
	}
}

func TestProcessFanoutRoutesFragmentRetryControlsToControlDispatcher(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"

[[rig]]
name = "gascity"
path = "/tmp/gascity"

[[agent]]
name = "reviewer"
dir = "gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.run_target" = "{reviewer}", "gc.scope_ref" = "body", "gc.scope_role" = "member" }

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"gascity/reviewer"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "fanout",
			"gc.root_bead_id":        workflow.ID,
			"gc.control_for":         "demo.survey",
			"gc.execution_routed_to": "gascity/reviewer",
			"gc.for_each":            "output.items",
			"gc.bond":                "expansion-review",
			"gc.bond_vars":           `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":         "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{
		CityPath:           dir,
		FormulaSearchPaths: []string{dir},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	logical := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if logical.ID == "" {
		t.Fatal("logical retry control not created")
	}
	if logical.Metadata["gc.kind"] != "retry" {
		t.Fatalf("logical gc.kind = %q, want retry", logical.Metadata["gc.kind"])
	}
	if got := logical.Assignee; got != "" {
		t.Fatalf("logical retry assignee = %q, want empty routed control-dispatcher queue", got)
	}
	if got := logical.Metadata["gc.routed_to"]; got != "gascity/control-dispatcher" {
		t.Fatalf("logical retry gc.routed_to = %q, want gascity/control-dispatcher", got)
	}
	if got := logical.Metadata["gc.execution_routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("logical retry gc.execution_routed_to = %q, want gascity/reviewer", got)
	}
}

func TestProcessFanoutRoutesFragmentMemberSteps(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"

[[rig]]
name = "gascity"
path = "/tmp/gascity"

[[agent]]
name = "reviewer"
dir = "gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.run_target" = "{reviewer}", "gc.scope_ref" = "body", "gc.scope_role" = "member" }
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"gascity/reviewer"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "fanout",
			"gc.root_bead_id":        workflow.ID,
			"gc.control_for":         "demo.survey",
			"gc.execution_routed_to": "gascity/reviewer",
			"gc.for_each":            "output.items",
			"gc.bond":                "expansion-review",
			"gc.bond_vars":           `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":         "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{
		CityPath:           dir,
		FormulaSearchPaths: []string{dir},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	member := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if member.ID == "" {
		t.Fatal("member step not created")
	}
	if got := member.Assignee; got != "" {
		t.Fatalf("member assignee = %q, want empty metadata-routed assignee", got)
	}
	if got := member.Metadata["gc.routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("member gc.routed_to = %q, want gascity/reviewer", got)
	}
	if got := member.Metadata["gc.execution_routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("member gc.execution_routed_to = %q, want gascity/reviewer", got)
	}
}

func TestProcessFanoutDoesNotUseControlRoutedToAsExecutionRoute(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"

[[rig]]
name = "gascity"
path = "/tmp/gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review"

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "fanout",
			"gc.root_bead_id":   workflow.ID,
			"gc.root_store_ref": "rig:gascity",
			"gc.control_for":    "demo.survey",
			"gc.routed_to":      "gascity/control-dispatcher",
			"gc.for_each":       "output.items",
			"gc.bond":           "expansion-review",
			"gc.fanout_mode":    "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{
		CityPath:           dir,
		FormulaSearchPaths: []string{dir},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	logical := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if logical.ID == "" {
		t.Fatal("logical retry control not created")
	}
	if got := logical.Metadata["gc.execution_routed_to"]; got != "" {
		t.Fatalf("logical retry gc.execution_routed_to = %q, want empty when control has no execution route", got)
	}
}

func TestProcessFanoutPreservesPreparedControlExecutionRoutes(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"

[[rig]]
name = "gascity"
path = "/tmp/gascity"

[[agent]]
name = "reviewer"
dir = "gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.run_target" = "{reviewer}", "gc.scope_ref" = "body", "gc.scope_role" = "member" }

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"gascity/reviewer"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "fanout",
			"gc.root_bead_id":        workflow.ID,
			"gc.control_for":         "demo.survey",
			"gc.execution_routed_to": "gascity/reviewer",
			"gc.for_each":            "output.items",
			"gc.bond":                "expansion-review",
			"gc.bond_vars":           `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":         "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{
		CityPath:           dir,
		FormulaSearchPaths: []string{dir},
		PrepareFragment: func(fragment *formula.FragmentRecipe, _ beads.Bead) error {
			for i := range fragment.Steps {
				if fragment.Steps[i].Metadata == nil {
					fragment.Steps[i].Metadata = make(map[string]string)
				}
				fragment.Steps[i].Metadata["gc.dynamic_fragment"] = "true"
			}
			formula.ApplyFragmentRecipeGraphControls(fragment)
			for i := range fragment.Steps {
				step := &fragment.Steps[i]
				switch step.Metadata["gc.kind"] {
				case "workflow", "scope", "ralph", "retry", "spec":
					continue
				case "scope-check", "workflow-finalize", "fanout", "check", "retry-eval":
					step.Metadata["gc.execution_routed_to"] = "gascity/reviewer"
					step.Metadata["gc.routed_to"] = "gascity/control-dispatcher"
					step.Assignee = ""
				default:
					step.Metadata["gc.routed_to"] = "gascity/reviewer"
					delete(step.Metadata, "gc.execution_routed_to")
					step.Assignee = "gascity--reviewer"
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	retryControl := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if retryControl.ID == "" {
		t.Fatal("retry control not created")
	}
	if retryControl.Metadata["gc.kind"] != "retry" {
		t.Fatalf("retry control gc.kind = %q, want retry", retryControl.Metadata["gc.kind"])
	}
	if got := retryControl.Assignee; got != "" {
		t.Fatalf("retry control assignee = %q, want empty routed control-dispatcher queue", got)
	}
	if got := retryControl.Metadata["gc.routed_to"]; got != "gascity/control-dispatcher" {
		t.Fatalf("retry control gc.routed_to = %q, want gascity/control-dispatcher", got)
	}
	if got := retryControl.Metadata["gc.execution_routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("retry control gc.execution_routed_to = %q, want gascity/reviewer", got)
	}

	scopeCheck := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review-scope-check")
	if scopeCheck.ID == "" {
		t.Fatal("scope-check control not created")
	}
	if scopeCheck.Metadata["gc.kind"] != "scope-check" {
		t.Fatalf("scope-check gc.kind = %q, want scope-check", scopeCheck.Metadata["gc.kind"])
	}
	if got := scopeCheck.Assignee; got != "" {
		t.Fatalf("scope-check assignee = %q, want empty routed control-dispatcher queue", got)
	}
	if got := scopeCheck.Metadata["gc.routed_to"]; got != "gascity/control-dispatcher" {
		t.Fatalf("scope-check gc.routed_to = %q, want gascity/control-dispatcher", got)
	}
	if got := scopeCheck.Metadata["gc.execution_routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("scope-check gc.execution_routed_to = %q, want gascity/reviewer", got)
	}
}

func TestProcessFanoutUsesResolvedSourceStepRefForIterationScopedFragments(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[vars.scope_ref]
default = "body"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.scope_ref" = "{scope_ref}" }
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "stale prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"stale"}]}`,
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.2.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.2",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}","scope_ref":"body"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	opts := testProcessOptionsWithControlDispatcher("")
	opts.FormulaSearchPaths = []string{dir}
	result, err := ProcessControl(store, fanout, opts)
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("spawn result = %+v, want processed fanout-spawn", result)
	}

	child := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.2.design-review.prepare-review-items.item.1.review")
	if child.ID == "" {
		t.Fatal("missing iteration-qualified fanout child")
	}
	if got := child.Metadata["gc.scope_ref"]; got != "mol.review-loop.iteration.2" {
		t.Fatalf("child gc.scope_ref = %q, want live fanout scope", got)
	}
	if stale := findAttemptByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.review"); stale.ID != "" {
		t.Fatalf("spawned stale logical child ref %q; want iteration-qualified source step ref", stale.Metadata["gc.step_ref"])
	}
}

func TestProcessFanoutPropagatesLiveScopeRefWithoutBondVarOverride(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[vars.scope_ref]
default = "body"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.scope_ref" = "{scope_ref}" }
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.4.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.4",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	opts := testProcessOptionsWithControlDispatcher("")
	opts.FormulaSearchPaths = []string{dir}
	result, err := ProcessControl(store, fanout, opts)
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("spawn result = %+v, want processed fanout-spawn", result)
	}

	child := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.4.design-review.prepare-review-items.item.1.review")
	if child.ID == "" {
		t.Fatal("missing iteration-qualified fanout child")
	}
	if got := child.Metadata["gc.scope_ref"]; got != "mol.review-loop.iteration.4" {
		t.Fatalf("child gc.scope_ref = %q, want live fanout scope without bond_vars scope_ref", got)
	}
}

func TestProcessFanoutDoesNotReusePriorIterationFragments(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "old review",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-review.design-review.prepare-review-items.item.1.review",
			"gc.outcome":      "pass",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.3.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.3",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if result.Created == 0 {
		t.Fatalf("fanout reused a prior-iteration fragment; created=%d", result.Created)
	}
	if child := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.3.design-review.prepare-review-items.item.1.review"); child.ID == "" {
		t.Fatal("missing new iteration-qualified review child")
	}
}

func TestProcessFanoutUsesControlForWhenSourceStepRefIsLogical(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[vars.reviewer]
required = true

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	if _, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if child := findAttemptByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.review"); child.ID == "" {
		t.Fatal("missing logical source child")
	}
}

func TestProcessFanoutRecreatesExistingFragmentWithStaleRouteMetadata(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "maintainer-city"

[[rig]]
name = "gascity"
path = "/tmp/gascity"

[[agent]]
name = "reviewer"
dir = "gascity"

[[agent]]
name = "control-dispatcher"
dir = "gascity"
max_active_sessions = 1
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
metadata = { "gc.run_target" = "{reviewer}", "gc.scope_ref" = "body", "gc.scope_role" = "member" }

[template.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"gascity/reviewer"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "fanout",
			"gc.root_bead_id":        workflow.ID,
			"gc.control_for":         "demo.survey",
			"gc.execution_routed_to": "gascity/reviewer",
			"gc.for_each":            "output.items",
			"gc.bond":                "expansion-review",
			"gc.bond_vars":           `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":         "parallel",
			"gc.fanout_state":        "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	fragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
		ID:          "demo.survey.item.1",
		Title:       source.Title,
		Description: source.Description,
	}, map[string]string{"reviewer": "gascity/reviewer"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment: %v", err)
	}
	if err := routeFanoutFragmentSteps(fragment, fanout, ProcessOptions{CityPath: dir}, store); err != nil {
		t.Fatalf("routeFanoutFragmentSteps: %v", err)
	}
	if _, err := molecule.InstantiateFragment(context.Background(), store, fragment, molecule.FragmentOptions{RootID: workflow.ID}); err != nil {
		t.Fatalf("InstantiateFragment: %v", err)
	}
	staleRetryControl := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if staleRetryControl.ID == "" {
		t.Fatal("stale retry control not created")
	}
	if err := store.SetMetadataBatch(staleRetryControl.ID, map[string]string{"gc.execution_routed_to": "gascity/old-reviewer"}); err != nil {
		t.Fatalf("stale route metadata: %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{
		CityPath:           dir,
		FormulaSearchPaths: []string{dir},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout resume): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created == 0 {
		t.Fatal("expected stale fragment to be discarded and recreated")
	}

	staleAfter := mustGetBead(t, store, staleRetryControl.ID)
	if staleAfter.Status != "closed" || staleAfter.Metadata["gc.partial_fragment"] != "true" || staleAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("stale retry control = status %q partial=%q outcome=%q, want closed/true/skipped", staleAfter.Status, staleAfter.Metadata["gc.partial_fragment"], staleAfter.Metadata["gc.outcome"])
	}
	recreated := findAttemptByRef(t, store, workflow.ID, "expansion-review.demo.survey.item.1.review")
	if recreated.ID == "" || recreated.ID == staleRetryControl.ID {
		t.Fatalf("recreated retry control ID = %q, stale ID = %q", recreated.ID, staleRetryControl.ID)
	}
	if got := recreated.Metadata["gc.execution_routed_to"]; got != "gascity/reviewer" {
		t.Fatalf("recreated retry control gc.execution_routed_to = %q, want gascity/reviewer", got)
	}
}

func TestProcessFanoutResumesExistingFragmentsWithoutDuplicates(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"},{"name":"codex"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	for i, reviewer := range []string{"claude", "codex"} {
		targetRef := "demo.survey.item." + strconv.Itoa(i+1)
		fragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
			ID:          targetRef,
			Title:       source.Title,
			Description: source.Description,
		}, map[string]string{"reviewer": reviewer})
		if err != nil {
			t.Fatalf("CompileExpansionFragment(%s): %v", reviewer, err)
		}
		if _, err := molecule.InstantiateFragment(context.Background(), store, fragment, molecule.FragmentOptions{RootID: workflow.ID}); err != nil {
			t.Fatalf("InstantiateFragment(%s): %v", reviewer, err)
		}
	}

	before, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List before: %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout resume): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created != 0 {
		t.Fatalf("result.Created = %d, want 0 newly created beads", result.Created)
	}

	after, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("bead count after resume = %d, want unchanged %d", len(after), len(before))
	}

	fanoutAfter := mustGetBead(t, store, fanout.ID)
	if got := fanoutAfter.Metadata["gc.fanout_state"]; got != "spawned" {
		t.Fatalf("fanout state after resume = %q, want spawned", got)
	}
}

func TestProcessFanoutResumesLegacyIterationFragmentsWithoutDuplicates(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.5.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.5",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	legacyFragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
		ID:          "design-review.prepare-review-items.item.1",
		Title:       source.Title,
		Description: source.Description,
	}, map[string]string{
		"reviewer":  "claude",
		"scope_ref": "mol.review-loop.iteration.5",
	})
	if err != nil {
		t.Fatalf("CompileExpansionFragment(legacy): %v", err)
	}
	legacyInst, err := molecule.InstantiateFragment(context.Background(), store, legacyFragment, molecule.FragmentOptions{RootID: workflow.ID})
	if err != nil {
		t.Fatalf("InstantiateFragment(legacy): %v", err)
	}
	for _, sinkID := range mapStepIDs(legacyFragment.Sinks, legacyInst.IDMapping) {
		mustDepAdd(t, store, fanout.ID, sinkID, "blocks")
	}

	before, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List before: %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout legacy resume): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created != 0 {
		t.Fatalf("result.Created = %d, want legacy fragment reuse without duplication", result.Created)
	}

	after, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("open bead count after legacy resume = %d, want unchanged %d", len(after), len(before))
	}

	if reused := findAttemptByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.review"); reused.ID == "" {
		t.Fatal("missing reused legacy fragment bead")
	}
	if duplicated := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.5.design-review.prepare-review-items.item.1.review"); duplicated.ID != "" {
		t.Fatalf("created duplicate iteration-qualified fragment %q instead of reusing legacy fragment", duplicated.ID)
	}
}

func TestProcessFanoutBlankStateRecreatesLegacyFragmentsWithoutOwnershipProof(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.6.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.6",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	legacyFragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
		ID:          "design-review.prepare-review-items.item.1",
		Title:       source.Title,
		Description: source.Description,
	}, map[string]string{"reviewer": "claude"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment(legacy): %v", err)
	}
	if _, err := molecule.InstantiateFragment(context.Background(), store, legacyFragment, molecule.FragmentOptions{RootID: workflow.ID}); err != nil {
		t.Fatalf("InstantiateFragment(legacy): %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout blank-state legacy recreate): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created == 0 {
		t.Fatal("expected blank-state fanout to recreate a current fragment when legacy ownership is unproven")
	}

	fanoutAfter := mustGetBead(t, store, fanout.ID)
	if got := fanoutAfter.Metadata["gc.fanout_state"]; got != "spawned" {
		t.Fatalf("fanout gc.fanout_state = %q, want spawned", got)
	}
	if child := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.6.design-review.prepare-review-items.item.1.review"); child.ID == "" {
		t.Fatal("missing new iteration-qualified review child")
	}
	legacyReview := findWorkflowBeadByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.review")
	legacySynth := findWorkflowBeadByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.synth")
	for _, legacy := range []beads.Bead{legacyReview, legacySynth} {
		if legacy.ID == "" {
			t.Fatal("missing retired legacy fragment bead")
		}
		if legacy.Status != "closed" {
			t.Fatalf("legacy bead %s status = %q, want closed", legacy.ID, legacy.Status)
		}
		if legacy.Metadata["gc.partial_fragment"] != "true" {
			t.Fatalf("legacy bead %s gc.partial_fragment = %q, want true", legacy.ID, legacy.Metadata["gc.partial_fragment"])
		}
		if mustReadyContains(t, store, legacy.ID) {
			t.Fatalf("legacy bead %s should no longer be ready/open", legacy.ID)
		}
	}
}

func TestProcessFanoutDoesNotReuseOpenLegacyFragmentsWithoutOwnershipProof(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.7.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.7",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	legacyFragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
		ID:          "design-review.prepare-review-items.item.1",
		Title:       source.Title,
		Description: source.Description,
	}, map[string]string{"reviewer": "claude"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment(legacy): %v", err)
	}
	if _, err := molecule.InstantiateFragment(context.Background(), store, legacyFragment, molecule.FragmentOptions{RootID: workflow.ID}); err != nil {
		t.Fatalf("InstantiateFragment(legacy): %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout legacy resume): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created == 0 {
		t.Fatal("expected a new iteration-qualified fragment when legacy ownership is unproven")
	}

	if child := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.7.design-review.prepare-review-items.item.1.review"); child.ID == "" {
		t.Fatal("missing new iteration-qualified review child")
	}
	legacyReview := findWorkflowBeadByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.review")
	legacySynth := findWorkflowBeadByRef(t, store, workflow.ID, "expansion-review.design-review.prepare-review-items.item.1.synth")
	for _, legacy := range []beads.Bead{legacyReview, legacySynth} {
		if legacy.ID == "" {
			t.Fatal("missing retired legacy fragment bead")
		}
		if legacy.Status != "closed" {
			t.Fatalf("legacy bead %s status = %q, want closed", legacy.ID, legacy.Status)
		}
		if legacy.Metadata["gc.partial_fragment"] != "true" {
			t.Fatalf("legacy bead %s gc.partial_fragment = %q, want true", legacy.ID, legacy.Metadata["gc.partial_fragment"])
		}
		if mustReadyContains(t, store, legacy.ID) {
			t.Fatalf("legacy bead %s should no longer be ready/open", legacy.ID)
		}
	}
}

func TestProcessFanoutDoesNotReuseClosedLegacyFragmentsFromPriorIteration(t *testing.T) {
	t.Parallel()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare items",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol.review-loop.iteration.7.design-review.prepare-review-items",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for prepare items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "mol.review-loop.iteration.7",
			"gc.control_for":  "design-review.prepare-review-items",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	legacyRef := "expansion-review.design-review.prepare-review-items.item.1.review"
	legacyFragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{dir}, &formula.Step{
		ID:          "design-review.prepare-review-items.item.1",
		Title:       source.Title,
		Description: source.Description,
	}, map[string]string{"reviewer": "claude"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment(legacy): %v", err)
	}
	if _, err := molecule.InstantiateFragment(context.Background(), store, legacyFragment, molecule.FragmentOptions{RootID: workflow.ID}); err != nil {
		t.Fatalf("InstantiateFragment(legacy): %v", err)
	}

	legacy := findAttemptByRef(t, store, workflow.ID, legacyRef)
	if legacy.ID == "" {
		t.Fatal("missing legacy fragment bead")
	}
	if err := store.SetMetadataBatch(legacy.ID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("mark legacy fragment pass: %v", err)
	}
	if err := store.Close(legacy.ID); err != nil {
		t.Fatalf("close legacy fragment: %v", err)
	}

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout closed legacy resume): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}
	if result.Created == 0 {
		t.Fatalf("result.Created = %d, want a new iteration-qualified fragment instead of closed legacy reuse", result.Created)
	}

	current := findAttemptByRef(t, store, workflow.ID, "expansion-review.mol.review-loop.iteration.7.design-review.prepare-review-items.item.1.review")
	if current.ID == "" {
		t.Fatal("missing new iteration-qualified fragment bead")
	}
	if current.ID == legacy.ID {
		t.Fatalf("reused closed legacy fragment %q for current iteration", current.ID)
	}

	all, err := store.ListByMetadata(map[string]string{"gc.root_bead_id": workflow.ID}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata(root): %v", err)
	}
	var legacyAfter beads.Bead
	for _, bead := range all {
		if bead.Metadata["gc.step_ref"] == legacyRef {
			legacyAfter = bead
			break
		}
	}
	if legacyAfter.ID == "" {
		t.Fatal("missing closed legacy fragment after resume")
	}
	if legacyAfter.Status != "closed" {
		t.Fatalf("legacy fragment status = %q, want closed", legacyAfter.Status)
	}
	if got := legacyAfter.Metadata["gc.partial_fragment"]; got != "" {
		t.Fatalf("legacy fragment gc.partial_fragment = %q, want preserved historical bead", got)
	}
	if got := legacyAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("legacy fragment gc.outcome = %q, want preserved pass outcome", got)
	}
}

func TestProcessFanoutSequentialChainsFragments(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-seq"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-seq.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"},{"name":"codex"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-seq",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "sequential",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout sequential): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	stepByRef := make(map[string]beads.Bead)
	for _, bead := range all {
		if strings.HasSuffix(bead.Metadata["gc.step_ref"], ".review") {
			stepByRef[bead.Metadata["gc.step_ref"]] = bead
		}
	}
	first := stepByRef["expansion-seq.demo.survey.item.1.review"]
	second := stepByRef["expansion-seq.demo.survey.item.2.review"]
	if first.ID == "" || second.ID == "" {
		t.Fatalf("missing sequential fragment steps: first=%q second=%q", first.ID, second.ID)
	}

	deps, err := store.DepList(second.ID, "down")
	if err != nil {
		t.Fatalf("DepList(second): %v", err)
	}
	foundChain := false
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == first.ID {
			foundChain = true
			break
		}
	}
	if !foundChain {
		t.Fatalf("expected %s to block on %s in sequential fanout", second.ID, first.ID)
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	if !beadListContainsID(ready, first.ID) {
		t.Fatalf("expected first fragment entry %s to be ready", first.ID)
	}
	if beadListContainsID(ready, second.ID) {
		t.Fatalf("second fragment entry %s should not be ready before first closes", second.ID)
	}
}

func TestProcessFanoutSequentialResumeRestoresExternalDeps(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-seq"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-seq.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"},{"name":"codex"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-seq",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "sequential",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	first := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-seq.demo.survey.item.1.review",
		},
	})
	secondPartial := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review codex",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-seq.demo.survey.item.2.review",
		},
	})

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout resume sequential): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	secondPartialAfter := mustGetBead(t, store, secondPartial.ID)
	if secondPartialAfter.Status != "closed" || secondPartialAfter.Metadata["gc.partial_fragment"] != "true" || secondPartialAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("partial second review = status %q partial_fragment=%q outcome=%q, want closed/true/skipped", secondPartialAfter.Status, secondPartialAfter.Metadata["gc.partial_fragment"], secondPartialAfter.Metadata["gc.outcome"])
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var second beads.Bead
	for _, bead := range all {
		if bead.ID == secondPartial.ID {
			continue
		}
		if bead.Metadata["gc.step_ref"] == "expansion-seq.demo.survey.item.2.review" {
			second = bead
			break
		}
	}
	if second.ID == "" {
		t.Fatal("missing recreated second fragment step")
	}

	deps, err := store.DepList(second.ID, "down")
	if err != nil {
		t.Fatalf("DepList(second): %v", err)
	}
	foundChain := false
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == first.ID {
			foundChain = true
			break
		}
	}
	if !foundChain {
		t.Fatalf("expected recreated %s to block on %s after resume", second.ID, first.ID)
	}
}

func TestProcessFanoutSequentialChainSurvivesEmptyMiddleFragment(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-seq-conditional"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-seq-conditional.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"},{"name":"skip"},{"name":"gemini"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-seq-conditional",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "sequential",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{
		FormulaSearchPaths: []string{dir},
		PrepareFragment: func(fragment *formula.FragmentRecipe, _ beads.Bead) error {
			for _, step := range fragment.Steps {
				if strings.Contains(step.ID, ".item.2.") {
					fragment.Steps = nil
					fragment.Deps = nil
					fragment.Entries = nil
					fragment.Sinks = nil
					break
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(fanout sequential with empty middle): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	stepByRef := make(map[string]beads.Bead)
	for _, bead := range all {
		if strings.HasSuffix(bead.Metadata["gc.step_ref"], ".review") {
			stepByRef[bead.Metadata["gc.step_ref"]] = bead
		}
	}
	first := stepByRef["expansion-seq-conditional.demo.survey.item.1.review"]
	third := stepByRef["expansion-seq-conditional.demo.survey.item.3.review"]
	if first.ID == "" || third.ID == "" {
		t.Fatalf("missing sequential fragment steps after empty middle: first=%q third=%q", first.ID, third.ID)
	}
	if _, exists := stepByRef["expansion-seq-conditional.demo.survey.item.2.review"]; exists {
		t.Fatal("middle fragment should have been filtered out entirely")
	}

	deps, err := store.DepList(third.ID, "down")
	if err != nil {
		t.Fatalf("DepList(third): %v", err)
	}
	foundChain := false
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == first.ID {
			foundChain = true
			break
		}
	}
	if !foundChain {
		t.Fatalf("expected %s to block on %s after empty middle fragment", third.ID, first.ID)
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	if !beadListContainsID(ready, first.ID) {
		t.Fatalf("expected first fragment entry %s to be ready", first.ID)
	}
	if beadListContainsID(ready, third.ID) {
		t.Fatalf("third fragment entry %s should not be ready before first closes", third.ID)
	}
}

func TestProcessFanoutRecoversPartialFragmentInstance(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	partial := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-review.demo.survey.item.1.review",
		},
	})

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout recover partial): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	partialAfter := mustGetBead(t, store, partial.ID)
	if partialAfter.Status != "closed" || partialAfter.Metadata["gc.partial_fragment"] != "true" || partialAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("partial bead = status %q partial=%q outcome=%q, want closed/true/skipped", partialAfter.Status, partialAfter.Metadata["gc.partial_fragment"], partialAfter.Metadata["gc.outcome"])
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	recreated := 0
	for _, bead := range all {
		if bead.ID == partial.ID {
			continue
		}
		switch bead.Metadata["gc.step_ref"] {
		case "expansion-review.demo.survey.item.1.review", "expansion-review.demo.survey.item.1.synth":
			recreated++
		}
	}
	if recreated != 2 {
		t.Fatalf("recreated steps = %d, want 2", recreated)
	}
}

func TestProcessFanoutRecoversIncompletelyWiredFragmentInstance(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"

[[template]]
id = "{target}.synth"
title = "Synthesize {reviewer}"
needs = ["{target}.review"]
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
			"gc.fanout_state": "spawning",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	partialReview := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-review.demo.survey.item.1.review",
		},
	})
	partialSynth := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Synthesize claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "expansion-review.demo.survey.item.1.synth",
		},
	})

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout recover incomplete): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("result = %+v, want processed fanout-spawn", result)
	}

	for _, partialID := range []string{partialReview.ID, partialSynth.ID} {
		partial := mustGetBead(t, store, partialID)
		if partial.Status != "closed" || partial.Metadata["gc.partial_fragment"] != "true" || partial.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("partial bead %s = status %q partial_fragment=%q outcome=%q, want closed/true/skipped", partialID, partial.Status, partial.Metadata["gc.partial_fragment"], partial.Metadata["gc.outcome"])
		}
	}
}

func TestFragmentInstanceCompleteAllowsRetriedRalphLogicalDep(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.review-loop",
		},
	})
	originalCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Check review-loop",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.root_bead_id":    "wf-1",
			"gc.step_ref":        "demo.review-loop.check.1",
			"gc.logical_bead_id": logical.ID,
			"gc.outcome":         "fail",
		},
	})
	retryCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Check review-loop retry",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.root_bead_id":    "wf-1",
			"gc.step_ref":        "demo.review-loop.check.2",
			"gc.logical_bead_id": logical.ID,
		},
	})
	mustDepAdd(t, store, logical.ID, retryCheck.ID, "blocks")

	fragment := &formula.FragmentRecipe{
		Name: "expansion-review",
		Steps: []formula.RecipeStep{
			{
				ID:       "expansion-review.review-loop",
				Title:    "Review loop",
				Metadata: map[string]string{"gc.kind": "ralph"},
			},
			{
				ID:       "expansion-review.review-loop.check.1",
				Title:    "Check review-loop",
				Metadata: map[string]string{"gc.kind": "check"},
			},
		},
		Deps: []formula.RecipeDep{
			{
				StepID:      "expansion-review.review-loop",
				DependsOnID: "expansion-review.review-loop.check.1",
				Type:        "blocks",
			},
		},
	}
	mapping := map[string]string{
		"expansion-review.review-loop":         logical.ID,
		"expansion-review.review-loop.check.1": originalCheck.ID,
	}

	complete, err := fragmentInstanceComplete(store, fragment, mapping, nil)
	if err != nil {
		t.Fatalf("fragmentInstanceComplete: %v", err)
	}
	if !complete {
		t.Fatal("expected retry-rewired fragment instance to be treated as complete")
	}
}

func TestCollectRalphAttemptBeadsSkipsDynamicFragments(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	subject := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Review scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "review-loop.run.1",
		},
	})
	staticMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Synthesis",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "review-loop.run.1",
		},
	})
	dynamicMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Fanout child",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id":     "wf-1",
			"gc.scope_ref":        "review-loop.run.1",
			"gc.dynamic_fragment": "true",
		},
	})

	attemptSet, err := collectRalphAttemptBeads(store, subject)
	if err != nil {
		t.Fatalf("collectRalphAttemptBeads: %v", err)
	}
	if _, ok := attemptSet[subject.ID]; !ok {
		t.Fatal("missing subject from attempt set")
	}
	if _, ok := attemptSet[staticMember.ID]; !ok {
		t.Fatal("missing static member from attempt set")
	}
	if _, ok := attemptSet[dynamicMember.ID]; ok {
		t.Fatal("dynamic fragment member should not be cloned into the next retry attempt")
	}
}

func TestResolveWorkflowStepByRefFromBeadsPrefersExactMatch(t *testing.T) {
	t.Parallel()

	exact := beads.Bead{ID: "exact", Metadata: map[string]string{"gc.step_ref": "demo.survey"}}
	suffix := beads.Bead{ID: "suffix", Metadata: map[string]string{"gc.step_ref": "other.demo.survey"}}

	got, err := resolveWorkflowStepByRefFromBeads([]beads.Bead{suffix, exact}, "wf-1", "demo.survey", workflowStepMatchOptions{})
	if err != nil {
		t.Fatalf("resolveWorkflowStepByRefFromBeads: %v", err)
	}
	if got.ID != exact.ID {
		t.Fatalf("matched bead %s, want exact match %s", got.ID, exact.ID)
	}
}

func TestResolveWorkflowStepByRefFromBeadsPrefersCurrentBlockerMatch(t *testing.T) {
	t.Parallel()

	exact := beads.Bead{ID: "exact", Metadata: map[string]string{"gc.step_ref": "demo.survey"}}
	current := beads.Bead{ID: "current", Metadata: map[string]string{"gc.step_ref": "mol.iteration.2.demo.survey"}}

	got, err := resolveWorkflowStepByRefFromBeads(
		[]beads.Bead{exact, current},
		"wf-1",
		"demo.survey",
		workflowStepMatchOptions{PreferredIDs: map[string]struct{}{current.ID: {}}},
	)
	if err != nil {
		t.Fatalf("resolveWorkflowStepByRefFromBeads: %v", err)
	}
	if got.ID != current.ID {
		t.Fatalf("matched bead %s, want current blocker %s", got.ID, current.ID)
	}
}

func TestCopyRetryDepsSkipsDynamicFragmentTargets(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	oldControl := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout", Type: "task"})
	newControl := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout retry", Type: "task"})
	dynamicSink := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "dynamic sink",
		Type:  "task",
		Metadata: map[string]string{
			"gc.dynamic_fragment": "true",
		},
	})
	mustDepAdd(t, store, oldControl.ID, dynamicSink.ID, "blocks")

	if err := copyRetryDeps(store, oldControl.ID, newControl.ID, map[string]string{}); err != nil {
		t.Fatalf("copyRetryDeps: %v", err)
	}

	deps, err := store.DepList(newControl.ID, "down")
	if err != nil {
		t.Fatalf("DepList(newControl): %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("new control deps = %+v, want none", deps)
	}
}

func TestCopiedDepsPresentSkipsDynamicFragmentTargets(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	oldControl := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout", Type: "task"})
	newControl := mustCreateWorkflowBead(t, store, beads.Bead{Title: "fanout retry", Type: "task"})
	dynamicSink := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "dynamic sink",
		Type:  "task",
		Metadata: map[string]string{
			"gc.dynamic_fragment": "true",
		},
	})
	mustDepAdd(t, store, oldControl.ID, dynamicSink.ID, "blocks")

	ok, err := copiedDepsPresent(store, oldControl.ID, newControl.ID, map[string]string{})
	if err != nil {
		t.Fatalf("copiedDepsPresent: %v", err)
	}
	if !ok {
		t.Fatal("expected dynamic-fragment deps to be ignored during retry resume validation")
	}
}

func TestCanDiscardPartialFragmentBeadWaitsForDependents(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	dependent := mustCreateWorkflowBead(t, store, beads.Bead{Title: "dependent", Type: "task"})
	blocker := mustCreateWorkflowBead(t, store, beads.Bead{Title: "blocker", Type: "task"})
	mustDepAdd(t, store, dependent.ID, blocker.ID, "blocks")

	pending := map[string]beads.Bead{
		dependent.ID: dependent,
		blocker.ID:   blocker,
	}
	if canDiscardPartialFragmentBead(store, blocker.ID, pending) {
		t.Fatal("blocker should not be discarded before its dependent")
	}
	if !canDiscardPartialFragmentBead(store, dependent.ID, pending) {
		t.Fatal("dependent should be discardable before its blocker")
	}
}

func TestProcessFanoutClosesScopeWhenLastMember(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

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
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "body",
			"gc.scope_role":   "body",
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
	mustDepAdd(t, store, body.ID, fanout.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawn): %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("spawn result = %+v, want processed fanout-spawn", result)
	}

	var sinkID string
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, bead := range all {
		if strings.HasSuffix(bead.Metadata["gc.step_ref"], ".review") {
			sinkID = bead.ID
			break
		}
	}
	if sinkID == "" {
		t.Fatal("missing spawned fanout sink")
	}
	if err := store.SetMetadataBatch(sinkID, map[string]string{"gc.outcome": "pass"}); err != nil {
		t.Fatalf("set sink outcome: %v", err)
	}
	if err := store.Close(sinkID); err != nil {
		t.Fatalf("close sink: %v", err)
	}

	result, err = ProcessControl(store, mustGetBead(t, store, fanout.ID), ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(fanout finalize): %v", err)
	}
	if !result.Processed || result.Action != "fanout-pass" {
		t.Fatalf("finalize result = %+v, want processed fanout-pass", result)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("body = status %q outcome %q, want closed/pass", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	if got := bodyAfter.Metadata["gc.output_json"]; got != `{"items":[{"name":"claude"}]}` {
		t.Fatalf("body gc.output_json = %q, want propagated source output", got)
	}
}

func TestProcessFanoutSpawnedNoOpsWhileBlockersRemainOpen(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.output_json": `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "fanout",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.fanout_state": "spawned",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(fanout spawned pending): %v", err)
	}
	if result.Processed {
		t.Fatalf("result = %+v, want no-op while blockers remain open", result)
	}

	fanoutAfter := mustGetBead(t, store, fanout.ID)
	if fanoutAfter.Status != "open" {
		t.Fatalf("fanout status = %q, want open", fanoutAfter.Status)
	}
}

func TestDiscardPartialFragmentMarksClosedBeads(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := mustCreateWorkflowBead(t, store, beads.Bead{Title: "partial", Type: "task", Status: "closed"})

	if err := discardPartialFragmentInstance(store, map[string]beads.Bead{bead.ID: bead}); err != nil {
		t.Fatalf("discardPartialFragmentInstance: %v", err)
	}

	after := mustGetBead(t, store, bead.ID)
	if after.Metadata["gc.partial_fragment"] != "true" || after.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("closed partial fragment metadata = %#v, want tombstone metadata", after.Metadata)
	}
}

func TestDiscardPartialRetryMarksClosedBeads(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	bead := mustCreateWorkflowBead(t, store, beads.Bead{Title: "partial", Type: "task", Status: "closed"})

	if err := discardPartialRalphRetry(store, map[string]beads.Bead{bead.ID: bead}); err != nil {
		t.Fatalf("discardPartialRalphRetry: %v", err)
	}

	after := mustGetBead(t, store, bead.ID)
	if after.Metadata["gc.partial_retry"] != "true" || after.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("closed partial retry metadata = %#v, want tombstone metadata", after.Metadata)
	}
}

func TestProcessFanoutFailsWhenSourceFailed(t *testing.T) {
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
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.outcome":      "fail",
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(fanout fail): %v", err)
	}
	if !result.Processed || result.Action != "fanout-fail" {
		t.Fatalf("result = %+v, want processed fanout-fail", result)
	}

	fanoutAfter := mustGetBead(t, store, fanout.ID)
	if fanoutAfter.Status != "closed" || fanoutAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("fanout = status %q outcome %q, want closed/fail", fanoutAfter.Status, fanoutAfter.Metadata["gc.outcome"])
	}
}

// Regression test for gastownhall/gascity#1657, fanout sibling site: a fanout
// whose abort_scope source closed bare (no gc.outcome) must fail instead of
// attempting expansion against the missing required output.
func TestProcessFanoutFailsWhenAbortScopeSourceClosedBare(t *testing.T) {
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
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.survey",
			"gc.on_fail":      "abort_scope",
			"close_reason":    "FAIL: subagent returned non-zero",
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")

	result, err := ProcessControl(store, fanout, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(fanout bare-failed source): %v", err)
	}
	if !result.Processed || result.Action != "fanout-fail" {
		t.Fatalf("result = %+v, want processed fanout-fail", result)
	}

	fanoutAfter := mustGetBead(t, store, fanout.ID)
	if fanoutAfter.Status != "closed" || fanoutAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("fanout = status %q outcome %q, want closed/fail", fanoutAfter.Status, fanoutAfter.Metadata["gc.outcome"])
	}
}

func TestClearRetryEphemeraPreservesRoutingAndClearsFanoutState(t *testing.T) {
	t.Parallel()

	meta := map[string]string{
		"gc.routed_to":     "demo/reviewer",
		"gc.outcome":       "pass",
		"gc.fanout_state":  "spawned",
		"gc.spawned_count": "2",
	}

	clearRetryEphemera(meta)

	if got := meta["gc.routed_to"]; got != "demo/reviewer" {
		t.Fatalf("gc.routed_to = %q, want preserved", got)
	}
	if _, ok := meta["gc.outcome"]; ok {
		t.Fatal("gc.outcome should be cleared")
	}
	if _, ok := meta["gc.fanout_state"]; ok {
		t.Fatal("gc.fanout_state should be cleared")
	}
	if _, ok := meta["gc.spawned_count"]; ok {
		t.Fatal("gc.spawned_count should be cleared")
	}
	if _, ok := meta["gc.output_json"]; ok {
		t.Fatal("gc.output_json should be cleared")
	}
}

func TestRewriteRalphAttemptRefRespectsAttemptBoundaries(t *testing.T) {
	t.Parallel()

	got := rewriteRalphAttemptRef("review-loop.run.10.review", 1, 2)
	if got != "review-loop.run.10.review" {
		t.Fatalf("rewriteRalphAttemptRef() = %q, want unchanged run.10 ref", got)
	}
}

func TestRewriteRalphAttemptRefRewritesInnermostMatchingAttempt(t *testing.T) {
	t.Parallel()

	got := rewriteRalphAttemptRef("outer.run.1.inner.run.1", 1, 2)
	if got != "outer.run.1.inner.run.2" {
		t.Fatalf("rewriteRalphAttemptRef() = %q, want innermost attempt rewritten", got)
	}
}

func TestRewriteRalphAttemptRefRewritesIterationAttempt(t *testing.T) {
	t.Parallel()

	got := rewriteRalphAttemptRef("mol-review.review-loop.iteration.1", 1, 2)
	if got != "mol-review.review-loop.iteration.2" {
		t.Fatalf("rewriteRalphAttemptRef() = %q, want iteration attempt rewritten", got)
	}
}

func TestResolveInheritedMetadataPrefersParentBeforeWorkflowRoot(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow",
			"gc.root_bead_id": "wf-root",
			"work_dir":        "/workflow/root",
		},
	})
	parent := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "run body",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"work_dir":        "/run/body",
		},
	})
	check := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "check",
		Type:     "task",
		ParentID: parent.ID,
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})

	got := resolveInheritedMetadata(store, check, "work_dir")
	if got != "/run/body" {
		t.Fatalf("resolveInheritedMetadata() = %q, want parent-scoped value", got)
	}
}

func TestRunRalphCheckResolvesRelativeWorkDirAgainstCityPath(t *testing.T) {
	cityPath := t.TempDir()
	workDir := filepath.Join(cityPath, "frontend")
	checkDir := filepath.Join(workDir, "checks")
	if err := os.MkdirAll(checkDir, 0o755); err != nil {
		t.Fatalf("mkdir check dir: %v", err)
	}

	checkPath := filepath.Join(checkDir, "pass.sh")
	writeExecutableScript(t, checkPath, "#!/usr/bin/env bash\nexit 0\n")

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-1",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path": "checks/pass.sh",
			// This test is about relative path resolution, not timeout behavior.
			// Use a generous deadline so repo-wide test load does not turn it into
			// a spurious timeout flake.
			"gc.check_timeout": "30s",
			"gc.work_dir":      "frontend",
		},
	}
	subject := beads.Bead{ID: "run-1", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != "pass" {
		t.Fatalf("result.Outcome = %q, want pass", result.Outcome)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result.ExitCode = %+v, want 0", result.ExitCode)
	}
}

func TestRunRalphCheckRejectsNonPositiveMetadataTimeouts(t *testing.T) {
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "pass.sh", "#!/usr/bin/env bash\nexit 0\n")

	tests := []struct {
		name string
		key  string
		raw  string
	}{
		{name: "step zero", key: "gc.step_timeout", raw: "0s"},
		{name: "step negative", key: "gc.step_timeout", raw: "-1s"},
		{name: "check zero", key: "gc.check_timeout", raw: "0s"},
		{name: "check negative", key: "gc.check_timeout", raw: "-1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			check := beads.Bead{
				ID:   "check-1",
				Type: "task",
				Metadata: map[string]string{
					"gc.check_path": checkPath,
					tt.key:          tt.raw,
				},
			}
			subject := beads.Bead{ID: "run-1", Type: "task"}

			_, err := runRalphCheck(store, check, subject, 1, ProcessOptions{CityPath: cityPath})
			if err == nil {
				t.Fatalf("runRalphCheck succeeded, want non-positive %s error", tt.key)
			}
			if !strings.Contains(err.Error(), "must be positive") {
				t.Fatalf("runRalphCheck error = %v, want positive timeout error", err)
			}
		})
	}
}

func TestRunRalphCheckTimeoutMetadataPrecedence(t *testing.T) {
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "sleep.sh", "#!/usr/bin/env bash\nsleep 0.05\nexit 0\n")
	store := beads.NewMemStore()
	subject := beads.Bead{ID: "run-1", Type: "task"}

	stepOnly := beads.Bead{
		ID:   "step-only",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":   checkPath,
			"gc.step_timeout": "1ms",
		},
	}
	stepResult, err := runRalphCheck(store, stepOnly, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck step-only: %v", err)
	}
	if stepResult.Outcome != convergence.GateTimeout {
		t.Fatalf("step-only outcome = %q, want timeout", stepResult.Outcome)
	}

	checkOverrides := beads.Bead{
		ID:   "check-overrides",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkPath,
			"gc.step_timeout":  "1ms",
			"gc.check_timeout": "30s",
		},
	}
	checkResult, err := runRalphCheck(store, checkOverrides, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck check-overrides: %v", err)
	}
	if checkResult.Outcome != convergence.GatePass {
		t.Fatalf("check-overrides outcome = %q, want pass", checkResult.Outcome)
	}
}

func TestRunRalphCheckUsesStorePathForRelativeCheckAndSubjectEnv(t *testing.T) {
	cityPath := t.TempDir()
	// storePath models a rig store living as a subtree of the city, matching
	// the production rig layout. The disjoint-tempdir construction this test
	// previously used was unrealistic and obscured the gastownhall/gascity#2320
	// envelope/base distinction: relative gc.check_path now resolves under
	// storePath (the join base) but containment is validated against cityPath
	// (the security envelope).
	storePath := filepath.Join(cityPath, "rig-store")
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir rig-store: %v", err)
	}
	workDir := filepath.Join(storePath, "frontend")
	checkDir := filepath.Join(workDir, "checks")
	if err := os.MkdirAll(checkDir, 0o755); err != nil {
		t.Fatalf("mkdir check dir: %v", err)
	}

	checkPath := filepath.Join(checkDir, "env.sh")
	script := "#!/bin/sh\n" +
		"pwd\n" +
		"printf 'BEAD=%s\\n' \"$GC_BEAD_ID\"\n" +
		"printf 'CITY=%s\\n' \"$GC_CITY\"\n" +
		"printf 'STORE=%s\\n' \"$GC_STORE_PATH\"\n" +
		"printf 'BEADS=%s\\n' \"$BEADS_DIR\"\n"
	writeExecutableScript(t, checkPath, script)

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-1",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    "checks/env.sh",
			"gc.check_timeout": "30s",
			"gc.work_dir":      "frontend",
		},
	}
	subject := beads.Bead{ID: "run-1", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 2, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != "pass" {
		t.Fatalf("result.Outcome = %q, want pass (stderr=%q)", result.Outcome, result.Stderr)
	}
	for _, want := range []string{
		workDir,
		"BEAD=run-1",
		"CITY=" + cityPath,
		"STORE=" + storePath,
		"BEADS=" + filepath.Join(storePath, ".beads"),
	} {
		if !strings.Contains(result.Stdout, want) {
			t.Fatalf("stdout = %q, want to contain %q", result.Stdout, want)
		}
	}
}

// TestRunRalphCheckRigScopedRelativeCheckPathResolvesAgainstStore pins the
// gastownhall/gascity#2320 fix: a ralph check bead with a relative
// gc.check_path and a storePath that is a SUBTREE of cityPath. The
// gc.check_path here (`../scripts/check.sh`) deliberately escapes the store
// (the join base) upward into the city tree (the security envelope) — the
// exact shape that fails pre-fix. Before the envelope/base split,
// ResolveConditionPath conflated the two roles, so this relative path was
// validated against storePath and the traversal check rejected it even
// though the script is comfortably inside the city envelope.
func TestRunRalphCheckRigScopedRelativeCheckPathResolvesAgainstStore(t *testing.T) {
	cityPath := t.TempDir()
	storePath := filepath.Join(cityPath, "rig-frontend")
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	// Script lives under the city tree, NOT under the store. A relative
	// gc.check_path must climb out of the store to reach it.
	scriptDir := filepath.Join(cityPath, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "check.sh")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nexit 0\n")

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-rig",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    "../scripts/check.sh",
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-rig", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != "pass" {
		t.Fatalf("Outcome = %q, want pass (stderr=%q)", result.Outcome, result.Stderr)
	}
}

// TestRunRalphCheckSiblingStoreRelativeCheckPathResolves pins the
// gastownhall/gascity#2354 fix: when storePath is a SIBLING of cityPath
// (neither a subtree of the other), a relative gc.check_path that joins
// under the store must still resolve. Before the fix, the traversal
// guard rejected paths under the store because they were outside the
// city envelope; this is the canonical operator layout where rig and
// city live as separate directories under $HOME.
func TestRunRalphCheckSiblingStoreRelativeCheckPathResolves(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	// Script lives under the store at the path runRalphCheck would synthesize
	// for a pack-shipped check (relative gc.check_path joined against base).
	scriptDir := filepath.Join(storePath, "assets", "pack", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "check.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-sibling",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    "assets/pack/scripts/check.sh",
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-sibling", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != "pass" {
		t.Fatalf("Outcome = %q, want pass (stderr=%q)", result.Outcome, result.Stderr)
	}
}

// TestRunRalphCheckRejectsPathTraversalAboveCityPath pins the security
// contract: when envelope (cityPath) and base (scriptBase) diverge, a
// relative gc.check_path that traverses above the city must still be
// rejected even though it might otherwise resolve via the join base.
func TestRunRalphCheckRejectsPathTraversalAboveCityPath(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(cityPath, "rig-frontend")
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Outside script lives above cityPath — must be rejected by the envelope.
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	outsideScript := filepath.Join(outsideDir, "check.sh")
	writeExecutableScript(t, outsideScript, "#!/bin/sh\nexit 0\n")

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-evil",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    "../../outside/check.sh",
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-evil", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err == nil {
		t.Fatalf("expected traversal rejection, got outcome=%q stdout=%q", result.Outcome, result.Stdout)
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("expected traversal error, got: %v", err)
	}
}

// TestRunRalphCheckAllowsAbsoluteCheckPath pins the registry/import-pack
// behavior: packs can be installed outside the city/store roots, so formula
// expansion may produce an absolute gc.check_path.
func TestRunRalphCheckAllowsAbsoluteCheckPath(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	packDir := filepath.Join(home, ".gc", "cache", "repos", "pack-key", "packs", "workflows")
	if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
		t.Fatalf("mkdir pack formulas: %v", err)
	}
	checkScript := filepath.Join(packDir, "check.sh")
	if err := os.WriteFile(checkScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-abs",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkScript,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-abs", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(packDir, "formulas")},
	})
	if err != nil {
		t.Fatalf("absolute check path should be accepted: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("outcome = %q, want %q; stdout=%q stderr=%q", result.Outcome, convergence.GatePass, result.Stdout, result.Stderr)
	}
}

func TestRunRalphCheckAllowsAbsoluteCheckPathUnderFormulaPackRoot(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("HOME", filepath.Join(parent, "home"))
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	localPackRoot := filepath.Join(parent, "local-workflows-pack")
	for _, dir := range []string{cityPath, storePath, filepath.Join(localPackRoot, "formulas"), filepath.Join(localPackRoot, "assets", "scripts", "checks")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	checkScript := filepath.Join(localPackRoot, "assets", "scripts", "checks", "review-approved.sh")
	if err := os.WriteFile(checkScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-local-pack",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkScript,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-local-pack", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(localPackRoot, "formulas")},
	})
	if err != nil {
		t.Fatalf("absolute check path under formula pack root should be accepted: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("outcome = %q, want %q; stdout=%q stderr=%q", result.Outcome, convergence.GatePass, result.Stdout, result.Stderr)
	}
}

func TestRunRalphCheckRejectsAbsoluteCheckPathUnderUnrelatedCachedPack(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	activePackRoot := filepath.Join(home, ".gc", "cache", "repos", "active-key", "packs", "workflows")
	otherPackRoot := filepath.Join(home, ".gc", "cache", "repos", "other-key", "packs", "workflows")
	for _, dir := range []string{cityPath, storePath, filepath.Join(activePackRoot, "formulas"), filepath.Join(otherPackRoot, "assets", "scripts")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	checkScript := filepath.Join(otherPackRoot, "assets", "scripts", "check.sh")
	if err := os.WriteFile(checkScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-other-pack",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkScript,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-other-pack", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(activePackRoot, "formulas")},
	})
	if err == nil {
		t.Fatalf("expected unrelated cached pack rejection, got outcome=%q stdout=%q", result.Outcome, result.Stdout)
	}
	if !strings.Contains(err.Error(), "trusted roots") {
		t.Errorf("expected trusted roots error, got: %v", err)
	}
}

func TestRunRalphCheckRejectsAbsoluteCheckPathOutsideTrustedRoots(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	checkScript := filepath.Join(outsideDir, "check.sh")
	if err := os.WriteFile(checkScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-abs-outside",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkScript,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-abs-outside", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err == nil {
		t.Fatalf("expected absolute check path rejection, got outcome=%q stdout=%q", result.Outcome, result.Stdout)
	}
	if !strings.Contains(err.Error(), "trusted roots") {
		t.Errorf("expected trusted roots error, got: %v", err)
	}
}

func TestRunRalphCheckRejectsAbsoluteCheckPathSymlinkOutsideTrustedRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	packDir := filepath.Join(home, ".gc", "cache", "repos", "pack-key")
	outsideDir := filepath.Join(parent, "outside")
	for _, dir := range []string{cityPath, storePath, packDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	outsideScript := filepath.Join(outsideDir, "check.sh")
	if err := os.WriteFile(outsideScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write outside check script: %v", err)
	}
	checkLink := filepath.Join(packDir, "check.sh")
	if err := os.Symlink(outsideScript, checkLink); err != nil {
		t.Fatalf("symlink check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-abs-symlink-outside",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkLink,
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-abs-symlink-outside", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err == nil {
		t.Fatalf("expected symlinked absolute check path rejection, got outcome=%q stdout=%q", result.Outcome, result.Stdout)
	}
	if !strings.Contains(err.Error(), "trusted roots") {
		t.Errorf("expected trusted roots error, got: %v", err)
	}
}

// TestRunRalphCheckAllowsAbsoluteCheckPathWithExternalWorkDir pins the
// PR-review worktree contract: adopted PR worktrees are sibling directories
// outside both the city and store roots, while review checks are absolute
// pack-local scripts whose source is independently trusted.
func TestRunRalphCheckAllowsAbsoluteCheckPathWithExternalWorkDir(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	worktreeDir := filepath.Join(parent, "worktree")
	packRoot := filepath.Join(parent, "workflows-pack")
	for _, dir := range []string{
		cityPath,
		storePath,
		worktreeDir,
		filepath.Join(packRoot, "formulas"),
		filepath.Join(packRoot, "assets", "scripts", "checks"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	checkScript := filepath.Join(packRoot, "assets", "scripts", "checks", "review-approved.sh")
	if err := os.WriteFile(checkScript, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$GC_WORK_DIR\" \"$(pwd)\"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pack check script: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-abs-workdir",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    checkScript,
			"gc.work_dir":      worktreeDir, // absolute, outside both roots
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-abs-workdir", Type: "task"}

	result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:           cityPath,
		StorePath:          storePath,
		FormulaSearchPaths: []string{filepath.Join(packRoot, "formulas")},
	})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != "pass" {
		t.Fatalf("Outcome = %q, want pass (stderr=%q)", result.Outcome, result.Stderr)
	}
	if !strings.Contains(result.Stdout, worktreeDir) {
		t.Fatalf("stdout = %q, want GC_WORK_DIR %q", result.Stdout, worktreeDir)
	}
}

// TestRunRalphCheckRejectsRelativeCheckPathWithExternalWorkDir keeps
// metadata-derived external work_dir values from becoming the trusted base for
// relative gc.check_path scripts.
func TestRunRalphCheckRejectsRelativeCheckPathWithExternalWorkDir(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	absoluteWorktreeDir := filepath.Join(parent, "abs-worktree")
	relativeWorktreeDir := filepath.Join(parent, "rel-worktree")
	for _, dir := range []string{cityPath, storePath, absoluteWorktreeDir, relativeWorktreeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, dir := range []string{absoluteWorktreeDir, relativeWorktreeDir} {
		if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\necho \"$GC_WORK_DIR\"\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write worktree script in %s: %v", dir, err)
		}
	}

	cases := []struct {
		name    string
		workDir string
	}{
		{name: "absolute_external", workDir: absoluteWorktreeDir},
		{name: "relative_external", workDir: "../rel-worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			check := beads.Bead{
				ID:   "check-rel-workdir",
				Type: "task",
				Metadata: map[string]string{
					"gc.check_path":    "check.sh",
					"gc.work_dir":      tc.workDir,
					"gc.check_timeout": "30s",
				},
			}
			subject := beads.Bead{ID: "run-rel-workdir", Type: "task"}

			result, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
				CityPath:  cityPath,
				StorePath: storePath,
			})
			if err == nil {
				t.Fatalf("expected external work_dir rejection, got outcome=%q stdout=%q", result.Outcome, result.Stdout)
			}
			if !strings.Contains(err.Error(), "escapes both city and store roots") {
				t.Errorf("expected work_dir containment error, got: %v", err)
			}
		})
	}
}

// TestRunRalphCheckRejectsSymlinkEscapeViaStore pins the symlink half
// of the gastownhall/gascity#2354 review: a script that lives under
// storePath (so the pre-resolution containment check passes) but
// symlinks to a location outside both roots must be rejected by the
// post-EvalSymlinks containment check in convergence.ResolveConditionPath.
func TestRunRalphCheckRejectsSymlinkEscapeViaStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	storePath := filepath.Join(parent, "rig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	scriptDir := filepath.Join(storePath, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	outside := filepath.Join(parent, "outside.sh")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(scriptDir, "check.sh")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	store := beads.NewMemStore()
	check := beads.Bead{
		ID:   "check-symlink-escape",
		Type: "task",
		Metadata: map[string]string{
			"gc.check_path":    "scripts/check.sh",
			"gc.check_timeout": "30s",
		},
	}
	subject := beads.Bead{ID: "run-symlink-escape", Type: "task"}

	_, err := runRalphCheck(store, check, subject, 1, ProcessOptions{
		CityPath:  cityPath,
		StorePath: storePath,
	})
	if err == nil {
		t.Fatal("expected symlink-escape rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlink target outside containment") {
		t.Errorf("expected symlink-escape error, got: %v", err)
	}
}

func writeCheckScript(t *testing.T, cityPath, name, contents string) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, name)
	writeExecutableScript(t, scriptPath, contents)
	return filepath.ToSlash(filepath.Join(".gc", "scripts", name))
}

func writeExecutableScript(t *testing.T, scriptPath, contents string) {
	t.Helper()
	scriptDir := filepath.Dir(scriptPath)
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	tmp, err := os.CreateTemp(scriptDir, "."+filepath.Base(scriptPath)+".tmp-*")
	if err != nil {
		t.Fatalf("create temp script %s: %v", scriptPath, err)
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(contents); err != nil {
		_ = tmp.Close()
		t.Fatalf("write %s: %v", scriptPath, err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close %s: %v", scriptPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", scriptPath, err)
	}
	if err := os.Rename(tmpPath, scriptPath); err != nil {
		t.Fatalf("install %s: %v", scriptPath, err)
	}
	keepTemp = false
}

func newSimpleRalphLoopInStore(t *testing.T, store beads.Store, stepID, checkPath string, maxAttempts int) (beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()

	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "logical",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.step_id":      stepID,
			"gc.max_attempts": strconv.Itoa(maxAttempts),
			"gc.root_bead_id": workflow.ID,
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "run 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "run",
			"gc.step_id":         stepID,
			"gc.ralph_step_id":   stepID,
			"gc.attempt":         "1",
			"gc.step_ref":        stepID + ".run.1",
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	check1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "check 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "check",
			"gc.step_id":         stepID,
			"gc.ralph_step_id":   stepID,
			"gc.attempt":         "1",
			"gc.step_ref":        stepID + ".check.1",
			"gc.check_mode":      "exec",
			"gc.check_path":      checkPath,
			"gc.check_timeout":   "30s",
			"gc.max_attempts":    strconv.Itoa(maxAttempts),
			"gc.root_bead_id":    workflow.ID,
			"gc.logical_bead_id": logical.ID,
		},
	})
	mustDepAdd(t, store, check1.ID, run1.ID, "blocks")
	mustDepAdd(t, store, logical.ID, check1.ID, "blocks")
	return logical, run1, check1
}

func newSimpleRalphLoop(t *testing.T, _stepID, checkPath string, maxAttempts int) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()

	store := beads.NewMemStore()
	logical, run1, check1 := newSimpleRalphLoopInStore(t, store, _stepID, checkPath, maxAttempts)
	return store, logical, run1, check1
}

func nextSimpleAttempt(t *testing.T, store beads.Store, logicalID string) (beads.Bead, beads.Bead) {
	t.Helper()
	logicalDeps, err := store.DepList(logicalID, "down")
	if err != nil {
		t.Fatalf("dep list logical: %v", err)
	}
	if len(logicalDeps) != 1 {
		t.Fatalf("logical deps = %+v, want exactly one current blocker", logicalDeps)
	}
	check2 := mustGetBead(t, store, logicalDeps[0].DependsOnID)
	if check2.Metadata["gc.kind"] != "check" || check2.Metadata["gc.attempt"] != "2" {
		t.Fatalf("check2 metadata = %+v, want check attempt 2", check2.Metadata)
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("ready after retry append: %v", err)
	}
	for _, bead := range ready {
		if bead.Metadata["gc.kind"] == "run" && bead.Metadata["gc.attempt"] == "2" {
			return bead, check2
		}
	}
	t.Fatalf("missing ready run attempt 2; ready=%+v", ready)
	return beads.Bead{}, beads.Bead{}
}

func mustCreateWorkflowBead(t *testing.T, store beads.Store, bead beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("create bead %q: %v", bead.Title, err)
	}
	if bead.Status == "closed" {
		if err := store.Close(created.ID); err != nil {
			t.Fatalf("close bead %q: %v", bead.Title, err)
		}
		created, err = store.Get(created.ID)
		if err != nil {
			t.Fatalf("reload closed bead %q: %v", bead.Title, err)
		}
	}
	for k, v := range bead.Metadata {
		if err := store.SetMetadata(created.ID, k, v); err != nil {
			t.Fatalf("set metadata on %q: %v", bead.Title, err)
		}
	}
	if len(bead.Labels) > 0 {
		if err := store.Update(created.ID, beads.UpdateOpts{Labels: bead.Labels}); err != nil {
			t.Fatalf("add labels to %q: %v", bead.Title, err)
		}
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("reload bead %q: %v", bead.Title, err)
	}
	return created
}

func mustDepAdd(t *testing.T, store beads.Store, issueID, dependsOnID, _depType string) {
	t.Helper()
	if err := store.DepAdd(issueID, dependsOnID, _depType); err != nil {
		t.Fatalf("dep add %s --%s--> %s: %v", issueID, _depType, dependsOnID, err)
	}
}

func mustReadyContains(t *testing.T, store beads.Store, beadID string) bool {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	return beadListContainsID(ready, beadID)
}

func beadListContainsID(list []beads.Bead, beadID string) bool {
	for _, bead := range list {
		if bead.ID == beadID {
			return true
		}
	}
	return false
}

func mustGetBead(t *testing.T, store beads.Store, beadID string) beads.Bead {
	t.Helper()
	bead, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get bead %s: %v", beadID, err)
	}
	return bead
}

func findWorkflowBeadByRef(t *testing.T, store beads.Store, rootID, stepRef string) beads.Bead {
	t.Helper()
	all, err := beads.DirectMembers(store, rootID)
	if err != nil {
		t.Fatalf("list workflow beads: %v", err)
	}
	for _, bead := range all {
		if bead.Metadata["gc.step_ref"] == stepRef {
			return bead
		}
	}
	return beads.Bead{}
}

type ralphPassOrderStore struct {
	*beads.MemStore
	logicalID string
	checkID   string
}

func (s *ralphPassOrderStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" && id == s.logicalID {
		check, err := s.Get(s.checkID)
		if err != nil {
			return err
		}
		if check.Status != "closed" {
			return fmt.Errorf("logical bead %s closed before check bead %s", s.logicalID, s.checkID)
		}
	}
	return s.MemStore.Update(id, opts)
}

type assigneeVisibilityOnCreateStore struct {
	*beads.MemStore
	visibleOnCreate []string
}

func (s *assigneeVisibilityOnCreateStore) Create(bead beads.Bead) (beads.Bead, error) {
	created, err := s.MemStore.Create(bead)
	if err != nil {
		return created, err
	}
	if created.Metadata["gc.attempt"] != "2" || bead.Assignee == "" {
		return created, nil
	}
	assigned, err := s.ListByAssignee(bead.Assignee, "open", 0)
	if err != nil {
		return created, err
	}
	if beadListContainsID(assigned, created.ID) {
		s.visibleOnCreate = append(s.visibleOnCreate, created.ID)
	}
	return created, nil
}

// --- Metadata propagation regression tests ---

// Regression: retry control must propagate non-gc metadata from its
// successful attempt to itself (compositional bubbling).
func TestRetryControlPropagatesAttemptMetadata(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	retryControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review codex",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     "wf-1",
			"gc.step_ref":         "demo.review-codex",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": "{}",
			"gc.control_epoch":    "1",
		},
	})
	attempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review codex attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":    "wf-1",
			"gc.step_ref":        "demo.review-codex.attempt.1",
			"gc.logical_bead_id": retryControl.ID,
			"gc.attempt":         "1",
			"gc.outcome":         "pass",
			"review.verdict":     "done",
			"review.summary":     "LGTM",
		},
	})
	mustDepAdd(t, store, retryControl.ID, attempt.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, retryControl.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry): %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("action = %q, want pass", result.Action)
	}

	after := mustGetBead(t, store, retryControl.ID)
	if after.Status != "closed" {
		t.Fatalf("retry status = %q, want closed", after.Status)
	}
	if after.Metadata["review.verdict"] != "done" {
		t.Errorf("review.verdict = %q, want done (propagated from attempt)", after.Metadata["review.verdict"])
	}
	if after.Metadata["review.summary"] != "LGTM" {
		t.Errorf("review.summary = %q, want LGTM (propagated from attempt)", after.Metadata["review.summary"])
	}
}

// Regression: scope body must propagate non-gc metadata from its closed
// members when the scope completes with pass.
func TestScopeBodyPropagatesMemberMetadata(t *testing.T) {
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
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
			"review.verdict":  "done",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("action = %q, want scope-pass", result.Action)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if bodyAfter.Metadata["review.verdict"] != "done" {
		t.Errorf("body review.verdict = %q, want done (propagated from member)", bodyAfter.Metadata["review.verdict"])
	}
}

// Regression: full compositional chain — attempt → retry → scope → ralph.
// The review.verdict set on a deeply nested attempt must be visible on the
// ralph control bead before the check script runs.
func TestFullMetadataPropagationChain(t *testing.T) {
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

	// Scope body for the iteration.
	iterBody := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "iteration 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "scope",
			"gc.scope_role":      "body",
			"gc.root_bead_id":    workflow.ID,
			"gc.step_ref":        "review-loop.iteration.1",
			"gc.logical_bead_id": "ralph-1",
			"gc.attempt":         "1",
		},
	})

	// Apply-fixes retry — closed with pass, has review.verdict.
	retryControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply-fixes",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
			"review.verdict":  "done",
		},
	})

	// Scope-check for the iteration.
	scopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize iteration",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "control",
		},
	})

	mustDepAdd(t, store, scopeCheck.ID, retryControl.ID, "blocks")
	mustDepAdd(t, store, iterBody.ID, scopeCheck.ID, "blocks")

	// Process scope-check — should propagate review.verdict to iteration body.
	result, err := ProcessControl(store, mustGetBead(t, store, scopeCheck.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("scope-check action = %q, want scope-pass", result.Action)
	}

	iterBodyAfter := mustGetBead(t, store, iterBody.ID)
	if iterBodyAfter.Status != "closed" {
		t.Fatalf("iteration body status = %q, want closed", iterBodyAfter.Status)
	}
	if iterBodyAfter.Metadata["review.verdict"] != "done" {
		t.Fatalf("iteration body review.verdict = %q, want done (propagated from retry member)", iterBodyAfter.Metadata["review.verdict"])
	}
}

func TestProcessScopeCheckIgnoresOpenSpecBeadsWhenCompletingScope(t *testing.T) {
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
		Title: "iteration 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "review-loop.iteration.1",
		},
	})
	mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Step spec for apply",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
			"gc.step_ref":     "review-loop.iteration.1.apply.spec",
		},
	})
	member := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	scopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize apply",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, scopeCheck.ID, member.ID, "blocks")
	mustDepAdd(t, store, body.ID, scopeCheck.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, scopeCheck.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "scope-pass" {
		t.Fatalf("scope-check action = %q, want scope-pass", result.Action)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("scope body = status %q outcome %q, want closed/pass", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
}

func TestProcessScopeCheckDoesNotSkipOpenSpecBeadsWhenFailingScope(t *testing.T) {
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
		Title: "iteration 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "review-loop.iteration.1",
		},
	})
	spec := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Step spec for apply",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
			"gc.step_ref":     "review-loop.iteration.1.apply.spec",
		},
	})
	openMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
		},
	})
	failedMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	scopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "review-loop.iteration.1",
			"gc.scope_role":   "control",
		},
	})
	mustDepAdd(t, store, scopeCheck.ID, failedMember.ID, "blocks")
	mustDepAdd(t, store, body.ID, scopeCheck.ID, "blocks")

	result, err := ProcessControl(store, mustGetBead(t, store, scopeCheck.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "scope-fail" {
		t.Fatalf("scope-check action = %q, want scope-fail", result.Action)
	}
	if result.Skipped != 1 {
		t.Fatalf("scope-check skipped = %d, want 1 non-spec member", result.Skipped)
	}

	specAfter := mustGetBead(t, store, spec.ID)
	if specAfter.Status != "open" {
		t.Fatalf("spec status = %q, want open", specAfter.Status)
	}
	openMemberAfter := mustGetBead(t, store, openMember.ID)
	if openMemberAfter.Status != "closed" || openMemberAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("open member = status %q outcome %q, want closed/skipped", openMemberAfter.Status, openMemberAfter.Metadata["gc.outcome"])
	}
	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
}

// TestProcessControlEmitsSkipReasonWhenNotOpen is the regression guard for
// the 20-minute silent stall on ga-ttn5z. When a rogue worker had flipped
// a retry-control bead (ga-fw2fm) to status=in_progress, ProcessControl
// returned {Processed: false} at the very first guard without any trace
// output. The serve loop upstream traced "serve processed" either way, so
// nothing in the dispatcher log revealed why the workflow wasn't moving.
// The fix emits a specific "process-control ... skip reason=bead_not_open"
// line before the early return.
func TestProcessControlEmitsSkipReasonWhenNotOpen(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "rogue in_progress control",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.max_attempts": "3",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(control.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	control, err = store.Get(control.ID)
	if err != nil {
		t.Fatalf("reload control: %v", err)
	}

	var traceBuf bytes.Buffer
	opts := ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&traceBuf, format, args...)
			traceBuf.WriteByte('\n')
		},
	}

	result, err := ProcessControl(store, control, opts)
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Processed {
		t.Fatalf("result.Processed = true, want false when bead is not open")
	}

	traced := traceBuf.String()
	if !strings.Contains(traced, "skip reason=bead_not_open") {
		t.Fatalf("trace missing skip reason; got:\n%s", traced)
	}
	if !strings.Contains(traced, control.ID) {
		t.Fatalf("trace missing control ID %q; got:\n%s", control.ID, traced)
	}
	if !strings.Contains(traced, "status=in_progress") {
		t.Fatalf("trace missing the actual status; got:\n%s", traced)
	}
}

func TestProcessControlClosesControlWhenWorkflowRootMissing(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "orphaned retry control",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":           "retry",
			"gc.max_attempts":   "3",
			"gc.root_bead_id":   "missing-root",
			"gc.root_store_ref": "rig:gascity",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	var traceBuf bytes.Buffer
	opts := ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&traceBuf, format, args...)
			traceBuf.WriteByte('\n')
		},
	}

	result, err := ProcessControl(store, control, opts)
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "orphaned-workflow" {
		t.Fatalf("result = %+v, want processed orphaned-workflow", result)
	}
	after := mustGetBead(t, store, control.ID)
	if after.Status != "closed" {
		t.Fatalf("status = %q, want closed", after.Status)
	}
	if after.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("gc.outcome = %q, want fail", after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.failure_reason"] != "missing_workflow_root" {
		t.Fatalf("gc.failure_reason = %q, want missing_workflow_root", after.Metadata["gc.failure_reason"])
	}
	if after.Metadata["gc.final_disposition"] != "orphaned_workflow" {
		t.Fatalf("gc.final_disposition = %q, want orphaned_workflow", after.Metadata["gc.final_disposition"])
	}
	if after.Metadata["gc.missing_root_bead_id"] != "missing-root" {
		t.Fatalf("gc.missing_root_bead_id = %q, want missing-root", after.Metadata["gc.missing_root_bead_id"])
	}
	traced := traceBuf.String()
	if !strings.Contains(traced, "close reason=missing_workflow_root") {
		t.Fatalf("trace missing missing-root close reason; got:\n%s", traced)
	}
}

// TestProcessControlClosesControlWhenWorkflowRootCanceled pins the run-cancel
// gate: a control bead whose workflow root is closed with gc.outcome=canceled is
// closed as canceled instead of being spawned/continued, so POST /runs/{id}/cancel
// converges to stopped rather than racing the dispatcher. The gate must record a
// cancellation, NOT a failure.
func TestProcessControlClosesControlWhenWorkflowRootCanceled(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:    "canceled run root",
		Type:     "molecule",
		Status:   "open",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	// Close the root as canceled, exactly as run cancel does (outcome + intent).
	if _, err := store.CloseAll([]string{root.ID}, map[string]string{
		"gc.outcome":          "canceled",
		"gc.cancel_requested": "true",
	}); err != nil {
		t.Fatalf("close root canceled: %v", err)
	}
	control, err := store.Create(beads.Bead{
		Title:  "fanout under canceled root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":           "fanout",
			"gc.root_bead_id":   root.ID,
			"gc.root_store_ref": "rig:gascity",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	var traceBuf bytes.Buffer
	opts := ProcessOptions{
		Tracef: func(format string, args ...any) {
			fmt.Fprintf(&traceBuf, format, args...)
			traceBuf.WriteByte('\n')
		},
	}

	result, err := ProcessControl(store, control, opts)
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "canceled-workflow" {
		t.Fatalf("result = %+v, want processed canceled-workflow", result)
	}
	after := mustGetBead(t, store, control.ID)
	if after.Status != "closed" {
		t.Fatalf("status = %q, want closed", after.Status)
	}
	if after.Metadata["gc.outcome"] != "canceled" {
		t.Fatalf("gc.outcome = %q, want canceled", after.Metadata["gc.outcome"])
	}
	// A cancellation must not be recorded as a failure.
	if got := after.Metadata["gc.failure_reason"]; got != "" {
		t.Fatalf("gc.failure_reason = %q, want empty (cancellation is not a failure)", got)
	}
	if got := after.Metadata["gc.failure_class"]; got != "" {
		t.Fatalf("gc.failure_class = %q, want empty (cancellation is not a failure)", got)
	}
	traced := traceBuf.String()
	if !strings.Contains(traced, "close reason=root_canceled") {
		t.Fatalf("trace missing root_canceled close reason; got:\n%s", traced)
	}
}

// TestProcessWorkflowFinalize_PurgesMoleculeArtifactDir verifies that
// when a workflow finalizes, the molecule-scoped artifact directory is
// removed so disk does not leak and a successor run with the same root
// ID gets a clean slate.
func TestProcessWorkflowFinalize_PurgesMoleculeArtifactDir(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "finalize",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	// Simulate a polecat writing artifacts during the workflow.
	artifactDir := filepath.Join(cityPath, ".gc", "molecules", workflow.ID, "artifacts", "step-1")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "iteration-1.json"), []byte(`{"round":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ProcessControl(store, finalizer, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed {
		t.Fatalf("result = %+v, want processed", result)
	}

	// The molecule root directory should no longer exist.
	moleculeDir := molecule.Dir(cityPath, workflow.ID)
	if _, err := os.Stat(moleculeDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("molecule dir %q still exists after finalize (stat err = %v)", moleculeDir, err)
	}
}

// TestProcessWorkflowFinalize_NoCityPath verifies the purge is a no-op
// when CityPath is not provided (tests, legacy call sites). The finalize
// should succeed without touching any filesystem.
func TestProcessWorkflowFinalize_NoCityPath(t *testing.T) {
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
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "finalize",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	// CityPath omitted → purge is skipped.
	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed {
		t.Fatalf("result = %+v, want processed", result)
	}
}

// TestProcessWorkflowFinalize_PurgeOnMissingDir verifies that finalize
// succeeds even when the molecule artifact directory was never created
// (e.g., a workflow that ran but wrote no artifacts). molecule.RemoveDir
// returns nil on missing dirs, so the best-effort purge is a no-op.
// This exercises the crash-recovery precondition: RemoveDir must never
// surface a fatal error just because the tree it's trying to remove is
// already gone.
func TestProcessWorkflowFinalize_PurgeOnMissingDir(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	// Molecule dir never existed. Finalize must not surface an error.
	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "finalize",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed {
		t.Fatalf("result = %+v, want processed", result)
	}
}

// TestCloseScopeAsPassedSharedConvergence exercises the single scope-close
// helper that both processScopeCheck branches and reconcileTerminalScopedMember
// now share: propagate non-gc.* member metadata onto the body, write the
// resolved output_json, and close the body pass unless it is already closed.
func TestCloseScopeAsPassedSharedConvergence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		bodyStatus     string
		wantCloseTrace bool
	}{
		{name: "open body closes pass", bodyStatus: "open", wantCloseTrace: true},
		{name: "already closed body is not re-closed", bodyStatus: "closed", wantCloseTrace: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			workflow := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:    "workflow",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			})
			body := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "body",
				Type:   "task",
				Status: tc.bodyStatus,
				Metadata: map[string]string{
					"gc.kind":         "scope",
					"gc.scope_role":   "body",
					"gc.root_bead_id": workflow.ID,
					"gc.scope_ref":    "body",
					"gc.step_ref":     "demo.body",
				},
			})
			subject := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "member",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					"gc.root_bead_id": workflow.ID,
					"gc.scope_ref":    "body",
					"gc.scope_role":   "member",
					"gc.outcome":      "pass",
					"gc.output_json":  `{"verdict":"approved"}`,
					"review.verdict":  "done",
				},
			})

			snapshot, err := loadScopeSnapshotWithBody(store, workflow.ID, "body", body)
			if err != nil {
				t.Fatalf("loadScopeSnapshotWithBody: %v", err)
			}
			var trace bytes.Buffer
			opts := ProcessOptions{Tracef: func(format string, args ...any) {
				fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
			}}
			if err := closeScopeAsPassed(store, snapshot, subject, opts, subject.ID); err != nil {
				t.Fatalf("closeScopeAsPassed: %v", err)
			}

			bodyAfter := mustGetBead(t, store, body.ID)
			if bodyAfter.Status != "closed" {
				t.Fatalf("body status = %q, want closed", bodyAfter.Status)
			}
			// Member metadata and output_json bubble onto the body regardless of
			// whether the body was already closed.
			if got := bodyAfter.Metadata["review.verdict"]; got != "done" {
				t.Fatalf("body review.verdict = %q, want done", got)
			}
			if got := bodyAfter.Metadata["gc.output_json"]; got != `{"verdict":"approved"}` {
				t.Fatalf("body gc.output_json = %q, want approved payload", got)
			}
			if tc.wantCloseTrace {
				if got := bodyAfter.Metadata["gc.outcome"]; got != "pass" {
					t.Fatalf("body outcome = %q, want pass", got)
				}
			}

			traceText := trace.String()
			for _, want := range []string{"phase=propagate-metadata", "phase=resolve-output", "phase=write-output", "phase=reload-body"} {
				if !strings.Contains(traceText, want) {
					t.Fatalf("trace missing %q:\n%s", want, traceText)
				}
			}
			gotClose := strings.Contains(traceText, "phase=close-body ")
			if gotClose != tc.wantCloseTrace {
				t.Fatalf("close-body trace present=%v, want %v:\n%s", gotClose, tc.wantCloseTrace, traceText)
			}
		})
	}
}

// TestAbortScopeSharedConvergence exercises the single scope-abort helper shared
// by processScopeCheck and reconcileTerminalScopedMember: skip still-open
// members, propagate closed-member metadata onto the body, and close the body
// fail.
func TestAbortScopeSharedConvergence(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "failed member",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
			"review.verdict":  "blocked",
		},
	})
	openMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "open member",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})

	snapshot, err := loadScopeSnapshotWithBody(store, workflow.ID, "body", body)
	if err != nil {
		t.Fatalf("loadScopeSnapshotWithBody: %v", err)
	}
	var trace bytes.Buffer
	opts := ProcessOptions{Tracef: func(format string, args ...any) {
		fmt.Fprintf(&trace, format+"\n", args...) //nolint:errcheck // test buffer
	}}
	skipped, err := abortScope(store, snapshot, opts, failed.ID)
	if err != nil {
		t.Fatalf("abortScope: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}

	bodyAfter := mustGetBead(t, store, body.ID)
	if bodyAfter.Status != "closed" || bodyAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("body = status %q outcome %q, want closed/fail", bodyAfter.Status, bodyAfter.Metadata["gc.outcome"])
	}
	if got := bodyAfter.Metadata["review.verdict"]; got != "blocked" {
		t.Fatalf("body review.verdict = %q, want blocked", got)
	}
	openAfter := mustGetBead(t, store, openMember.ID)
	if openAfter.Status != "closed" || openAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("open member = status %q outcome %q, want closed/skipped", openAfter.Status, openAfter.Metadata["gc.outcome"])
	}

	traceText := trace.String()
	for _, want := range []string{"phase=skip-open-members", "phase=propagate-metadata", "phase=close-body-fail"} {
		if !strings.Contains(traceText, want) {
			t.Fatalf("trace missing %q:\n%s", want, traceText)
		}
	}
}

// Teardown-scoped work is post-settlement by contract: a teardown step's pass
// condition may (and for worktree cleanup does) branch on the root outcome that
// only workflow-finalize produces. These tests pin both halves of that contract
// (ga-99u0u): finalize does not wait for the teardown tail, and finalize's
// terminal sweep does not skip-close it either.

// TestProcessWorkflowFinalizeSettlesWithOpenTeardownTail pins the runtime half:
// settlement proceeds with an open teardown control + attempt, the tail survives
// the terminal sweep, and a non-teardown straggler is still swept.
func TestProcessWorkflowFinalizeSettlesWithOpenTeardownTail(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Body",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindScope,
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	teardownControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Clean up the worktree (retry)",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindRetry,
			beadmeta.OriginalKindMetadataKey: beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey:   workflow.ID,
			beadmeta.ScopeRefMetadataKey:     "body",
			beadmeta.ScopeRoleMetadataKey:    beadmeta.ScopeRoleTeardown,
			beadmeta.StepIDMetadataKey:       "cleanup-worktree",
			beadmeta.MaxAttemptsMetadataKey:  "3",
			beadmeta.OnExhaustedMetadataKey:  "hard_fail",
		},
	})
	teardownAttempt := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Clean up the worktree",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindCleanup,
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
			beadmeta.StepIDMetadataKey:     "cleanup-worktree",
			beadmeta.ControlForMetadataKey: "cleanup-worktree",
			beadmeta.AttemptMetadataKey:    "1",
		},
	})
	straggler := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "partially materialized step that can no longer execute",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
			beadmeta.StepRefMetadataKey:    "unused",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, body.ID, "blocks")
	mustDepAdd(t, store, teardownControl.ID, teardownAttempt.ID, "blocks")

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("workflow result = %+v, want processed workflow-pass", result)
	}

	rootAfter := mustGetBead(t, store, workflow.ID)
	if rootAfter.Status != "closed" || rootAfter.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("root status/outcome = %q/%q, want closed/pass",
			rootAfter.Status, rootAfter.Metadata[beadmeta.OutcomeMetadataKey])
	}
	for _, id := range []string{teardownControl.ID, teardownAttempt.ID} {
		after := mustGetBead(t, store, id)
		if after.Status != "open" {
			t.Fatalf("teardown member %s status = %q (outcome %q), want open: the real cleanup must still run after settlement",
				id, after.Status, after.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
	// Control for the exemption predicate: the sweep still repairs partial
	// materializations, so a non-teardown straggler is closed skipped.
	stragglerAfter := mustGetBead(t, store, straggler.ID)
	if stragglerAfter.Status != "closed" {
		t.Fatalf("non-teardown straggler status = %q, want closed", stragglerAfter.Status)
	}
	if got := stragglerAfter.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeSkipped {
		t.Fatalf("non-teardown straggler gc.outcome = %q, want skipped", got)
	}
}

// TestProcessWorkflowFinalizePendsOnOpenNonTeardownBlocker is the control for
// the sink change: settlement still gates on real work.
func TestProcessWorkflowFinalizePendsOnOpenNonTeardownBlocker(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	})
	work := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "Work",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: workflow.ID},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
		},
	})
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, work.ID, "blocks")

	if _, err := ProcessControl(store, finalizer, ProcessOptions{}); !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(workflow-finalize) err = %v, want ErrControlPending", err)
	}
	if after := mustGetBead(t, store, workflow.ID); after.Status != "open" {
		t.Fatalf("root status = %q, want open while a non-teardown blocker is open", after.Status)
	}
}

const teardownSettlementFormula = `
formula = "teardown-settlement"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"

[[steps]]
id = "body"
title = "Body"
needs = ["work"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "worktree" }

[[steps]]
id = "cleanup-worktree"
title = "Clean up the worktree"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
`

// cookTeardownMolecule materializes the teardown-settlement formula into store
// exactly as the real materializer would (compile → instantiate) and returns the
// workflow root ID.
func cookTeardownMolecule(t *testing.T, store beads.Store) string {
	t.Helper()
	formulatest.EnableV2ForTest(t)
	prevGraphApply := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prevGraphApply) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "teardown-settlement.toml"), []byte(teardownSettlementFormula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	result, err := molecule.Cook(context.Background(), store, "teardown-settlement", []string{dir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	if result.RootID == "" {
		t.Fatal("Cook returned no root bead")
	}
	return result.RootID
}

// runMolecule drives a materialized molecule to quiescence with a fake worker
// and the control dispatcher, mirroring how a city actually converges: every
// ready control bead is served by ProcessControl, and every ready work bead is
// closed by the worker.
//
// Controls are served before work in each round because that is the real
// timing: the dispatcher ticks in seconds while an agent takes minutes on a
// work bead, so the state a worker observes is the state after the dispatcher
// has drained everything ready.
//
// The worker reproduces the pack's premature-teardown guard: a cleanup attempt
// picked up before the root has settled closes transient ("root_not_closed_yet")
// because the run outcome it must branch on does not exist yet. Non-teardown
// work closes with workOutcome.
func runMolecule(t *testing.T, store beads.Store, rootID, workOutcome string) {
	t.Helper()
	const maxRounds = 60
	for round := 0; round < maxRounds; round++ {
		members, err := molecule.ListSubtree(store, rootID)
		if err != nil {
			t.Fatalf("listing molecule members: %v", err)
		}
		slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
		teardownSteps := teardownStepIDs(members)

		progressed := false
		for _, servingControls := range []bool{true, false} {
			for _, member := range members {
				current := mustGetBead(t, store, member.ID)
				if current.Status != "open" || current.ID == rootID {
					continue
				}
				kind := current.Metadata[beadmeta.KindMetadataKey]
				if kind == beadmeta.KindSpec || kind == beadmeta.KindScope {
					// Sidecars and scope latches are closed by the runtime,
					// never by a worker.
					continue
				}
				if beadmeta.IsControlKind(kind) != servingControls {
					continue
				}
				if !allBlockersClosed(t, store, current.ID) {
					continue
				}
				if servingControls {
					_, err := ProcessControl(store, current, ProcessOptions{})
					if errors.Is(err, ErrControlPending) {
						continue
					}
					if err != nil {
						t.Fatalf("ProcessControl(%s kind=%s): %v", current.ID, kind, err)
					}
					progressed = true
					continue
				}
				closeAsWorker(t, store, current, rootID, workOutcome, teardownSteps)
				progressed = true
			}
			if progressed && servingControls {
				// A served control can make more controls ready; re-observe
				// before handing anything to a worker.
				break
			}
		}
		if !progressed {
			return
		}
	}
	t.Fatalf("molecule %s never reached quiescence:\n%s", rootID, memberReport(t, store, rootID))
}

func allBlockersClosed(t *testing.T, store beads.Store, beadID string) bool {
	t.Helper()
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		t.Fatalf("dep list %s: %v", beadID, err)
	}
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		if mustGetBead(t, store, dep.DependsOnID).Status != "closed" {
			return false
		}
	}
	return true
}

func closeAsWorker(t *testing.T, store beads.Store, bead beads.Bead, rootID, workOutcome string, teardownSteps map[string]bool) {
	t.Helper()
	metadata := map[string]string{beadmeta.OutcomeMetadataKey: workOutcome}
	if workOutcome == beadmeta.OutcomeFail {
		metadata[beadmeta.FailureClassMetadataKey] = beadmeta.FailureClassHard
		metadata[beadmeta.FailureReasonMetadataKey] = "work_failed"
	}
	if isTeardownAttempt(bead, teardownSteps) {
		if mustGetBead(t, store, rootID).Status == "closed" {
			// The root outcome exists: the real cleanup can branch on it.
			metadata = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}
		} else {
			metadata = map[string]string{
				beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
				beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassTransient,
				beadmeta.FailureReasonMetadataKey: "root_not_closed_yet",
			}
		}
	}
	if err := updateMetadataAndClose(store, bead.ID, metadata); err != nil {
		t.Fatalf("worker close %s: %v", bead.ID, err)
	}
}

// teardownStepIDs collects the gc.step_id of every teardown-scoped member. The
// same step runs for every attempt, so this is what identifies an attempt as
// teardown work — the attempt beads themselves are inconsistent about it
// (attempt 1 has gc.scope_role stripped by retry expansion; re-spawned attempts
// inherit it from the frozen step spec).
func teardownStepIDs(members []beads.Bead) map[string]bool {
	out := make(map[string]bool)
	for _, member := range members {
		if member.Metadata[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleTeardown {
			continue
		}
		if stepID := member.Metadata[beadmeta.StepIDMetadataKey]; stepID != "" {
			out[stepID] = true
		}
	}
	return out
}

func isTeardownAttempt(bead beads.Bead, teardownSteps map[string]bool) bool {
	if bead.Metadata[beadmeta.AttemptMetadataKey] == "" {
		return false
	}
	return teardownSteps[bead.Metadata[beadmeta.StepIDMetadataKey]]
}

func memberReport(t *testing.T, store beads.Store, rootID string) string {
	t.Helper()
	members, err := molecule.ListSubtree(store, rootID)
	if err != nil {
		t.Fatalf("listing molecule members: %v", err)
	}
	slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
	lines := make([]string, 0, len(members))
	for _, member := range members {
		lines = append(lines, strings.Join([]string{
			"  " + member.ID,
			member.Status,
			"kind=" + member.Metadata[beadmeta.KindMetadataKey],
			"step_ref=" + member.Metadata[beadmeta.StepRefMetadataKey],
			"outcome=" + member.Metadata[beadmeta.OutcomeMetadataKey],
		}, " "))
	}
	return strings.Join(lines, "\n")
}

// teardownTailBeads returns the teardown retry control and its single attempt.
// Exactly one attempt is the assertion: a second attempt means the cleanup was
// dispatched before the root settled and burned a transient retry on the
// root-outcome guard.
func teardownTailBeads(t *testing.T, store beads.Store, rootID string) (control, attempt beads.Bead) {
	t.Helper()
	members, err := molecule.ListSubtree(store, rootID)
	if err != nil {
		t.Fatalf("listing molecule members: %v", err)
	}
	teardownSteps := teardownStepIDs(members)
	attempts := make([]beads.Bead, 0, 1)
	for _, member := range members {
		switch {
		case member.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRetry &&
			member.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown:
			control = member
		case isTeardownAttempt(member, teardownSteps):
			attempts = append(attempts, member)
		}
	}
	if control.ID == "" {
		t.Fatalf("no teardown retry control in molecule:\n%s", memberReport(t, store, rootID))
	}
	if len(attempts) != 1 {
		t.Fatalf("teardown attempts = %d, want exactly 1 (extra attempts mean the cleanup ran before settlement):\n%s",
			len(attempts), memberReport(t, store, rootID))
	}
	return control, attempts[0]
}

func assertNoOpenMembers(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	members, err := molecule.ListSubtree(store, rootID)
	if err != nil {
		t.Fatalf("listing molecule members: %v", err)
	}
	for _, member := range members {
		if member.Status == "open" {
			t.Fatalf("molecule %s still has open members after convergence:\n%s", rootID, memberReport(t, store, rootID))
		}
	}
}

// TestTeardownTailClosesItselfAfterSettlement is the end-to-end control: a
// molecule materialized from a formula with a retry-managed teardown step
// converges to zero open members with no hand-close. Settlement happens first
// (the teardown tail is not a finalize blocker), the real cleanup then passes
// its root-outcome guard, and the retry control closes itself.
//
// It also pins the outcome-poisoning regression: before the fix, the teardown
// control was a finalize sink, so its attempts exhausted transient
// "root_not_closed_yet" and hard-failed, flipping a green run to fail.
func TestTeardownTailClosesItselfAfterSettlement(t *testing.T) {
	store := beads.NewMemStore()
	rootID := cookTeardownMolecule(t, store)

	runMolecule(t, store, rootID, beadmeta.OutcomePass)

	root := mustGetBead(t, store, rootID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed:\n%s", root.Status, memberReport(t, store, rootID))
	}
	if got := root.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("root gc.outcome = %q, want pass — a teardown control must not grade the run:\n%s",
			got, memberReport(t, store, rootID))
	}

	control, attempt := teardownTailBeads(t, store, rootID)
	control = mustGetBead(t, store, control.ID)
	attempt = mustGetBead(t, store, attempt.ID)
	if got := attempt.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("teardown attempt %s gc.outcome = %q, want pass (it must run, not be skip-closed)", attempt.ID, got)
	}
	if got := attempt.Metadata["close_reason"]; got == sourceworkflow.WorkflowSkippedCloseReason {
		t.Fatalf("teardown attempt %s was swept by the terminal close, not executed", attempt.ID)
	}
	if got := control.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("teardown control %s gc.outcome = %q, want pass (the dispatcher must serve it after settlement)", control.ID, got)
	}
	if got := control.Metadata["close_reason"]; got == sourceworkflow.WorkflowSkippedCloseReason {
		t.Fatalf("teardown control %s was swept by the terminal close, not served", control.ID)
	}
	assertNoOpenMembers(t, store, rootID)
}

// TestTeardownTailClosesItselfAfterFailedSettlement pins the fail branch: the
// run settles fail, the teardown still runs afterwards (that is how
// preserve-the-worktree-on-failure becomes reachable), and its own outcome
// never re-grades the root.
func TestTeardownTailClosesItselfAfterFailedSettlement(t *testing.T) {
	store := beads.NewMemStore()
	rootID := cookTeardownMolecule(t, store)

	runMolecule(t, store, rootID, beadmeta.OutcomeFail)

	root := mustGetBead(t, store, rootID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed:\n%s", root.Status, memberReport(t, store, rootID))
	}
	if got := root.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeFail {
		t.Fatalf("root gc.outcome = %q, want fail:\n%s", got, memberReport(t, store, rootID))
	}

	control, attempt := teardownTailBeads(t, store, rootID)
	if got := mustGetBead(t, store, attempt.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("teardown attempt %s gc.outcome = %q, want pass after a failed settlement", attempt.ID, got)
	}
	if got := mustGetBead(t, store, control.ID).Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomePass {
		t.Fatalf("teardown control %s gc.outcome = %q, want pass after a failed settlement", control.ID, got)
	}
	assertNoOpenMembers(t, store, rootID)
}
