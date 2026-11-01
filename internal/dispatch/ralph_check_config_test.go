package dispatch

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/execenv"
)

// configExitScript is a check script that declares it cannot run in this
// environment (exit 78, EX_CONFIG) and prints the diagnostic a real validator
// would: the interpreter it ran, and the module it could not import.
const configExitScript = "#!/bin/sh\n" +
	"echo 'build-artifact-check: python3 (/usr/bin/python3) cannot import yaml; user site under HOME=/city has no PyYAML' >&2\n" +
	"echo 'second line that must not reach the hold reason' >&2\n" +
	"exit 78\n"

// TestRunRalphCheckExitConfigIsDriftPending pins that a check script exiting
// 78 is reported as ErrControlDriftPending with a stable reason built from the
// first stderr line. A bare GateFail would spend an attempt on a question the
// script could not answer; a bare error would quarantine-close the step and
// release its dependents.
func TestRunRalphCheckExitConfigIsDriftPending(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "config.sh", configExitScript)
	store := beads.NewMemStore()
	check := beads.Bead{ID: "check-config", Type: "task", Metadata: map[string]string{beadmeta.CheckPathMetadataKey: checkPath}}
	subject := beads.Bead{ID: "run-config", Type: "task"}
	opts := ProcessOptions{CityPath: cityPath}

	_, err := runRalphCheck(store, check, subject, 1, opts)
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("runRalphCheck error = %v, want ErrControlDriftPending", err)
	}
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("runRalphCheck error = %v, want it to also match ErrControlPending", err)
	}
	msg := err.Error()
	for _, want := range []string{check.ID, "exited 78", "EX_CONFIG", "cannot import yaml", "stays open"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "second line") {
		t.Errorf("error %q carries more than the first stderr line", msg)
	}
	// The cmd layer freezes bookkeeping on a verbatim repeat, so the hold
	// text must be stable across sweeps.
	_, again := runRalphCheck(store, check, subject, 1, opts)
	if again == nil || again.Error() != msg {
		t.Fatalf("second runRalphCheck error = %v, want verbatim repeat %q", again, msg)
	}
}

// TestRunRalphCheckExitConfigWithoutStderrHasPlaceholderReason pins that a
// silent 78 still produces a readable, stable hold reason.
func TestRunRalphCheckExitConfigWithoutStderrHasPlaceholderReason(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "silent.sh", "#!/bin/sh\nexit 78\n")
	check := beads.Bead{ID: "check-silent", Type: "task", Metadata: map[string]string{beadmeta.CheckPathMetadataKey: checkPath}}

	_, err := runRalphCheck(beads.NewMemStore(), check, beads.Bead{ID: "run-silent", Type: "task"}, 1, ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("runRalphCheck error = %v, want ErrControlDriftPending", err)
	}
	if !strings.Contains(err.Error(), "(no stderr from the check)") {
		t.Fatalf("error %q, want the placeholder reason for an empty stderr", err)
	}
}

// TestGateConfigReason pins the reason-shaping rules: first non-blank line,
// trimmed, valid UTF-8, no control characters, clipped on a rune boundary,
// placeholder when nothing usable was printed.
func TestGateConfigReason(t *testing.T) {
	t.Parallel()
	// 255 ASCII bytes then a 3-byte rune: a byte clip at 256 would cut the
	// rune in half; a rune-safe clip backs off to 255.
	straddle := strings.Repeat("x", gateConfigReasonCap-1) + "\u20ac" + "tail"
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "empty", stderr: "", want: gateConfigReasonEmpty},
		{name: "blank lines only", stderr: "\n  \n\t\n", want: gateConfigReasonEmpty},
		{name: "first line wins", stderr: "first\nsecond\n", want: "first"},
		{name: "leading blank lines are skipped", stderr: "\n\n  actual reason  \nmore\n", want: "actual reason"},
		{name: "long line is clipped", stderr: strings.Repeat("x", gateConfigReasonCap+10), want: strings.Repeat("x", gateConfigReasonCap) + gateConfigReasonClipMarker},
		{name: "exactly at the cap is not clipped", stderr: strings.Repeat("x", gateConfigReasonCap), want: strings.Repeat("x", gateConfigReasonCap)},
		{name: "clip lands on a rune boundary", stderr: straddle, want: strings.Repeat("x", gateConfigReasonCap-1) + gateConfigReasonClipMarker},
		{name: "invalid utf-8 is replaced", stderr: "bad \xff byte\n", want: "bad \uFFFD byte"},
		{name: "control characters become spaces", stderr: "a\x00b\x1b[31mc\x07\n", want: "a b [31mc"},
		{name: "a line of only control characters is skipped", stderr: "\x00\x01\nreal\n", want: "real"},
		{name: "secret assignment is redacted", stderr: "env: API_KEY=sk-live-123 missing module\n", want: "env: API_KEY=" + execenv.Redacted + " missing module"},
		{name: "tab-separated secret is still redacted", stderr: "API_TOKEN\tsk-live-123 missing module\n", want: "API_TOKEN " + execenv.Redacted + " missing module"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := gateConfigReason(tc.stderr)
			if got != tc.want {
				t.Fatalf("gateConfigReason(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("gateConfigReason(%q) = %q is not valid UTF-8", tc.stderr, got)
			}
		})
	}
}

// TestGateConfigReasonRedactsControllerSecretValues pins that a secret value
// present in the controller's environment is scrubbed from the excerpt before
// it reaches gc.control_pending_reason and the control.stalled event.
func TestGateConfigReasonRedactsControllerSecretValues(t *testing.T) {
	t.Setenv("GC_TEST_GATE_SECRET_TOKEN", "hunter2-very-secret")
	// No sensitive keyword on the line: only the environment-value scrub can
	// catch this one.
	got := gateConfigReason("validator saw hunter2-very-secret in env\n")
	if strings.Contains(got, "hunter2-very-secret") {
		t.Fatalf("gateConfigReason leaked the controller secret: %q", got)
	}
	if !strings.Contains(got, execenv.Redacted) {
		t.Fatalf("gateConfigReason = %q, want the redaction marker", got)
	}
}

// TestRunRalphCheckExitConfigWinsOverInfraMarker pins the precedence between
// the two script protocols: a run that exits 78 is held even when its stderr
// carries one of the typed infrastructure strings ClassifyInfraBlind matches.
// The status the script chose wins over the stderr fallback, so the legacy
// lane must not spend infrastructure budget on it.
func TestRunRalphCheckExitConfigWinsOverInfraMarker(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "both.sh",
		"#!/bin/sh\necho 'Error 1045 (28000): Access denied for user' >&2\nexit 78\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)
	mustClose(t, store, run1.ID)

	_, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending (exit 78 beats the stderr fallback)", err)
	}
	if !strings.Contains(err.Error(), "Error 1045") {
		t.Fatalf("error %q, want the stderr line carried as the reason", err)
	}
	if v := mustGet(t, store, check1.ID).Metadata[beadmeta.CheckInfraRetryMetadataKey]; v != "" {
		t.Fatalf("gc.check_infra_retry = %q, want unset (the hold must not charge the infrastructure budget)", v)
	}
}

// TestRunRalphCheckOtherExitCodesAreNotHeld guards against over-classification:
// at the runRalphCheck level, every nonzero status other than 78 (an ordinary
// verdict, the transient 75 protocol, and 78's numeric neighbors) is returned
// as a graded GateFail for the caller to dispose of, never as the hold.
func TestRunRalphCheckOtherExitCodesAreNotHeld(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"1", "2", "75", "77", "79"} {
		t.Run("exit "+code, func(t *testing.T) {
			t.Parallel()
			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "exit.sh", "#!/bin/sh\necho 'not a config problem' >&2\nexit "+code+"\n")
			check := beads.Bead{ID: "check-" + code, Type: "task", Metadata: map[string]string{beadmeta.CheckPathMetadataKey: checkPath}}

			result, err := runRalphCheck(beads.NewMemStore(), check, beads.Bead{ID: "run-" + code, Type: "task"}, 1, ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("runRalphCheck error = %v, want a graded result", err)
			}
			if result.Outcome != convergence.GateFail {
				t.Fatalf("outcome = %s, want GateFail", result.Outcome)
			}
		})
	}
}

// TestProcessRalphControlCheckExitConfigStaysOpen covers the compiled
// kind=ralph lane, the one [steps.check] materializes to: a check exiting 78
// holds the control open without spending its attempt or spawning a successor
// iteration, and fixing the environment lets the next sweep pass it.
func TestProcessRalphControlCheckExitConfigStaysOpen(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := ".gc/scripts/checks/verify.sh"
	scriptPath := filepath.Join(cityPath, checkPath)
	writeExecutableScript(t, scriptPath, configExitScript)
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	opts := ProcessOptions{CityPath: cityPath}
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, control, opts)
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	assertUnlaunchableRalphControlHeldOpen(t, store, control.ID, beadsBefore)

	// The operator fixes the environment; the same script now passes.
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nexit 0\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl after the fix: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// TestProcessRalphControlCheckExitConfigThenVerdictSpendsOneAttempt pins that
// the hold is free: however many sweeps a step spent held on exit 78, the
// first real "not yet" verdict after the fix is still attempt 1, and the held
// sweeps left no entry in the attempt log.
func TestProcessRalphControlCheckExitConfigThenVerdictSpendsOneAttempt(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := ".gc/scripts/checks/build-artifact-valid.sh"
	scriptPath := filepath.Join(cityPath, checkPath)
	writeExecutableScript(t, scriptPath, configExitScript)
	store, control := newUnlaunchableCheckRalphControl(t, checkPath)
	opts := ProcessOptions{CityPath: cityPath}

	for i := range 3 {
		_, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
		if !errors.Is(err, ErrControlDriftPending) {
			t.Fatalf("sweep %d error = %v, want ErrControlDriftPending", i+1, err)
		}
	}
	if log := mustGet(t, store, control.ID).Metadata[beadmeta.AttemptLogMetadataKey]; log != "" {
		t.Fatalf("gc.attempt_log = %q after held sweeps, want empty", log)
	}

	// The environment is fixed and the check now reaches a real verdict. With
	// max_attempts = 1 that verdict exhausts the budget, so the step closes
	// failed with exactly one attempt-log entry: attempt 1, the real reason.
	writeExecutableScript(t, scriptPath, "#!/bin/sh\necho 'front matter missing required fields' >&2\nexit 1\n")
	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl with a real verdict: %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("result = %+v, want fail (budget of 1 spent by the real verdict)", result)
	}
	got := mustGet(t, store, control.ID)
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("control status=%q outcome=%q, want closed fail", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	var log []map[string]string
	if err := json.Unmarshal([]byte(got.Metadata[beadmeta.AttemptLogMetadataKey]), &log); err != nil {
		t.Fatalf("gc.attempt_log = %q: %v", got.Metadata[beadmeta.AttemptLogMetadataKey], err)
	}
	if len(log) != 1 {
		t.Fatalf("gc.attempt_log has %d entries, want exactly 1 (the held sweeps must not log): %v", len(log), log)
	}
	if log[0]["attempt"] != "1" {
		t.Errorf("attempt_log[0].attempt = %q, want \"1\"", log[0]["attempt"])
	}
	if !strings.Contains(log[0]["reason"], "front matter missing") {
		t.Errorf("attempt_log[0].reason = %q, want the real verdict", log[0]["reason"])
	}
	if strings.Contains(log[0]["reason"], "cannot import yaml") {
		t.Errorf("attempt_log[0].reason = %q, carries the held sweep's stderr", log[0]["reason"])
	}
}

// TestProcessRalphCheckExitConfigStaysOpen covers the legacy kind=check lane: a
// check exiting 78 holds the check and its logical bead open, spends no
// infrastructure budget, records no verdict, and clones no next attempt.
func TestProcessRalphCheckExitConfigStaysOpen(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "config.sh", configExitScript)
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)
	mustClose(t, store, run1.ID)
	beadsBefore := countAllBeads(t, store)

	_, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	for _, id := range []string{check1.ID, logical.ID} {
		if got := mustGet(t, store, id); got.Status == "closed" {
			t.Fatalf("bead %s closed (outcome=%q), want it held open", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
	got := mustGet(t, store, check1.ID)
	for _, key := range []string{
		beadmeta.CheckInfraRetryMetadataKey,
		beadmeta.OutcomeMetadataKey,
		beadmeta.ExitCodeMetadataKey,
		beadmeta.StderrMetadataKey,
	} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q, want unset (a configuration hold is neither an infrastructure blip nor a verdict)", key, v)
		}
	}
	if n := countAllBeads(t, store); n != beadsBefore {
		t.Errorf("bead count = %d, want %d (no attempt-2 run or check may be cloned)", n, beadsBefore)
	}
}

// TestProcessRalphCheckExitConfigKeepsPendingBudget pins that a held 78 does
// not reset the drift-pending budget the cmd layer stamped on earlier sweeps.
// persistCheckResult blanks gc.control_pending_* for every launched check; if
// the 78 path reached it, the stall budget would restart every sweep and
// control.stalled would never fire.
func TestProcessRalphCheckExitConfigKeepsPendingBudget(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "config.sh", configExitScript)
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)
	stamped := map[string]string{
		beadmeta.ControlPendingReasonMetadataKey:    "check-1: check exited 78 (EX_CONFIG): earlier sweep",
		beadmeta.ControlPendingCountMetadataKey:     "4",
		beadmeta.ControlPendingFirstSeenMetadataKey: "2026-10-01T00:00:00Z",
	}
	if err := store.SetMetadataBatch(check1.ID, stamped); err != nil {
		t.Fatalf("stamp pending budget: %v", err)
	}
	mustClose(t, store, run1.ID)

	_, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if !errors.Is(err, ErrControlDriftPending) {
		t.Fatalf("ProcessControl error = %v, want ErrControlDriftPending", err)
	}
	got := mustGet(t, store, check1.ID)
	for key, want := range stamped {
		if v := got.Metadata[key]; v != want {
			t.Errorf("%s = %q after a held sweep, want %q preserved", key, v, want)
		}
	}
}

// TestProcessRalphCheckExitConfigHealClearsPendingBudget pins the other side:
// once the environment is fixed and the check launches to a verdict, the
// pending budget and stall latch are cleared on the same write.
func TestProcessRalphCheckExitConfigHealClearsPendingBudget(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "healed.sh", "#!/bin/sh\nexit 0\n")
	store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 2)
	if err := store.SetMetadataBatch(check1.ID, map[string]string{
		beadmeta.ControlPendingReasonMetadataKey:    "check-1: check exited 78 (EX_CONFIG): python3 cannot import yaml",
		beadmeta.ControlPendingCountMetadataKey:     "7",
		beadmeta.ControlPendingFirstSeenMetadataKey: "2026-10-01T00:00:00Z",
		beadmeta.ControlPendingStalledMetadataKey:   "true",
	}); err != nil {
		t.Fatalf("stamp pending budget: %v", err)
	}
	mustClose(t, store, run1.ID)

	result, err := ProcessControl(store, mustGet(t, store, check1.ID), ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("result = %+v, want pass", result)
	}
	got := mustGet(t, store, check1.ID)
	for _, key := range controlPendingBudgetKeys {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q after the heal, want cleared", key, v)
		}
	}
}
