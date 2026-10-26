package dispatch

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
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

// TestProcessRalphControlInfraRetryThenPassClosesSameIteration pins that the
// attempt-free re-run grades the same iteration: once the check that timed out
// passes, the control closes pass on iteration 1 with a single attempt-log
// entry and no successor iteration.
func TestProcessRalphControlInfraRetryThenPassClosesSameIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "flaky-check.sh", "#!/bin/sh\nsleep 30\n")
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	if err := store.SetMetadata(control.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}
	beadsBefore := countAllBeads(t, store)
	opts := ProcessOptions{CityPath: cityPath}

	if _, err := ProcessControl(store, mustGet(t, store, control.ID), opts); !errors.Is(err, ErrControlPending) {
		t.Fatalf("first ProcessControl error = %v, want ErrControlPending", err)
	}

	writeCheckScript(t, cityPath, "flaky-check.sh", "#!/bin/sh\nexit 0\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl after the check recovered: %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	entries := decodeAttemptLog(t, got)
	if len(entries) != 1 || entries[0]["attempt"] != "1" || entries[0]["outcome"] != convergence.GatePass {
		t.Errorf("gc.attempt_log = %q, want one attempt-1 pass entry", got.Metadata[beadmeta.AttemptLogMetadataKey])
	}
	if n := countAllBeads(t, store); n != beadsBefore {
		t.Errorf("bead count = %d, want %d (no successor iteration may be spawned)", n, beadsBefore)
	}
}

// decodeAttemptLog returns the entries of bead's gc.attempt_log.
func decodeAttemptLog(t *testing.T, bead beads.Bead) []map[string]string {
	t.Helper()
	var entries []map[string]string
	if err := json.Unmarshal([]byte(bead.Metadata[beadmeta.AttemptLogMetadataKey]), &entries); err != nil {
		t.Fatalf("decode gc.attempt_log %q: %v", bead.Metadata[beadmeta.AttemptLogMetadataKey], err)
	}
	return entries
}

// TestProcessRalphControlInfraRetryBudgetExhaustionBurns pins the bound on the
// compiled kind=ralph lane: once the control's infrastructure budget is spent,
// a check that still yields no verdict is graded as the gate reported it, so a
// permanently hung or blind check closes the step instead of pending forever.
func TestProcessRalphControlInfraRetryBudgetExhaustionBurns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		script      string
		wantOutcome string
	}{
		{name: "timeout", script: "#!/bin/sh\nsleep 30\n", wantOutcome: convergence.GateTimeout},
		{name: "exit 75", script: "#!/bin/sh\nexit 75\n", wantOutcome: convergence.GateFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "infra-check.sh", tc.script)
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)
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
				t.Fatalf("control status=%q outcome=%q, want closed fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
			}
			entries := decodeAttemptLog(t, got)
			if len(entries) != 1 || entries[0]["attempt"] != "1" || entries[0]["outcome"] != tc.wantOutcome {
				t.Errorf("gc.attempt_log = %q, want one attempt-1 %s entry", got.Metadata[beadmeta.AttemptLogMetadataKey], tc.wantOutcome)
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != strconv.Itoa(maxCheckInfraRetries) {
				t.Errorf("gc.check_infra_retry = %q, want %d (an exhausted budget is not spent further)", r, maxCheckInfraRetries)
			}
		})
	}
}

// TestProcessRalphControlInfraRetryTransientStoreErrorStaysPending pins the
// store boundary of the infra-retry spend: a transient failure writing
// gc.check_infra_retry leaves the control open and pending with the transient
// class recorded and no attempt graded, and the next sweep records the spend.
func TestProcessRalphControlInfraRetryTransientStoreErrorStaysPending(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/sh\nsleep 30\n")
	base, control := newUnlaunchableCheckRalphControl(t, checkPath)
	if err := base.SetMetadata(control.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}
	store := &failOnceMetadataKeyStore{
		Store: base,
		key:   beadmeta.CheckInfraRetryMetadataKey,
		err:   errors.New("failed to update metadata: invalid connection: i/o timeout"),
	}
	opts := ProcessOptions{CityPath: cityPath}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl = (%+v, %v), want ErrControlPending on a transient infra-retry write", result, err)
	}
	got := mustGet(t, store, control.ID)
	if got.Status == "closed" {
		t.Fatalf("control closed (outcome=%q), want it held open", got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got.Metadata["gc.controller_error_class"] != "transient" {
		t.Errorf("gc.controller_error_class = %q, want transient", got.Metadata["gc.controller_error_class"])
	}
	for _, key := range []string{beadmeta.AttemptLogMetadataKey, beadmeta.FailedAttemptMetadataKey, beadmeta.CheckInfraRetryMetadataKey} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q, want empty after the failed write", key, v)
		}
	}

	if _, err := ProcessControl(store, mustGet(t, store, control.ID), opts); !errors.Is(err, ErrControlPending) {
		t.Fatalf("second ProcessControl error = %v, want ErrControlPending", err)
	}
	if r := mustGet(t, store, control.ID).Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
		t.Errorf("gc.check_infra_retry = %q after the store recovered, want 1", r)
	}
}
