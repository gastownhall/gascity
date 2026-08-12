package dispatch

import (
	"errors"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
