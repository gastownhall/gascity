// Package systemdscope places child processes into a systemd user slice by
// wrapping their command line in a transient systemd scope.
//
// The wrapper shape is:
//
//	systemd-run --user --scope --slice=<slice> --collect --quiet -- <command>
//
// Two properties of `--scope` make this safe for callers that track the child
// by PID: systemd-run registers the scope and then execs the command *in
// place*, so the PID the caller observed from exec.Cmd.Start is the PID the
// wrapped process keeps; and `--quiet` keeps systemd-run's own chatter off the
// child's stdout, which some callers use for a startup handshake. Placement is
// inherited across fork, so wrapping a supervisor also places everything it
// spawns.
//
// The default Wrapper entry point degrades gracefully: a host without
// systemd-run, without a reachable user bus, or with an empty slice runs the
// command unwrapped. WrapRequired is the fail-closed boundary for callers that
// must prove placement.
package systemdscope

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds the one-time systemd-run availability probe.
const probeTimeout = 5 * time.Second

// failureRetryInterval is how long a probe failure is trusted before the next
// call re-probes.
//
// Success is cached for the life of the Wrapper — a user manager that answered
// once does not stop existing. Failure is not, because the callers are not all
// short-lived: `gc supervisor run` stays up for weeks, and a transient bus
// hiccup during its first spawn would otherwise disable placement for the
// entire lifetime of the process, silently, behind one log line. That is the
// failure this package exists to prevent, so it must not be self-inflicted.
// Test-overridable.
var failureRetryInterval = 10 * time.Minute

// Argv returns command wrapped in a transient systemd user scope on slice, or
// command unchanged when either slice or command is empty. It does not check
// whether systemd-run is usable — see [Wrapper] for the probed form.
func Argv(slice string, command []string) []string {
	if slice == "" || len(command) == 0 {
		return command
	}
	wrapped := make([]string, 0, len(command)+7)
	wrapped = append(wrapped, "systemd-run", "--user", "--scope",
		"--slice="+slice, "--collect", "--quiet", "--")
	return append(wrapped, command...)
}

// Probe verifies that systemd-run exists and the systemd user manager responds
// by running a no-op command in a transient scope on the target slice.
func Probe(slice string) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run",
		"--user", "--scope", "--slice="+slice, "--collect", "--quiet", "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("systemd user manager probe failed: %w: %s", err, msg)
		}
		return fmt.Errorf("systemd user manager probe failed: %w", err)
	}
	return nil
}

// verdict is a cached probe result for one slice.
type verdict struct {
	ok      bool
	err     error
	checked time.Time
}

// Wrapper decides whether commands are wrapped in a transient systemd user
// scope, caching the availability probe per slice so a retry loop does not pay
// for it repeatedly.
//
// The zero value is usable: it probes with [Probe] and warns to the standard
// logger.
type Wrapper struct {
	Probe func(slice string) error // test seam; nil means [Probe]
	Warn  io.Writer                // test seam; nil means the standard logger
	Label string                   // knob named in the warning, e.g. "GC_DOLT_SLICE"

	mu       sync.Mutex
	verdicts map[string]verdict
}

// Wrap returns command placed in slice, or command unchanged when slice or
// command is empty or transient user scopes are unavailable on this host.
func (w *Wrapper) Wrap(slice string, command []string) []string {
	if len(command) == 0 || !w.Available(slice) {
		return command
	}
	return Argv(slice, command)
}

// WrapRequired returns command placed in slice, or an error when placement
// cannot be proved. Unlike [Wrapper.Wrap], it never degrades to an unwrapped
// command.
func (w *Wrapper) WrapRequired(slice string, command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("systemd scope: command is empty")
	}
	if strings.TrimSpace(slice) == "" {
		return nil, fmt.Errorf("systemd scope: required slice is empty")
	}
	if err := w.check(slice, false); err != nil {
		return nil, fmt.Errorf("systemd scope: required placement in %s: %w", slice, err)
	}
	return Argv(slice, command), nil
}

// Available reports whether transient user scopes can be used for slice.
//
// The verdict is cached per slice: a success is reused for the life of the
// Wrapper, a failure only until failureRetryInterval elapses. Callers that need
// a shape other than argv — a shell string for a tmux pane, say — gate on this
// and then build with [Argv]. An empty slice is never available and never
// probes, so it cannot populate the cache.
//
// The lock is deliberately held across the probe. That serializes callers
// racing on the same slice so the loser reuses the verdict instead of firing a
// second systemd-run, which is the case that actually occurs: each Wrapper
// reads one env-derived slice, so its callers contend on that one value.
// Releasing the lock while probing would trade that guard away to unblock
// concurrent callers for *different* slices, which no production caller has.
func (w *Wrapper) Available(slice string) bool {
	if slice == "" {
		return false
	}
	return w.check(slice, true) == nil
}

func (w *Wrapper) check(slice string, warn bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cached, seen := w.verdicts[slice]; seen && (cached.ok || time.Since(cached.checked) < failureRetryInterval) {
		return cached.err
	}
	probe := w.Probe
	if probe == nil {
		probe = Probe
	}
	err := probe(slice)
	if w.verdicts == nil {
		w.verdicts = make(map[string]verdict, 1)
	}
	w.verdicts[slice] = verdict{ok: err == nil, err: err, checked: time.Now()}
	if err != nil && warn {
		w.warn(slice, err)
	}
	return err
}

// warn reports a probe failure once per slice per retry window.
func (w *Wrapper) warn(slice string, err error) {
	label := w.Label
	if label == "" {
		label = "systemd slice"
	}
	msg := fmt.Sprintf("%s=%q set but transient user scopes are unavailable; command runs unwrapped: %v",
		label, slice, err)
	if w.Warn != nil {
		_, _ = fmt.Fprintln(w.Warn, "gc: "+msg)
		return
	}
	log.Printf("systemd scope: %s", msg)
}

// SlicePolicy is the resource policy applied to a systemd user slice.
type SlicePolicy struct {
	MemoryMax            string
	MemoryLow            string
	ManagedOOMPreference string
}

func (p SlicePolicy) assignments() ([]string, error) {
	values := []struct {
		name  string
		value string
	}{
		{name: "MemoryMax", value: p.MemoryMax},
		{name: "MemoryLow", value: p.MemoryLow},
		{name: "ManagedOOMPreference", value: p.ManagedOOMPreference},
	}
	assignments := make([]string, 0, len(values))
	for _, item := range values {
		value := strings.TrimSpace(item.value)
		if value == "" {
			return nil, fmt.Errorf("systemd slice policy: %s is empty", item.name)
		}
		assignments = append(assignments, item.name+"="+value)
	}
	return assignments, nil
}

// CommandRunner executes one command and returns its combined output.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Manager performs fail-closed systemd user-manager operations.
//
// The zero value executes real commands. Run is a narrow test seam.
type Manager struct {
	Run CommandRunner
}

func (m Manager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if m.Run != nil {
		return m.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// EnsureSlice applies policy to slice and verifies the effective properties
// before returning.
func (m Manager) EnsureSlice(ctx context.Context, slice string, policy SlicePolicy) error {
	if !strings.HasSuffix(slice, ".slice") {
		return fmt.Errorf("systemd slice: invalid unit %q", slice)
	}
	assignments, err := policy.assignments()
	if err != nil {
		return err
	}
	args := []string{"--user", "set-property", "--runtime", slice}
	args = append(args, assignments...)
	if out, err := m.run(ctx, "systemctl", args...); err != nil {
		return commandError("apply systemd slice policy", err, out)
	}

	showArgs := []string{
		"--user", "show", slice,
		"--property=MemoryMax",
		"--property=MemoryLow",
		"--property=ManagedOOMPreference",
	}
	out, err := m.run(ctx, "systemctl", showArgs...)
	if err != nil {
		return commandError("verify systemd slice policy", err, out)
	}
	actual := make(map[string]string, len(assignments))
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			actual[key] = value
		}
	}
	var mismatches []string
	for _, expected := range assignments {
		key, value, _ := strings.Cut(expected, "=")
		if actual[key] != value {
			mismatches = append(mismatches, key+"="+actual[key]+" (want "+value+")")
		}
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("verify systemd slice policy: %s", strings.Join(mismatches, ", "))
	}
	return nil
}

// StartScopeWithProcesses atomically creates a transient scope containing the
// supplied existing processes.
func (m Manager) StartScopeWithProcesses(
	ctx context.Context,
	unit string,
	slice string,
	description string,
	pids []int,
) error {
	if !strings.HasSuffix(unit, ".scope") {
		return fmt.Errorf("systemd scope: invalid unit %q", unit)
	}
	if !strings.HasSuffix(slice, ".slice") {
		return fmt.Errorf("systemd scope: invalid slice %q", slice)
	}
	if len(pids) == 0 {
		return fmt.Errorf("systemd scope: no processes supplied")
	}
	seen := make(map[int]struct{}, len(pids))
	pidArgs := make([]string, 0, len(pids))
	for _, pid := range pids {
		if pid <= 1 {
			return fmt.Errorf("systemd scope: invalid pid %d", pid)
		}
		if _, duplicate := seen[pid]; duplicate {
			return fmt.Errorf("systemd scope: duplicate pid %d", pid)
		}
		seen[pid] = struct{}{}
		pidArgs = append(pidArgs, strconv.Itoa(pid))
	}

	args := []string{
		"--user", "call",
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"StartTransientUnit",
		"ssa(sv)a(sa(sv))",
		unit,
		"fail",
		"4",
		"PIDs", "au", strconv.Itoa(len(pidArgs)),
	}
	args = append(args, pidArgs...)
	args = append(args,
		"Slice", "s", slice,
		"Description", "s", description,
		"CollectMode", "s", "inactive-or-failed",
		"0",
	)
	if out, err := m.run(ctx, "busctl", args...); err != nil {
		return commandError("start transient systemd scope", err, out)
	}
	return nil
}

func commandError(action string, err error, output []byte) error {
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("%s: %w: %s", action, err, detail)
	}
	return fmt.Errorf("%s: %w", action, err)
}
