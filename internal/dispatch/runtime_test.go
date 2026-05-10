package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, cleanup.ID, body.ID, "blocks")
	mustDepAdd(t, store, futureMember.ID, control.ID, "blocks")
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
	memberDeps, err := store.DepList(futureMember.ID, "down")
	if err != nil {
		t.Fatalf("dep list future member: %v", err)
	}
	if len(memberDeps) != 1 || memberDeps[0].DependsOnID != control.ID || memberDeps[0].Type != "blocks" {
		t.Fatalf("future member deps = %+v, want preserved block on %s", memberDeps, control.ID)
	}

	cleanupReady := mustReadyContains(t, store, cleanup.ID)
	if !cleanupReady {
		t.Fatalf("cleanup %s should be ready after body fails closed", cleanup.ID)
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

func TestProcessScopeCheckReturnsPendingWhenScopeBodyMissing(t *testing.T) {
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
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl(scope-check missing body) err = %v, want %v", err, ErrControlPending)
	}

	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open", controlAfter.Status)
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
}

type countingListStore struct {
	*beads.MemStore
	listCalls int
	queries   []beads.ListQuery
}

type workflowFinalizeCloseFailStore struct {
	beads.Store
	finalizerID string
}

func (s *countingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	s.queries = append(s.queries, query)
	return s.MemStore.List(query)
}

func (s *workflowFinalizeCloseFailStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.finalizerID && opts.Status != nil && *opts.Status == "closed" {
		return errors.New("finalizer close failed")
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

func (s *strictCloseStore) Close(id string) error {
	deps, err := s.DepList(id, "down")
	if err != nil {
		return err
	}
	var openBlockers []string
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := s.Get(dep.DependsOnID)
		if err != nil {
			return err
		}
		if blocker.Status == "open" {
			openBlockers = append(openBlockers, blocker.ID)
		}
	}
	if len(openBlockers) > 0 {
		return fmt.Errorf("cannot close %s: blocked by open issues %v", id, openBlockers)
	}
	return s.MemStore.Close(id)
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

func TestProcessWorkflowFinalizeOrphanedRootClosesFinalizerWithoutError(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": "missing-root-id",
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

type updateErrorStore struct {
	beads.Store
	failID string
	err    error
}

func (s updateErrorStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID {
		return s.err
	}
	return s.Store.Update(id, opts)
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
	rigStore := updateErrorStore{Store: f.rigStore, failID: f.workflow.ID, err: rootCloseErr}
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

	checkResult, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
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
		Metadata: map[string]string{"gc.routed_to": "polecat"},
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
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.bond_vars":    `{"reviewer":"{item.name}"}`,
			"gc.fanout_mode":  "parallel",
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
	if got := logical.Assignee; got != "gascity--control-dispatcher" {
		t.Fatalf("logical retry assignee = %q, want gascity--control-dispatcher", got)
	}
	if got := logical.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("logical retry gc.routed_to = %q, want empty direct dispatcher assignee", got)
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
			"gc.kind":         "fanout",
			"gc.root_bead_id": workflow.ID,
			"gc.control_for":  "demo.survey",
			"gc.routed_to":    "gascity/control-dispatcher",
			"gc.for_each":     "output.items",
			"gc.bond":         "expansion-review",
			"gc.fanout_mode":  "parallel",
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
					delete(step.Metadata, "gc.routed_to")
					step.Assignee = "gascity--control-dispatcher"
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
	if got := retryControl.Assignee; got != "gascity--control-dispatcher" {
		t.Fatalf("retry control assignee = %q, want gascity--control-dispatcher", got)
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
	if got := scopeCheck.Assignee; got != "gascity--control-dispatcher" {
		t.Fatalf("scope-check assignee = %q, want gascity--control-dispatcher", got)
	}
	if got := scopeCheck.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("scope-check gc.routed_to = %q, want empty direct dispatcher assignee", got)
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

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
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

	result, err := ProcessControl(store, fanout, ProcessOptions{FormulaSearchPaths: []string{dir}})
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
	routeFanoutFragmentSteps(fragment, fanout, ProcessOptions{CityPath: dir}, store)
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
	if err := os.WriteFile(checkPath, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

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
	storePath := t.TempDir()
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
	if err := os.WriteFile(checkPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write check script: %v", err)
	}

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

func writeCheckScript(t *testing.T, cityPath, name, contents string) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, name)
	if err := os.WriteFile(scriptPath, []byte(contents), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return filepath.ToSlash(filepath.Join(".gc", "scripts", name))
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
	all, err := listByWorkflowRoot(store, rootID)
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
