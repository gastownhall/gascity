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
// Every entry point degrades gracefully: a host without systemd-run, without a
// reachable user bus, or with an empty slice runs the command unwrapped rather
// than failing to start it.
package systemdscope

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
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
	w.mu.Lock()
	defer w.mu.Unlock()
	if cached, seen := w.verdicts[slice]; seen && (cached.ok || time.Since(cached.checked) < failureRetryInterval) {
		return cached.ok
	}
	probe := w.Probe
	if probe == nil {
		probe = Probe
	}
	err := probe(slice)
	if w.verdicts == nil {
		w.verdicts = make(map[string]verdict, 1)
	}
	w.verdicts[slice] = verdict{ok: err == nil, checked: time.Now()}
	if err != nil {
		w.warn(slice, err)
	}
	return err == nil
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
