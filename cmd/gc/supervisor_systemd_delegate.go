package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Operator-facing env vars for systemd-delegated supervisor lifecycle.
//
// When GC_SUPERVISOR_SYSTEMD_UNIT names a systemd unit, `gc supervisor
// start`/`gc supervisor stop` and the `gc start` drift auto-restart path
// shell out to `systemctl {start,stop,try-restart} <unit>` instead of
// forking `gc supervisor run`, driving the destructive control-socket
// stop, or installing gc's own user service files. The delegated unit
// owns the supervisor lifecycle; gc only requests transitions and
// verifies their effect.
//
// Delegated-mode contract differences from the fork path:
//
//   - `gc supervisor stop` runs `systemctl stop <unit>` synchronously,
//     bounded by --wait-timeout whether or not --wait is set, then
//     verifies a previously-live supervisor actually exited. A live
//     supervisor that the unit does not manage fails the stop with its
//     PID instead of reporting a false "Supervisor stopped.". With no
//     live supervisor and an inactive unit, stop keeps the legacy
//     "supervisor is not running" exit-1 contract.
//   - `gc supervisor install`/`gc supervisor uninstall` never write or
//     load gc-owned service files for the delegated unit; install
//     refuses to run, and uninstall only touches gc's own legacy unit.
//   - `gc supervisor status` probes the delegated unit (not gc's own
//     user unit) when the control socket is unreachable.
//
// GC_SUPERVISOR_SYSTEMD_SCOPE selects the manager the unit lives in:
// "system" (the default) or "user" (systemctl --user). An invalid scope
// is a hard error on every lifecycle path, never a silent fallback.
const (
	supervisorSystemdUnitEnv  = "GC_SUPERVISOR_SYSTEMD_UNIT"
	supervisorSystemdScopeEnv = "GC_SUPERVISOR_SYSTEMD_SCOPE"
)

// delegatedStopVerifyTimeout bounds the post-stop liveness check in
// delegatedSupervisorStop. `systemctl stop` completes the unit's stop job
// synchronously, so a supervisor managed by the unit is already gone when
// systemctl returns; this budget only covers control-socket teardown
// slop. A supervisor still answering after it is one the unit never
// managed. Package var so tests can shrink the wait.
var delegatedStopVerifyTimeout = 5 * time.Second

// systemdDelegation names the operator-managed systemd unit that owns the
// supervisor lifecycle, plus the manager scope it lives in ("system" or
// "user").
type systemdDelegation struct {
	Unit  string
	Scope string
}

// supervisorSystemdDelegation reads the delegation env vars. ok is false
// when GC_SUPERVISOR_SYSTEMD_UNIT is unset or blank. An unrecognized
// scope value is an error rather than a silent fallback so a typo cannot
// quietly target the system manager.
func supervisorSystemdDelegation() (systemdDelegation, bool, error) {
	unit := strings.TrimSpace(os.Getenv(supervisorSystemdUnitEnv))
	if unit == "" {
		return systemdDelegation{}, false, nil
	}
	scope := strings.TrimSpace(os.Getenv(supervisorSystemdScopeEnv))
	switch scope {
	case "":
		scope = "system"
	case "system", "user":
	default:
		return systemdDelegation{}, false, fmt.Errorf("invalid %s=%q: want \"system\" or \"user\"", supervisorSystemdScopeEnv, scope)
	}
	return systemdDelegation{Unit: unit, Scope: scope}, true, nil
}

// systemctlArgs returns the systemctl argument vector (without the
// leading program name) for verb against the delegated unit.
func (d systemdDelegation) systemctlArgs(verb string) []string {
	if d.Scope == "user" {
		return []string{"--user", verb, d.Unit}
	}
	return []string{verb, d.Unit}
}

// systemctlIsActiveArgs returns the argument vector for a quiet
// is-active probe of the delegated unit at its configured scope.
func (d systemdDelegation) systemctlIsActiveArgs() []string {
	if d.Scope == "user" {
		return []string{"--user", "is-active", "--quiet", d.Unit}
	}
	return []string{"is-active", "--quiet", d.Unit}
}

// commandHint renders the operator-facing systemctl command line for verb
// against the delegated unit, e.g. "systemctl restart gascity.service".
func (d systemdDelegation) commandHint(verb string) string {
	return "systemctl " + strings.Join(d.systemctlArgs(verb), " ")
}

// delegatedUnitActive reports whether the delegated unit is currently
// active, via `systemctl [--user] is-active --quiet <unit>` resolved on
// PATH. A missing systemctl binary or unreachable manager reads as
// inactive.
//
// Decision: delegated paths exec PATH-resolved systemctl directly instead
// of routing through the supervisorSystemctlRun hook, and the PATH-shim
// fake systemctl (argv-recording) is the canonical test seam for them.
// The bounded stop needs CommandContext+WaitDelay semantics the hook
// cannot express, and the shim exercises the real exec path end to end.
func delegatedUnitActive(d systemdDelegation) bool {
	return exec.Command("systemctl", d.systemctlIsActiveArgs()...).Run() == nil
}

// runDelegatedSystemctl invokes systemctl (resolved via PATH) for verb
// against the delegated unit, folding any output into the returned error
// so operators see systemd's own diagnostic.
func runDelegatedSystemctl(d systemdDelegation, verb string) error {
	return runDelegatedSystemctlTimeout(d, verb, 0)
}

// runDelegatedSystemctlTimeout is runDelegatedSystemctl bounded by
// timeout (unbounded when timeout <= 0). systemctl runs unit jobs
// synchronously, so the bound keeps a wedged unit from holding the CLI
// past its advertised budget; the unit's own job keeps running inside
// systemd after the CLI gives up waiting.
func runDelegatedSystemctlTimeout(d systemdDelegation, verb string, timeout time.Duration) error {
	args := d.systemctlArgs(verb)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	// Don't let an inherited pipe from a systemctl child stretch the wait
	// past the kill triggered by the context deadline.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("systemctl %s: timed out after %s", strings.Join(args, " "), timeout)
		}
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// supervisorRestartGuidance returns the systemctl command operators
// should run to restart the supervisor by hand: the delegated unit's
// command when GC_SUPERVISOR_SYSTEMD_UNIT is configured, otherwise gc's
// default user unit. An invalid delegation scope yields guidance naming
// the bad value rather than silently pointing at the default unit. Used
// by drift remediation messages.
func supervisorRestartGuidance() string {
	d, ok, err := supervisorSystemdDelegation()
	switch {
	case err != nil:
		return fmt.Sprintf("fix %v", err)
	case ok:
		return d.commandHint("restart")
	}
	return "systemctl --user restart gascity-supervisor"
}

// supervisorStatusGuidance is supervisorRestartGuidance for `systemctl
// status`.
func supervisorStatusGuidance() string {
	d, ok, err := supervisorSystemdDelegation()
	switch {
	case err != nil:
		return fmt.Sprintf("fix %v", err)
	case ok:
		return d.commandHint("status")
	}
	return "systemctl --user status gascity-supervisor"
}

// delegatedSupervisorStart starts the supervisor by asking the
// operator-managed systemd unit to start, then waits for the control
// socket to answer — the same readiness contract as the fork path.
func delegatedSupervisorStart(d systemdDelegation, stdout, stderr io.Writer, jsonOut bool) int {
	if pid := supervisorAliveHook(); pid != 0 {
		fmt.Fprintf(stderr, "gc supervisor start: supervisor already running (PID %d)\n", pid) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := runDelegatedSystemctl(d, "start"); err != nil {
		fmt.Fprintf(stderr, "gc supervisor start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	deadline := time.Now().Add(supervisorReadyTimeout)
	for time.Now().Before(deadline) {
		if pid := supervisorAliveHook(); pid != 0 {
			if jsonOut {
				return writeLifecycleActionJSONOrExit(stdout, stderr, "gc supervisor start", lifecycleActionJSON{
					Command:       "supervisor start",
					Action:        "start",
					Message:       "Supervisor started.",
					SupervisorPID: pid,
				})
			}
			fmt.Fprintf(stdout, "Supervisor started (PID %d)\n", pid) //nolint:errcheck // best-effort stdout
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}
	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready after '%s'; check '%s'\n", d.commandHint("start"), d.commandHint("status")) //nolint:errcheck // best-effort stderr
	return 1
}

// delegatedSupervisorStop stops the supervisor by asking the
// operator-managed systemd unit to stop. The systemctl invocation is
// synchronous and bounded by waitTimeout; the destructive socket stop and
// service unload are intentionally skipped because the delegated unit
// owns the lifecycle (and its restart policy).
//
// `systemctl stop` succeeds without doing anything when the running
// supervisor is not managed by the unit (the common hazard mid-migration,
// e.g. a legacy forked supervisor still holding the control socket), so a
// supervisor that was alive before the stop must be verifiably gone
// afterwards or the command fails with its PID. With no live supervisor
// and an inactive unit, the legacy "supervisor is not running" exit-1
// contract is preserved.
func delegatedSupervisorStop(d systemdDelegation, stdout, stderr io.Writer, wait bool, waitTimeout time.Duration, jsonOut bool) int {
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Second
	}
	pidBefore := supervisorAliveHook()
	if pidBefore == 0 && !delegatedUnitActive(d) {
		fmt.Fprintf(stderr, "gc supervisor stop: supervisor is not running (delegated unit %s is inactive)\n", d.Unit) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := runDelegatedSystemctlTimeout(d, "stop", waitTimeout); err != nil {
		fmt.Fprintf(stderr, "gc supervisor stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pidBefore != 0 {
		deadline := time.Now().Add(delegatedStopVerifyTimeout)
		for supervisorAliveHook() != 0 && time.Now().Before(deadline) {
			time.Sleep(supervisorReadyPollInterval)
		}
		if pid := supervisorAliveHook(); pid != 0 {
			fmt.Fprintf(stderr, "gc supervisor stop: supervisor still running (PID %d) outside delegated unit %s after '%s'; it is not managed by that unit — stop it with %s unset, or fix the delegation env\n", pid, d.Unit, d.commandHint("stop"), supervisorSystemdUnitEnv) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if jsonOut {
		return writeSupervisorStopSuccess(stdout, stderr, wait)
	}
	fmt.Fprintln(stdout, "Supervisor stopped.") //nolint:errcheck // best-effort stdout
	return 0
}
