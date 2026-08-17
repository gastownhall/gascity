package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// delegatedSystemctlJobTimeout bounds the delegated `systemctl start`
// and `systemctl try-restart` invocations. systemctl waits for the
// unit's job synchronously, so without a CLI-side bound a unit with
// TimeoutStartSec=infinity or a wedged manager/D-Bus connection holds
// `gc supervisor start`, `gc start`, and drift remediation forever —
// the failure mode the bounded delegated stop already defends against.
// The budget sits above systemd's default 90s job timeout so a
// default-configured unit surfaces systemd's own diagnostic first.
// Killing systemctl at the bound does not cancel the job: it keeps
// running inside systemd. runDelegatedSystemctlTimeout reports that bound
// as a delegatedSystemctlTimeoutError so the start, ensure, and drift
// try-restart callers fall through into their own readiness or drift
// verification — which observe a late start — instead of treating the
// bounded wait as a terminal systemctl failure. Package var so tests can
// shrink the wait.
var delegatedSystemctlJobTimeout = 2 * time.Minute

// hostSystemctlDirs lists the standard system binary directories a
// PATH-resolved systemctl must not come from while running inside a
// test binary. Tests that exercise delegated paths install an
// argv-recording shim in a temp dir (installFakeDelegatedSystemctl);
// resolving past it means the test is about to drive the host's real
// systemd.
var hostSystemctlDirs = []string{
	"/bin/",
	"/sbin/",
	"/usr/bin/",
	"/usr/sbin/",
	"/usr/local/bin/",
	"/usr/local/sbin/",
}

// delegatedSystemctlPath resolves the systemctl binary the delegated
// paths exec. Delegated paths intentionally bypass the
// supervisorSystemctlRun hook (see delegatedUnitActive), so the PATH
// shim is the only test seam; this resolver backstops it by refusing —
// in test binaries only — to return the host's real systemctl. Same
// must-defend hazard class as guardSupervisorSocketDir: a test that
// reaches a delegated exec without the shim installed would stop or
// restart the operator's production supervisor unit. Production
// behavior is unchanged (exec.Command performs the same PATH lookup).
func delegatedSystemctlPath() string {
	resolved, err := exec.LookPath("systemctl")
	if err != nil {
		// Let exec.Command surface its standard not-found error.
		return "systemctl"
	}
	guardDelegatedSystemctlPath(resolved)
	return resolved
}

// guardDelegatedSystemctlPath panics when a test binary resolved
// systemctl into a host system directory instead of a test-installed
// PATH shim. No-op outside test binaries.
func guardDelegatedSystemctlPath(resolved string) {
	if !isTestBinary() {
		return
	}
	dir := filepath.Dir(resolved) + string(filepath.Separator)
	for _, sys := range hostSystemctlDirs {
		if dir == sys {
			panic("delegated systemctl exec: refusing to run host systemctl (" + resolved + ") from a test binary; install a fake via installFakeDelegatedSystemctl or unset " + supervisorSystemdUnitEnv)
		}
	}
}

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
// quietly target the system manager, and a non-Linux platform is an
// error rather than a low-level "systemctl: executable file not found"
// once a lifecycle path execs — delegation is a systemd contract.
func supervisorSystemdDelegation() (systemdDelegation, bool, error) {
	unit := strings.TrimSpace(os.Getenv(supervisorSystemdUnitEnv))
	if unit == "" {
		return systemdDelegation{}, false, nil
	}
	if supervisorRuntimeGOOS != "linux" {
		return systemdDelegation{}, false, fmt.Errorf("%s is set but systemd delegation requires linux (running on %s); unset it and use the platform service manager", supervisorSystemdUnitEnv, supervisorRuntimeGOOS)
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

// delegatedIsActiveTimeout bounds the delegated `systemctl is-active`
// probe. is-active is a quick unit-state query, but the same wedged
// manager or D-Bus connection that makes a delegated `systemctl
// start`/`try-restart` block past delegatedSystemctlJobTimeout also blocks
// is-active. The is-active fallback runs precisely after such a bounded
// start/restart timeout — inside delegatedLivenessWithoutSocket and the
// delegated `gc supervisor status` service-manager probe — so without a
// CLI-side bound it would hang `gc supervisor start`, `gc start`, and `gc
// supervisor status` forever, defeating the bound the mutating call just
// enforced and starving the supervisor-API fallback that follows it.
// Package var so tests can shrink the wait.
var delegatedIsActiveTimeout = 10 * time.Second

// delegatedUnitActive reports whether the delegated unit is currently
// active, via `systemctl [--user] is-active --quiet <unit>` resolved on
// PATH and bounded by delegatedIsActiveTimeout. A missing systemctl
// binary, an unreachable manager, or a probe that exceeds the bound reads
// as inactive, so callers fall through to the next liveness signal (the
// supervisor API) instead of blocking on a wedged manager.
//
// Decision: delegated paths exec PATH-resolved systemctl directly instead
// of routing through the supervisorSystemctlRun hook, and the PATH-shim
// fake systemctl (argv-recording) is the canonical test seam for them.
// The bounded stop and this bounded probe need CommandContext+WaitDelay
// semantics the hook cannot express, and the shim exercises the real exec
// path end to end. delegatedSystemctlPath backstops the seam in test
// binaries.
func delegatedUnitActive(d systemdDelegation) bool {
	ctx := context.Background()
	if delegatedIsActiveTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, delegatedIsActiveTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, delegatedSystemctlPath(), d.systemctlIsActiveArgs()...)
	// Don't let an inherited pipe from a systemctl child stretch the wait
	// past the kill the context deadline triggers.
	cmd.WaitDelay = time.Second
	return cmd.Run() == nil
}

// delegatedSystemctlTimeoutError reports that a delegated systemctl
// invocation hit its CLI-side bound (delegatedSystemctlJobTimeout) before
// systemctl returned. systemctl's unit job keeps running inside systemd
// after the CLI stops waiting, so this is a bounded-wait outcome —
// distinct from an ordinary systemctl failure — that callers with their
// own readiness or drift verification should fall through into rather
// than treat as terminal. Non-timeout systemctl errors stay terminal.
type delegatedSystemctlTimeoutError struct {
	args    string
	timeout time.Duration
}

// Error implements error.
func (e *delegatedSystemctlTimeoutError) Error() string {
	return fmt.Sprintf("systemctl %s: timed out after %s", e.args, e.timeout)
}

// isDelegatedSystemctlTimeout reports whether err is a bounded-wait
// timeout from runDelegatedSystemctlTimeout, as opposed to an ordinary
// systemctl failure. Callers with their own post-start readiness or drift
// verification use it to continue into that verification on timeout
// instead of returning failure before a late systemd success can be
// observed.
func isDelegatedSystemctlTimeout(err error) bool {
	var timeoutErr *delegatedSystemctlTimeoutError
	return errors.As(err, &timeoutErr)
}

// runDelegatedSystemctlTimeout invokes systemctl (resolved via PATH) for
// verb against the delegated unit, bounded by timeout (unbounded when
// timeout <= 0), folding any output into the returned error so operators
// see systemd's own diagnostic. systemctl runs unit jobs synchronously,
// so the bound keeps a wedged unit from holding the CLI past its
// advertised budget; the unit's own job keeps running inside systemd
// after the CLI gives up waiting. A timeout returns a
// *delegatedSystemctlTimeoutError (see isDelegatedSystemctlTimeout) so
// callers with their own readiness or drift verification can distinguish
// the bounded wait from an ordinary systemctl failure; every other
// failure is returned as an ordinary error.
func runDelegatedSystemctlTimeout(d systemdDelegation, verb string, timeout time.Duration) error {
	args := d.systemctlArgs(verb)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, delegatedSystemctlPath(), args...)
	// Don't let an inherited pipe from a systemctl child stretch the wait
	// past the kill triggered by the context deadline.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return &delegatedSystemctlTimeoutError{args: strings.Join(args, " "), timeout: timeout}
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
// own user unit via supervisorSystemdServiceName, which carries the
// GC_HOME-isolation suffix when one applies. An invalid delegation
// scope yields guidance naming the bad value rather than silently
// pointing at the default unit. Used by drift remediation messages.
func supervisorRestartGuidance() string {
	d, ok, err := supervisorSystemdDelegation()
	switch {
	case err != nil:
		return fmt.Sprintf("fix %v", err)
	case ok:
		return d.commandHint("restart")
	}
	return "systemctl --user restart " + supervisorSystemdServiceName()
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
	return "systemctl --user status " + supervisorSystemdServiceName()
}

// delegatedSupervisorStart starts the supervisor by asking the
// operator-managed systemd unit to start, then waits for the control
// socket to answer — the same readiness contract as the fork path.
//
// A system-scope unit running under another uid (the documented default
// topology) keeps its control socket unreachable from the operator's
// shell, so a socket that never answers is not evidence the start
// failed. After the socket poll times out, liveness falls back to the
// same evidence chain `gc supervisor status` trusts (gascity#2984): the
// delegated unit's is-active state, then the supervisor HTTP API. Only
// when all three are silent does start report failure.
func delegatedSupervisorStart(d systemdDelegation, stdout, stderr io.Writer, jsonOut bool) int {
	if pid := supervisorAliveHook(); pid != 0 {
		fmt.Fprintf(stderr, "gc supervisor start: supervisor already running (PID %d)\n", pid) //nolint:errcheck // best-effort stderr
		return 1
	}
	// A bounded systemctl-start timeout is not terminal: the start job can
	// still complete inside systemd after the CLI stops waiting, so fall
	// through to the same readiness poll and is-active/API fallback that
	// confirm a late start. Only an ordinary systemctl failure is terminal.
	if err := runDelegatedSystemctlTimeout(d, "start", delegatedSystemctlJobTimeout); err != nil && !isDelegatedSystemctlTimeout(err) {
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
			printDashboardStartHint(stdout)
			return 0
		}
		time.Sleep(supervisorReadyPollInterval)
	}
	if pidSource := delegatedLivenessWithoutSocket(d); pidSource != "" {
		if jsonOut {
			return writeLifecycleActionJSONOrExit(stdout, stderr, "gc supervisor start", lifecycleActionJSON{
				Command:   "supervisor start",
				Action:    "start",
				Message:   "Supervisor started.",
				PIDSource: pidSource,
			})
		}
		fmt.Fprintf(stdout, "Supervisor started (pid unavailable: control socket unreachable; liveness confirmed via %s)\n", pidSource) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stderr, "gc supervisor start: supervisor did not become ready after '%s'; check '%s'\n", d.commandHint("start"), d.commandHint("status")) //nolint:errcheck // best-effort stderr
	return 1
}

// delegatedLivenessWithoutSocket reports how a delegated supervisor
// whose control socket is unreachable can still be confirmed live:
// "service_manager" when the delegated unit is active, "api" when the
// supervisor HTTP API answers, "" when neither does. The order and
// naming mirror the supervisorStatusWithOptions fallback so start and
// status agree about the same supervisor.
func delegatedLivenessWithoutSocket(d systemdDelegation) string {
	switch {
	case delegatedUnitActive(d):
		return "service_manager"
	case supervisorAPIReachable():
		return "api"
	}
	return ""
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
