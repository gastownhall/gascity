package dispatch

import (
	"errors"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessRalphCheckInfraTimeoutDoesNotBurnAttempt is the regression for the
// maintainer-city "zero-merge day" incident: a transport/store outage made the
// adopt-pr gates fail to EXECUTE, and each gate-exec error consumed a ralph
// attempt, so after 3 attempts abort_scope fired on genuinely-green PRs.
//
// A GateTimeout (the gate could not finish) is an infra outcome — the gate
// never produced a verdict — and must NOT burn a gc.attempt. It re-runs via the
// benign ErrControlPending path, bumping only the separate infra-retry counter.
func TestProcessRalphCheckInfraTimeoutDoesNotBurnAttempt(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	// A gate that never finishes within its timeout -> GateTimeout.
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.SetMetadata(check1.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	check1 = mustGetBead(t, store, check1.ID)

	_, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl on GateTimeout = %v, want ErrControlPending (re-run without burning an attempt)", err)
	}

	got := mustGetBead(t, store, check1.ID)
	if a := got.Metadata[beadmeta.AttemptMetadataKey]; a != "1" {
		t.Errorf("gc.attempt = %q, want 1 (an infra gate-exec error must not burn an attempt)", a)
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
		t.Errorf("gc.check_infra_retry = %q, want 1", r)
	}
	if got.Status == "closed" {
		t.Errorf("check bead should stay open for the re-run, got closed")
	}
}

// TestProcessRalphCheckInfraRetryBudgetExhaustionBurns pins the required bound:
// once the infra-retry budget is spent, a gate that still cannot run falls
// through to the normal retry/burn path so a permanently-unrunnable gate
// terminates the workflow rather than pending forever.
func TestProcessRalphCheckInfraRetryBudgetExhaustionBurns(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.SetMetadataBatch(check1.ID, map[string]string{
		beadmeta.CheckTimeoutMetadataKey:    "100ms",
		beadmeta.CheckInfraRetryMetadataKey: strconv.Itoa(maxCheckInfraRetries),
	}); err != nil {
		t.Fatalf("prime infra-retry budget: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	check1 = mustGetBead(t, store, check1.ID)

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry (attempt burned once the infra-retry budget is spent)", result)
	}
}

// TestProcessRalphControlInfraTimeoutDoesNotBurnIteration is the regression for
// gastownhall/gascity#6947: a compiled kind=ralph control whose exec check runs
// past gc.check_timeout produced no verdict, so it must re-run via the bounded
// infra-retry path instead of grading the iteration. At max_attempts=1 the
// timeout used to close the step failed and abort_scope its downstream work; at
// max_attempts=2 it re-ran the whole iteration body.
func TestProcessRalphControlInfraTimeoutDoesNotBurnIteration(t *testing.T) {
	t.Parallel()

	for _, maxAttempts := range []string{"1", "2"} {
		t.Run("max_attempts="+maxAttempts, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/sh\nsleep 30\n")
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)
			if err := store.SetMetadataBatch(control.ID, map[string]string{
				beadmeta.CheckTimeoutMetadataKey: "100ms",
				beadmeta.MaxAttemptsMetadataKey:  maxAttempts,
			}); err != nil {
				t.Fatalf("set check timeout: %v", err)
			}
			beadsBefore := countAllBeads(t, store)

			result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
			if !errors.Is(err, ErrControlPending) {
				t.Fatalf("ProcessControl on GateTimeout = (%+v, %v), want ErrControlPending (re-run without spending the iteration)", result, err)
			}
			assertRalphControlInfraRetryPending(t, store, control.ID, "1", beadsBefore)
		})
	}
}

// assertRalphControlInfraRetryPending asserts a compiled-lane control was held
// open for an attempt-free gate re-run: the infra-retry counter reads
// wantRetry, no attempt was graded, and no successor iteration was spawned.
func assertRalphControlInfraRetryPending(t *testing.T, store beads.Store, controlID, wantRetry string, wantBeads int) {
	t.Helper()
	got := mustGet(t, store, controlID)
	if got.Status == "closed" {
		t.Fatalf("control %s closed (outcome=%q failed_attempt=%q attempt_log=%q), want it held open for an infra re-run",
			controlID, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailedAttemptMetadataKey], got.Metadata[beadmeta.AttemptLogMetadataKey])
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != wantRetry {
		t.Errorf("gc.check_infra_retry = %q, want %s", r, wantRetry)
	}
	for _, key := range []string{
		beadmeta.OutcomeMetadataKey,
		beadmeta.AttemptLogMetadataKey,
		beadmeta.FailedAttemptMetadataKey,
	} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q, want empty (a gate run without a verdict spends no attempt)", key, v)
		}
	}
	if n := countAllBeads(t, store); n != wantBeads {
		t.Errorf("bead count = %d, want %d (no successor iteration may be spawned)", n, wantBeads)
	}
}
