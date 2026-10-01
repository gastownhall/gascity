package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// failHaltsCity writes a city.toml with [workflows] fail_halts set as given and
// returns options that read it, so each test states which side of the switch
// it is on.
func failHaltsCity(t *testing.T, on bool) ProcessOptions {
	t.Helper()
	cityPath := t.TempDir()
	body := "[workspace]\nname = \"test-city\"\n"
	if on {
		body += "\n[workflows]\nfail_halts = true\n"
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return ProcessOptions{CityPath: cityPath}
}

// failHaltsWorkflow is a root with a closed step "upstream" whose metadata the
// test supplies, and an open worker step "downstream" that needs it.
type failHaltsWorkflow struct {
	root, upstream, downstream beads.Bead
}

func newFailHaltsWorkflow(t *testing.T, store beads.Store, upstreamMeta map[string]string) failHaltsWorkflow {
	t.Helper()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	meta := map[string]string{"gc.root_bead_id": root.ID}
	for k, v := range upstreamMeta {
		meta[k] = v
	}
	upstream := mustCreateWorkflowBead(t, store, beads.Bead{Title: "upstream", Type: "task", Status: "closed", Metadata: meta})
	downstream := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "downstream",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	mustDepAdd(t, store, downstream.ID, upstream.ID, "blocks")
	return failHaltsWorkflow{root: root, upstream: upstream, downstream: downstream}
}

func TestHaltIfNeedsHaltingStepHaltsTheDependentOfAFailedStep(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": "fail"})

	haltedBy, halted, err := HaltIfNeedsHaltingStep(store, wf.downstream)
	if err != nil {
		t.Fatalf("HaltIfNeedsHaltingStep: %v", err)
	}
	if !halted || haltedBy != wf.upstream.ID {
		t.Fatalf("halted=%v by %q, want halted by %s", halted, haltedBy, wf.upstream.ID)
	}
	after := mustGetBead(t, store, wf.downstream.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "skipped" || after.Metadata["gc.halted_by"] != wf.upstream.ID {
		t.Fatalf("downstream = status %q outcome %q halted_by %q, want closed/skipped/%s",
			after.Status, after.Metadata["gc.outcome"], after.Metadata["gc.halted_by"], wf.upstream.ID)
	}
}

func TestHaltIfNeedsHaltingStepLeavesTheDependentsOfANonFailure(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"pass", "skipped", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": outcome})

			if _, halted, err := HaltIfNeedsHaltingStep(store, wf.downstream); err != nil || halted {
				t.Fatalf("HaltIfNeedsHaltingStep = halted %v, err %v; want not halted", halted, err)
			}
			if after := mustGetBead(t, store, wf.downstream.ID); after.Status != "open" {
				t.Fatalf("downstream status = %q, want open", after.Status)
			}
		})
	}
}

// A refusal is a fail that says why in gc.failure_class; it halts like any
// other fail.
func TestHaltIfNeedsHaltingStepHaltsAfterAFailOfAnyClass(t *testing.T) {
	t.Parallel()

	for _, class := range []string{"", "hard", "transient", "foreign_evidence"} {
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": "fail", "gc.failure_class": class})

			if _, halted, err := HaltIfNeedsHaltingStep(store, wf.downstream); err != nil || !halted {
				t.Fatalf("HaltIfNeedsHaltingStep = halted %v, err %v; want halted", halted, err)
			}
		})
	}
}

// A halted step passes the halt on: whatever needs it is halted in turn, naming
// the step that halted it.
func TestHaltIfNeedsHaltingStepCascadesThroughAHaltedStep(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": "fail"})
	further := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "further",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": wf.root.ID},
	})
	mustDepAdd(t, store, further.ID, wf.downstream.ID, "blocks")

	if _, halted, err := HaltIfNeedsHaltingStep(store, wf.downstream); err != nil || !halted {
		t.Fatalf("halt downstream = %v, %v; want halted", halted, err)
	}
	haltedBy, halted, err := HaltIfNeedsHaltingStep(store, mustGetBead(t, store, further.ID))
	if err != nil || !halted || haltedBy != wf.downstream.ID {
		t.Fatalf("halt further = by %q halted %v err %v; want halted by %s", haltedBy, halted, err, wf.downstream.ID)
	}
}

// Retry-last-wins: an attempt's own failure never halts anything. The retry
// control grades it and records the last attempt's verdict on the logical
// bead, and that is the step's terminal outcome.
func TestHaltIfNeedsHaltingStepGradesARetriedStepByItsLogicalBead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		logicalOutcome string
		wantHalted     bool
	}{
		{logicalOutcome: "pass", wantHalted: false},
		{logicalOutcome: "fail", wantHalted: true},
	} {
		t.Run(tc.logicalOutcome, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			// The first attempt failed; the logical bead holds the last attempt's verdict.
			wf := newFailHaltsWorkflow(t, store, map[string]string{
				"gc.outcome":         "fail",
				"gc.logical_bead_id": "logical-1",
				"gc.retry_attempt":   "1",
			})
			logical := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:  "logical",
				Type:   "task",
				Status: "closed",
				Metadata: map[string]string{
					"gc.kind":         "retry",
					"gc.root_bead_id": wf.root.ID,
					"gc.outcome":      tc.logicalOutcome,
				},
			})
			mustDepAdd(t, store, wf.downstream.ID, logical.ID, "blocks")

			_, halted, err := HaltIfNeedsHaltingStep(store, wf.downstream)
			if err != nil || halted != tc.wantHalted {
				t.Fatalf("HaltIfNeedsHaltingStep = halted %v, err %v; want halted %v", halted, err, tc.wantHalted)
			}
		})
	}
}

// Finalizers always run after a fail, so a halted workflow closes rather than
// strands: the workflow finalizer, a scope-check and a teardown step.
func TestHaltIfNeedsHaltingStepNeverHaltsAFinalizer(t *testing.T) {
	t.Parallel()

	for name, meta := range map[string][2]string{
		"workflow-finalize": {"gc.kind", "workflow-finalize"},
		"scope-check":       {"gc.kind", "scope-check"},
		"teardown":          {"gc.scope_role", "teardown"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": "fail"})
			if err := store.SetMetadata(wf.downstream.ID, meta[0], meta[1]); err != nil {
				t.Fatalf("set %s: %v", meta[0], err)
			}
			if _, halted, err := HaltIfNeedsHaltingStep(store, mustGetBead(t, store, wf.downstream.ID)); err != nil || halted {
				t.Fatalf("HaltIfNeedsHaltingStep(%s) = halted %v, err %v; want not halted", name, halted, err)
			}
		})
	}
}

func TestProcessControlHaltsAControlThatNeedsAFailedStepOnlyWithTheSwitchOn(t *testing.T) {
	t.Parallel()

	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			wf := newFailHaltsWorkflow(t, store, map[string]string{"gc.outcome": "fail"})
			if err := store.SetMetadata(wf.downstream.ID, "gc.kind", "fanout"); err != nil {
				t.Fatalf("set kind: %v", err)
			}
			control := mustGetBead(t, store, wf.downstream.ID)

			result, halted, err := haltControl(store, control, failHaltsCity(t, on))
			if err != nil {
				t.Fatalf("haltControl: %v", err)
			}
			if halted != on {
				t.Fatalf("haltControl halted = %v, want %v", halted, on)
			}
			after := mustGetBead(t, store, control.ID)
			if on {
				if result.Action != "halted" || after.Status != "closed" || after.Metadata["gc.halted_by"] != wf.upstream.ID {
					t.Fatalf("on: result %+v, control status %q halted_by %q", result, after.Status, after.Metadata["gc.halted_by"])
				}
				return
			}
			if after.Status != "open" || after.Metadata["gc.halted_by"] != "" {
				t.Fatalf("off: control status %q halted_by %q, want open and unhalted", after.Status, after.Metadata["gc.halted_by"])
			}
		})
	}
}

// failedStepUnderPassingFinalizer builds a workflow where a step closed fail
// but every blocker of the finalizer passed, so finalize grading only its
// blockers passes the root.
func failedStepUnderPassingFinalizer(t *testing.T, store beads.Store) (root, failed, finalizer beads.Bead) {
	t.Helper()
	root = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	failed = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "validate",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.outcome":        "fail",
			"gc.failure_reason": "inputs_missing",
		},
	})
	publish := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "publish",
		Type:     "task",
		Status:   "closed",
		Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.outcome": "pass"},
	})
	finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "Finalize workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": root.ID},
	})
	mustDepAdd(t, store, finalizer.ID, publish.ID, "blocks")
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return root, failed, mustGetBead(t, store, finalizer.ID)
}

func TestProcessWorkflowFinalizeRootCannotPassOverAFailedStep(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, failed, finalizer := failedStepUnderPassingFinalizer(t, store)

	result, err := ProcessControl(store, finalizer, failHaltsCity(t, true))
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want workflow-fail", result)
	}
	after := mustGetBead(t, store, root.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("root = status %q outcome %q, want closed/fail", after.Status, after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.failure_subject"] != failed.ID || after.Metadata["gc.failure_reason"] != "inputs_missing" {
		t.Fatalf("root names subject %q reason %q, want %s/inputs_missing",
			after.Metadata["gc.failure_subject"], after.Metadata["gc.failure_reason"], failed.ID)
	}
}

// With the switch off the root passes over the failed step, exactly as before.
func TestProcessWorkflowFinalizeSwitchOffKeepsGradingOnlyTheBlockers(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, _, finalizer := failedStepUnderPassingFinalizer(t, store)

	result, err := ProcessControl(store, finalizer, failHaltsCity(t, false))
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want workflow-pass", result)
	}
	after := mustGetBead(t, store, root.ID)
	if after.Metadata["gc.outcome"] != "pass" || after.Metadata["gc.failure_subject"] != "" {
		t.Fatalf("root = outcome %q subject %q, want pass with no subject", after.Metadata["gc.outcome"], after.Metadata["gc.failure_subject"])
	}
}

// Retry-last-wins at finalize: a failed attempt whose step passed on a later
// attempt does not fail the root with the switch on.
func TestProcessWorkflowFinalizePassesOverASupersededAttempt(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, failed, finalizer := failedStepUnderPassingFinalizer(t, store)
	if err := store.SetMetadata(failed.ID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("set pass: %v", err)
	}
	_ = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "validate attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id":    root.ID,
			"gc.outcome":         "fail",
			"gc.logical_bead_id": failed.ID,
			"gc.retry_attempt":   "1",
		},
	})

	result, err := ProcessControl(store, finalizer, failHaltsCity(t, true))
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want workflow-pass", result)
	}
}

// A retry that exhausted its attempts on transient failures closes fail with
// gc.failure_class=transient. The abort-scope scan ignores the transient
// class; under the switch it is a terminal fail like any other and
// fails the root.
func TestProcessWorkflowFinalizeCountsATerminalTransientFail(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, failed, finalizer := failedStepUnderPassingFinalizer(t, store)
	if err := store.SetMetadataBatch(failed.ID, map[string]string{
		"gc.kind":              "retry",
		"gc.failure_class":     "transient",
		"gc.final_disposition": "hard_fail",
	}); err != nil {
		t.Fatalf("set exhausted retry: %v", err)
	}

	result, err := ProcessControl(store, finalizer, failHaltsCity(t, true))
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want workflow-fail", result)
	}
	if after := mustGetBead(t, store, root.ID); after.Metadata["gc.failure_subject"] != failed.ID {
		t.Fatalf("root names subject %q, want %s", after.Metadata["gc.failure_subject"], failed.ID)
	}
}
