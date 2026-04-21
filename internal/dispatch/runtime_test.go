package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
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
	if store.listCalls != 1 {
		t.Fatalf("List calls = %d, want 1 workflow snapshot", store.listCalls)
	}
	if len(store.queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(store.queries))
	}
	if got := store.queries[0].Metadata["gc.root_bead_id"]; got != workflow.ID {
		t.Fatalf("root metadata query = %q, want %q", got, workflow.ID)
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

func (s *countingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	s.queries = append(s.queries, query)
	return s.MemStore.List(query)
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
	if _, err := appendRalphRetry(store, logical.ID, run1, check1, 2, cityPath); err != nil {
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

	mapping, err := appendRalphRetry(inspect, logical.ID, run1, check1, 2, "")
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

	mapping, err := appendRalphRetry(store, logical.ID, run1, check1, 2, cityPath)
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

	mapping, err := appendRalphRetry(store, logical.ID, run1, check1, 2, "")
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

	got, err := resolveWorkflowStepByRefFromBeads([]beads.Bead{suffix, exact}, "wf-1", "demo.survey")
	if err != nil {
		t.Fatalf("resolveWorkflowStepByRefFromBeads: %v", err)
	}
	if got.ID != exact.ID {
		t.Fatalf("matched bead %s, want exact match %s", got.ID, exact.ID)
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
