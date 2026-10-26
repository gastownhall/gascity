package dispatch

import (
	"errors"
	"strconv"
	"strings"
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

// TestProcessRalphControlInfraTimeoutDoesNotConsumeIteration is the repro for
// gastownhall/gascity#6947: a compiled kind=ralph control (max_attempts=1)
// whose exec check times out produced no verdict, so it must take the bounded
// infra-retry path instead of consuming the iteration and closing failed.
func TestProcessRalphControlInfraTimeoutDoesNotConsumeIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	if err := store.SetMetadata(control.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}
	control = mustGet(t, store, control.ID)

	result, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Errorf("ProcessControl on GateTimeout = (%+v, %v), want ErrControlPending (infra re-run without consuming the iteration)", result, err)
	}

	got := mustGet(t, store, control.ID)
	if got.Status == "closed" {
		t.Errorf("ralph control closed (outcome=%q failed_attempt=%q attempt_log=%q), want held open for an infra re-run",
			got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailedAttemptMetadataKey], got.Metadata[beadmeta.AttemptLogMetadataKey])
	}
	if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
		t.Errorf("gc.check_infra_retry = %q, want 1", r)
	}
}

// TestProcessRalphControlInfraTimeoutDoesNotBurnIteration covers the compiled
// kind=ralph lane at max_attempts 1 and 2: a gate timeout produced no verdict,
// so the iteration is not spent — the control pends for an attempt-free re-run
// and no successor iteration is spawned.
func TestProcessRalphControlInfraTimeoutDoesNotBurnIteration(t *testing.T) {
	t.Parallel()

	for _, maxAttempts := range []string{"1", "2"} {
		t.Run("max_attempts="+maxAttempts, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)
			if err := store.SetMetadataBatch(control.ID, map[string]string{
				beadmeta.MaxAttemptsMetadataKey:  maxAttempts,
				beadmeta.CheckTimeoutMetadataKey: "100ms",
			}); err != nil {
				t.Fatalf("stamp max_attempts/check_timeout: %v", err)
			}
			control = mustGet(t, store, control.ID)
			beadsBefore := countAllBeads(t, store)

			_, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
			if !errors.Is(err, ErrControlPending) {
				t.Fatalf("ProcessControl on GateTimeout = %v, want ErrControlPending (infra re-run without burning the iteration)", err)
			}

			got := mustGet(t, store, control.ID)
			if got.Status == "closed" {
				t.Fatalf("control closed (outcome=%q failed_attempt=%q), want held open for an infra re-run",
					got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailedAttemptMetadataKey])
			}
			for _, key := range []string{
				beadmeta.OutcomeMetadataKey,
				beadmeta.AttemptLogMetadataKey,
				beadmeta.FailedAttemptMetadataKey,
			} {
				if v := got.Metadata[key]; v != "" {
					t.Errorf("%s = %q, want empty (no iteration was spent)", key, v)
				}
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
				t.Errorf("gc.check_infra_retry = %q, want 1", r)
			}
			if n := countAllBeads(t, store); n != beadsBefore {
				t.Errorf("bead count = %d, want %d (no successor iteration may be spawned)", n, beadsBefore)
			}
		})
	}
}

// TestProcessRalphControlInfraRetryThenPassClosesSameIteration pins the
// recovery path: after a timed-out run charged the infra budget (not the
// iteration), a sweep whose check now finishes and passes closes the control
// on iteration 1 with a single attempt-log entry and no successor iteration.
func TestProcessRalphControlInfraRetryThenPassClosesSameIteration(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/bash\nsleep 30\n")
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	if err := store.SetMetadata(control.ID, beadmeta.CheckTimeoutMetadataKey, "100ms"); err != nil {
		t.Fatalf("set check timeout: %v", err)
	}
	control = mustGet(t, store, control.ID)
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("first ProcessControl = %v, want ErrControlPending", err)
	}

	// The gate heals: the same script now exits 0 well within the timeout.
	writeCheckScript(t, cityPath, "slow-check.sh", "#!/bin/sh\nexit 0\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("second ProcessControl: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	log := got.Metadata[beadmeta.AttemptLogMetadataKey]
	if !strings.Contains(log, `"attempt":"1"`) || !strings.Contains(log, `"outcome":"pass"`) || strings.Contains(log, `"attempt":"2"`) {
		t.Errorf("gc.attempt_log = %q, want a single attempt-1 pass entry", log)
	}
	if n := countAllBeads(t, store); n != beadsBefore {
		t.Errorf("bead count = %d, want %d (no successor iteration)", n, beadsBefore)
	}
}

// TestProcessRalphControlInfraRetryBudgetExhaustionBurns pins the bound on the
// compiled lane: once the infra budget is spent, a gate that still yields no
// verdict falls through to normal grading so the step terminates instead of
// pending forever. The attempt log records what the gate actually did, and the
// counter stays at its cap.
func TestProcessRalphControlInfraRetryBudgetExhaustionBurns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		script     string
		timeout    string
		wantLogOut string
	}{
		{name: "timeout", script: "#!/bin/bash\nsleep 30\n", timeout: "100ms", wantLogOut: "timeout"},
		{name: "blind exit 75", script: "#!/bin/bash\nexit 75\n", wantLogOut: "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "budget-check.sh", tc.script)
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)
			meta := map[string]string{
				beadmeta.CheckInfraRetryMetadataKey: strconv.Itoa(maxCheckInfraRetries),
			}
			if tc.timeout != "" {
				meta[beadmeta.CheckTimeoutMetadataKey] = tc.timeout
			}
			if err := store.SetMetadataBatch(control.ID, meta); err != nil {
				t.Fatalf("prime infra budget: %v", err)
			}
			control = mustGet(t, store, control.ID)

			result, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != "fail" {
				t.Fatalf("result = %+v, want fail (budget spent, the outcome is graded)", result)
			}
			got := mustGet(t, store, control.ID)
			if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("control status=%q outcome=%q, want closed fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
			}
			log := got.Metadata[beadmeta.AttemptLogMetadataKey]
			if !strings.Contains(log, `"attempt":"1"`) || !strings.Contains(log, `"outcome":"`+tc.wantLogOut+`"`) {
				t.Errorf("gc.attempt_log = %q, want an attempt-1 entry with outcome %q", log, tc.wantLogOut)
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != strconv.Itoa(maxCheckInfraRetries) {
				t.Errorf("gc.check_infra_retry = %q, want it to stay at %d", r, maxCheckInfraRetries)
			}
		})
	}
}
