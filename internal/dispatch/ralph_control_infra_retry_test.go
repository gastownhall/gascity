package dispatch

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Regression tests for gastownhall/gascity#6947: a compiler-created direct
// gc.kind=ralph control (evaluateRalphIteration) mapped EVERY non-GatePass
// check result — including GateTimeout and infra-blind reads, where the gate
// never produced a verdict — to attemptContinue, so a single transient gate
// infrastructure timeout burned the iteration and abort_scope'd the workflow
// at max_attempts = 1. The bounded infra-retry budget from processRalphCheck
// (gc.kind=check) now applies to the direct ralph control path too — for
// no-verdict outcomes only: on this lane the gate runs on
// context.Background, so a GateError is always a failed start and keeps its
// formula-spec-v2 §3.1 semantics (it spends the attempt).

// newDirectRalphControl builds the graph.v2 shape from #6947: a gc.kind=ralph
// control carrying its own exec check config, blocked on one completed
// iteration under a workflow root. The iteration closed WITHOUT a failure
// outcome — the review work itself completed; only the gate timed out.
func newDirectRalphControl(t *testing.T, store beads.Store, checkPath string) beads.Bead {
	t.Helper()
	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "1",
			"gc.check_mode":   "exec",
			"gc.check_path":   checkPath,
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.1",
			"gc.scope_role":   "body",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, iteration.ID)
	mustDep(t, store, control.ID, iteration.ID, "blocks")
	return mustGet(t, store, control.ID)
}

// TestProcessRalphControlInfraTimeoutDoesNotConsumeIteration is the #6947
// reproduction: a completed iteration feeds a ralph control whose exec check
// times out once. The timeout produced no verdict, so the control must re-run
// the gate via the benign ErrControlPending path — bumping only
// gc.check_infra_retry — and must NOT burn the iteration at max_attempts = 1.
func TestProcessRalphControlInfraTimeoutDoesNotConsumeIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store := beads.NewMemStore()
	control := newDirectRalphControl(t, store, checkPath)
	if err := store.SetMetadata(control.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}

	_, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl on GateTimeout = %v, want ErrControlPending (re-run without consuming the iteration)", err)
	}

	got := mustGet(t, store, control.ID)
	if got.Status != "open" {
		t.Errorf("control status = %q, want open for the gate re-run", got.Status)
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
		t.Errorf("gc.check_infra_retry = %q, want 1", r)
	}
	if o := got.Metadata[beadmeta.OutcomeMetadataKey]; o != "" {
		t.Errorf("gc.outcome = %q, want empty (no verdict was produced)", o)
	}
	if log := got.Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
		t.Errorf("gc.attempt_log = %q, want empty (the iteration was not consumed)", log)
	}
	if f := got.Metadata[beadmeta.FailedAttemptMetadataKey]; f != "" {
		t.Errorf("gc.failed_attempt = %q, want empty", f)
	}

	// The infra re-run must not have spawned a second iteration.
	assertSingleIteration(t, store, got.Metadata[beadmeta.RootBeadIDMetadataKey])

	// Recovery: once the transient condition clears, the SAME iteration's
	// check passes and the control closes pass on iteration 1.
	writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nexit 0\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl after recovery: %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass after the gate recovers", result)
	}
	got = mustGet(t, store, control.ID)
	if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Errorf("gc.outcome = %q, want pass", got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	assertSingleIteration(t, store, got.Metadata[beadmeta.RootBeadIDMetadataKey])
}

// TestProcessRalphControlFailedStartConsumesIteration pins the formula-spec-v2
// §3.1 boundary on the direct ralph lane: a GateError here is always a failed
// START (the gate runs on context.Background, so a parent cancel cannot
// produce one — this script's interpreter does not exist, so fork/exec fails
// before anything runs). A failed start is a config/environment problem, not
// an infrastructure blip: it spends the iteration like any failure and must
// not touch the infra-retry budget. This is what keeps
// TestProcessRalphControlRemovedWorkDirStoreCopyIsNotHeld green alongside
// #6947. (A script that lost its executable bit never reaches the gate: it is
// rejected by ResolveConditionPath and takes the ErrControlDriftPending
// config-repair path instead.)
func TestProcessRalphControlFailedStartConsumesIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "badinterp-check.sh", "#!/nonexistent/gc-test/interp\nexit 0\n")
	store := beads.NewMemStore()
	control := newDirectRalphControl(t, store, checkPath)

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "fail" {
		t.Fatalf("result = %+v, want processed fail (a failed start is graded, not held open)", result)
	}

	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Errorf("control = status %q outcome %q, want closed/fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if f := got.Metadata[beadmeta.FailedAttemptMetadataKey]; f != "1" {
		t.Errorf("gc.failed_attempt = %q, want 1", f)
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "" {
		t.Errorf("gc.check_infra_retry = %q, want empty (a failed start is not an infrastructure re-run)", r)
	}
	if log := got.Metadata[beadmeta.AttemptLogMetadataKey]; log == "" {
		t.Errorf("gc.attempt_log is empty, want the failed start recorded")
	}
}

// TestProcessRalphControlInfraBlindDoesNotConsumeIteration pins the
// ga-pqlgh-shaped arm on the direct control path: a gate that ran to
// completion but could not see its infrastructure exits with the tempfail
// code (75) and must be charged to the infra-retry budget rather than
// consuming an iteration as a substantive negative verdict. Unlike the
// kind=check lane the outcome is not reclassified: if the budget runs out,
// the attempt log records the GateFail the gate actually reported.
func TestProcessRalphControlInfraBlindDoesNotConsumeIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "blind-check.sh", "#!/bin/bash\nexit 75\n")
	store := beads.NewMemStore()
	control := newDirectRalphControl(t, store, checkPath)

	_, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl on infra-blind GateFail(exit 75) = %v, want ErrControlPending", err)
	}

	got := mustGet(t, store, control.ID)
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
		t.Errorf("gc.check_infra_retry = %q, want 1", r)
	}
	assertSingleIteration(t, store, got.Metadata[beadmeta.RootBeadIDMetadataKey])
}

// TestProcessRalphControlCompletedNegativeVerdictStillExhausts pins the
// boundary the other way: a gate that ran to completion and returned a real
// negative verdict (exit 1) IS a substantive failure and must consume the
// iteration — at max_attempts = 1 the control closes failed immediately.
func TestProcessRalphControlCompletedNegativeVerdictStillExhausts(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "reject-check.sh", "#!/bin/bash\necho 'verdict: reject' >&2\nexit 1\n")
	store := beads.NewMemStore()
	control := newDirectRalphControl(t, store, checkPath)

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "fail" {
		t.Fatalf("result = %+v, want processed fail (a completed negative verdict consumes the iteration)", result)
	}

	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Errorf("control = status %q outcome %q, want closed/fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if f := got.Metadata[beadmeta.FailedAttemptMetadataKey]; f != "1" {
		t.Errorf("gc.failed_attempt = %q, want 1", f)
	}
	if log := got.Metadata[beadmeta.AttemptLogMetadataKey]; !strings.Contains(log, "\"fail\"") {
		t.Errorf("gc.attempt_log = %q, want a fail entry for iteration 1", log)
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "" {
		t.Errorf("gc.check_infra_retry = %q, want empty (no infra re-run for a completed verdict)", r)
	}
}

// TestProcessRalphControlInfraRetryBudgetExhaustionTerminates pins the bound:
// once the infra-retry budget is spent, a gate that still cannot run falls
// through to the normal exhaust path so a permanently unrunnable gate
// terminates the workflow rather than pending forever.
func TestProcessRalphControlInfraRetryBudgetExhaustionTerminates(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store := beads.NewMemStore()
	control := newDirectRalphControl(t, store, checkPath)
	if err := store.SetMetadataBatch(control.ID, map[string]string{
		beadmeta.CheckTimeoutMetadataKey:    "100ms",
		beadmeta.CheckInfraRetryMetadataKey: strconv.Itoa(maxCheckInfraRetries),
	}); err != nil {
		t.Fatalf("prime infra-retry budget: %v", err)
	}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "fail" {
		t.Fatalf("result = %+v, want processed fail once the infra-retry budget is spent", result)
	}

	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Errorf("control = status %q outcome %q, want closed/fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if log := got.Metadata[beadmeta.AttemptLogMetadataKey]; !strings.Contains(log, "timeout") {
		t.Errorf("gc.attempt_log = %q, want the timeout recorded on exhaustion", log)
	}
}

func assertSingleIteration(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	all, err := rootViewMembers(store, rootID, ProcessOptions{})
	if err != nil {
		t.Fatalf("list workflow beads: %v", err)
	}
	var got int
	for _, bead := range all {
		if strings.Contains(bead.Metadata[beadmeta.StepRefMetadataKey], ".iteration.") {
			got++
		}
	}
	if got != 1 {
		t.Errorf("iteration beads under root = %d, want 1 (infra re-runs must not spawn iterations)", got)
	}
}
