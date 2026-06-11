package main

import (
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
// owns the supervisor lifecycle; gc only requests transitions and waits
// for readiness.
//
// GC_SUPERVISOR_SYSTEMD_SCOPE selects the manager the unit lives in:
// "system" (the default) or "user" (systemctl --user).
const (
	supervisorSystemdUnitEnv  = "GC_SUPERVISOR_SYSTEMD_UNIT"
	supervisorSystemdScopeEnv = "GC_SUPERVISOR_SYSTEMD_SCOPE"
)

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

// commandHint renders the operator-facing systemctl command line for verb
// against the delegated unit, e.g. "systemctl restart gascity.service".
func (d systemdDelegation) commandHint(verb string) string {
	return "systemctl " + strings.Join(d.systemctlArgs(verb), " ")
}

// runDelegatedSystemctl invokes systemctl (resolved via PATH) for verb
// against the delegated unit, folding any output into the returned error
// so operators see systemd's own diagnostic.
func runDelegatedSystemctl(d systemdDelegation, verb string) error {
	args := d.systemctlArgs(verb)
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
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
// default user unit. Used by drift remediation messages.
func supervisorRestartGuidance() string {
	if d, ok, err := supervisorSystemdDelegation(); err == nil && ok {
		return d.commandHint("restart")
	}
	return "systemctl --user restart gascity-supervisor"
}

// supervisorStatusGuidance is supervisorRestartGuidance for `systemctl
// status`.
func supervisorStatusGuidance() string {
	if d, ok, err := supervisorSystemdDelegation(); err == nil && ok {
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
// operator-managed systemd unit to stop. systemctl blocks until the unit
// has stopped, so the control-socket wait protocol is unnecessary; the
// destructive socket stop and service unload are intentionally skipped
// because the delegated unit owns the lifecycle (and its restart policy).
func delegatedSupervisorStop(d systemdDelegation, stdout, stderr io.Writer, wait bool, jsonOut bool) int {
	if err := runDelegatedSystemctl(d, "stop"); err != nil {
		fmt.Fprintf(stderr, "gc supervisor stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if jsonOut {
		return writeSupervisorStopSuccess(stdout, stderr, wait)
	}
	fmt.Fprintln(stdout, "Supervisor stopped.") //nolint:errcheck // best-effort stdout
	return 0
}
