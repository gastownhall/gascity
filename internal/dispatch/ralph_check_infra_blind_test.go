package dispatch

import (
	"errors"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// blindGateCases are gate scripts that run to completion but could not see the
// infrastructure they read. Each one exits nonzero, so before ga-pqlgh they
// were indistinguishable from a genuine "the thing you check is not true yet"
// failure and burned a semantic ralph attempt — the mechanism that killed an
// APPROVED review at 7/7 attempts.
var blindGateCases = []struct {
	name   string
	script string
}{
	{
		name:   "tempfail exit code",
		script: "#!/bin/bash\nexit 75\n",
	},
	{
		name:   "unresolvable gc home",
		script: "#!/bin/bash\necho 'no GC_HOME available' >&2\nexit 1\n",
	},
	{
		name:   "uncached pack import",
		script: "#!/bin/bash\necho 'remote import gh:x is locked but not cached at /c; run \"gc import install\"' >&2\nexit 1\n",
	},
	{
		name:   "credential refusal",
		script: "#!/bin/bash\necho \"Error 1045 (28000): Access denied for user 'root'@'localhost'\" >&2\nexit 1\n",
	},
}

// verdictGateCases are gate scripts whose stderr carries a string that looks
// infrastructural but that the read recovered from, or that is remediation
// advice rather than an error. They failed for their own reason, so they are
// verdicts and must keep burning attempts. Reclassifying them would spend the
// whole infra budget (and ~5 minutes of dead time) on the first genuine gate
// failure in any city that emits them — the incident shape the budget exists
// to prevent.
var verdictGateCases = []struct {
	name   string
	script string
}{
	{
		// internal/beads/factory.go logs this and then falls back to bd, so
		// the read succeeded; in a native-ineligible city it prints on every
		// store-open.
		name:   "native store unavailable is a recovered path",
		script: "#!/bin/bash\necho 'WARN native_store_unavailable scope=/city' >&2\necho 'review verdict is reject' >&2\nexit 1\n",
	},
	{
		// The remediation every pack-cache error carries, and `gc import
		// install`'s own error prefix.
		name:   "gc import install is remediation advice",
		script: "#!/bin/bash\necho 'gc import install: pack \"core\" failed checksum verification' >&2\nexit 1\n",
	},
}

// TestProcessRalphCheckBlindReadDoesNotBurnAttempt pins ga-pqlgh: a gate that
// exits nonzero because it could not reach its infrastructure is an infra
// outcome, not a verdict, and must re-run via the benign ErrControlPending path
// without consuming a gc.attempt.
func TestProcessRalphCheckBlindReadDoesNotBurnAttempt(t *testing.T) {
	t.Parallel()

	for _, tc := range blindGateCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "blind-check.sh", tc.script)
			store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
			if err := store.Close(run1.ID); err != nil {
				t.Fatalf("close run1: %v", err)
			}
			check1 = mustGetBead(t, store, check1.ID)

			_, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
			if !errors.Is(err, ErrControlPending) {
				t.Fatalf("ProcessControl on a blind gate read = %v, want ErrControlPending (re-run without burning an attempt)", err)
			}

			got := mustGetBead(t, store, check1.ID)
			if a := got.Metadata[beadmeta.AttemptMetadataKey]; a != "1" {
				t.Errorf("gc.attempt = %q, want 1 (a blind read must not burn a semantic attempt)", a)
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
				t.Errorf("gc.check_infra_retry = %q, want 1", r)
			}
			if got.Status == "closed" {
				t.Errorf("check bead should stay open for the re-run, got closed")
			}
		})
	}
}

// TestProcessRalphCheckVerdictMarkerStillBurnsAttempt is the counterpart
// control to the table above: a gate whose stderr merely mentions an
// infrastructure string is still a verdict. It must burn a semantic attempt and
// must not touch the shared infra-retry budget.
func TestProcessRalphCheckVerdictMarkerStillBurnsAttempt(t *testing.T) {
	t.Parallel()

	for _, tc := range verdictGateCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "verdict-check.sh", tc.script)
			store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
			if err := store.Close(run1.ID); err != nil {
				t.Fatalf("close run1: %v", err)
			}
			check1 = mustGetBead(t, store, check1.ID)

			result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if !result.Processed || result.Action != "retry" {
				t.Fatalf("result = %+v, want processed retry (this gate produced a verdict and must burn an attempt)", result)
			}
			if r := mustGetBead(t, store, check1.ID).Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "" {
				t.Errorf("gc.check_infra_retry = %q, want empty — a verdict must not spend the infra budget", r)
			}
		})
	}
}

// TestProcessRalphCheckBlindReadBudgetExhaustionBurns pins the bound: the
// attempt-free re-run reuses the existing infra-retry budget, so a gate that is
// permanently blind still terminates the workflow instead of pending forever.
func TestProcessRalphCheckBlindReadBudgetExhaustionBurns(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "blind-check.sh", "#!/bin/bash\nexit 75\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.SetMetadata(check1.ID, beadmeta.CheckInfraRetryMetadataKey, strconv.Itoa(maxCheckInfraRetries)); err != nil {
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
		t.Fatalf("result = %+v, want processed retry once the infra-retry budget is spent", result)
	}
}

// TestProcessRalphControlBlindReadDoesNotBurnIteration covers the compiled
// kind=ralph lane for each blind gate shape: a gate that exits nonzero because
// it could not see its infrastructure produced no verdict, so it charges the
// infra budget and re-runs without spending the iteration.
func TestProcessRalphControlBlindReadDoesNotBurnIteration(t *testing.T) {
	t.Parallel()

	for _, tc := range blindGateCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "blind-check.sh", tc.script)
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)

			_, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
			if !errors.Is(err, ErrControlPending) {
				t.Fatalf("ProcessControl on a blind gate read = %v, want ErrControlPending (re-run without burning the iteration)", err)
			}

			got := mustGet(t, store, control.ID)
			if got.Status == "closed" {
				t.Fatalf("control closed (outcome=%q failed_attempt=%q), want held open for an infra re-run",
					got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata[beadmeta.FailedAttemptMetadataKey])
			}
			if log := got.Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
				t.Errorf("gc.attempt_log = %q, want empty (no iteration was spent)", log)
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "1" {
				t.Errorf("gc.check_infra_retry = %q, want 1", r)
			}
		})
	}
}

// TestProcessRalphControlVerdictStillBurnsIteration is the compiled-lane
// control: a gate whose nonzero exit is an ordinary verdict — including stderr
// that merely mentions an infrastructure string — still spends the iteration
// and never touches the infra budget.
func TestProcessRalphControlVerdictStillBurnsIteration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
	}{
		{name: "plain exit 1", script: "#!/bin/bash\necho 'tests failed' >&2\nexit 1\n"},
	}
	cases = append(cases, verdictGateCases...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "verdict-check.sh", tc.script)
			store, control := newUnlaunchableCheckRalphControl(t, checkPath)

			result, err := ProcessControl(store, control, ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != "fail" {
				t.Fatalf("result = %+v, want fail (a verdict consumes the only iteration)", result)
			}
			got := mustGet(t, store, control.ID)
			if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
				t.Fatalf("control status=%q outcome=%q, want closed fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if fa := got.Metadata[beadmeta.FailedAttemptMetadataKey]; fa != "1" {
				t.Errorf("gc.failed_attempt = %q, want 1", fa)
			}
			if r := got.Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "" {
				t.Errorf("gc.check_infra_retry = %q, want empty — a verdict must not spend the infra budget", r)
			}
		})
	}
}

// TestProcessRalphCheckOrdinaryFailureStillBurnsAttempt is the control: an
// ordinary nonzero gate is still a real verdict and must keep burning attempts.
// Without it, the blind-read classification could silently swallow every gate
// failure.
func TestProcessRalphCheckOrdinaryFailureStillBurnsAttempt(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "failing-check.sh", "#!/bin/bash\necho 'tests failed' >&2\nexit 1\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	check1 = mustGetBead(t, store, check1.ID)

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry (an ordinary gate failure is a verdict and burns an attempt)", result)
	}
	if r := mustGetBead(t, store, check1.ID).Metadata[beadmeta.CheckInfraRetryMetadataKey]; r != "" {
		t.Errorf("gc.check_infra_retry = %q, want empty for an ordinary gate failure", r)
	}
}
