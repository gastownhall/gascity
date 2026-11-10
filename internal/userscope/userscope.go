// Package userscope runs commands in transient systemd user scopes.
//
// A transient scope created with
//
//	systemd-run --user --scope --slice=<slice> --collect --quiet -- <argv>
//
// moves the command's process tree into its own cgroup under <slice>, so a
// cgroup-wide kill of whatever scope the caller runs in does not reach it.
// systemd-run in scope mode registers its own PID and then execs argv, so
// the spawned PID is the command's PID.
//
// When no systemd user manager is reachable (no systemd-run, no user bus,
// non-Linux hosts), wrapping is a no-op: commands run unwrapped exactly as
// they would without this package.
package userscope

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds the availability probe. Test-overridable.
var probeTimeout = 5 * time.Second

// Argv returns argv prefixed with the systemd-run invocation that places it
// in a transient user scope under slice. It does not probe availability;
// use [Wrapper.Wrap] for the guarded form.
func Argv(slice string, argv []string) []string {
	out := make([]string, 0, 7+len(argv))
	out = append(out, "systemd-run", "--user", "--scope", "--slice="+slice, "--collect", "--quiet", "--")
	return append(out, argv...)
}

// Probe verifies that systemd-run exists and the systemd user manager
// responds by running a no-op command in a transient scope on slice. The
// probe runs in the calling process's environment, so it is representative
// of spawns that inherit that environment.
func Probe(slice string) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	argv := Argv(slice, []string{"true"})
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("systemd user manager probe failed: %w: %s", err, msg)
		}
		return fmt.Errorf("systemd user manager probe failed: %w", err)
	}
	return nil
}

// Wrapper decides once whether commands are wrapped in a transient user
// scope. The availability probe runs at most once per Wrapper, on the first
// call with a non-empty slice; on failure Unavailable is told once and all
// subsequent commands run unwrapped (graceful fallback). The zero value is
// ready to use and falls back silently.
type Wrapper struct {
	// Probe checks availability for a slice; nil means [Probe].
	Probe func(slice string) error
	// Unavailable is called once when the probe fails; nil means the
	// fallback is silent.
	Unavailable func(slice string, err error)

	once sync.Once
	ok   bool
}

// Active reports whether commands are wrapped for slice: it is non-empty
// and transient user scopes are available on this host. The first
// non-empty slice decides availability for the Wrapper's lifetime.
func (w *Wrapper) Active(slice string) bool {
	if slice == "" {
		return false
	}
	w.once.Do(func() {
		probe := w.Probe
		if probe == nil {
			probe = Probe
		}
		if err := probe(slice); err != nil {
			if w.Unavailable != nil {
				w.Unavailable(slice, err)
			}
			return
		}
		w.ok = true
	})
	return w.ok
}

// Wrap returns argv wrapped in a transient user scope under slice, or argv
// unchanged when argv or slice is empty or user scopes are unavailable.
func (w *Wrapper) Wrap(slice string, argv []string) []string {
	if len(argv) == 0 || !w.Active(slice) {
		return argv
	}
	return Argv(slice, argv)
}
