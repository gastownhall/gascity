package main

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// SupervisorStatus is the subset of supervisor health/status information
// that the drift detector consumes. It is intentionally a small struct
// rather than a re-export of the full health response so that drift
// detection stays decoupled from the wire types.
type SupervisorStatus struct {
	// BuildID identifies the binary the running supervisor was built from.
	// Typically a short git commit hash. Empty when the supervisor binary
	// predates buildID exposure.
	BuildID string

	// UptimeSec is the supervisor's reported uptime in seconds. Used to
	// derive the `started=` token on the operator-facing identity line.
	UptimeSec int

	// PackRoots reports the supervisor's view of pack roots and when each
	// was last parsed. The drift detector compares ParsedAt to on-disk
	// mtime to determine whether the operator edited a pack since the
	// supervisor parsed it.
	PackRoots []PackRootStatus
}

// PackRootStatus describes a single pack root and the time the supervisor
// last parsed it.
type PackRootStatus struct {
	Dir      string
	ParsedAt time.Time
}

// SupervisorClient is the supervisor API surface required by drift
// detection. Implementations may be HTTP clients against the running
// supervisor's socket or test fakes.
type SupervisorClient interface {
	// Status returns the supervisor's reported status (build identity,
	// pack roots).
	Status(ctx context.Context) (SupervisorStatus, error)
	// Ping returns nil when the supervisor is responsive.
	Ping(ctx context.Context) error
}

// DetectBinaryDrift returns true when the locally-installed gc binary's
// build identity differs from the supervisor's reported build identity.
//
// Either side reporting an empty BuildID is treated as "unknown — cannot
// compare" and returns false. The caller is expected to fall back to a
// secondary signal (mtime comparison) when both buildIDs are absent.
func DetectBinaryDrift(localBuildID string, sv SupervisorStatus) bool {
	if localBuildID == "" || sv.BuildID == "" {
		return false
	}
	return localBuildID != sv.BuildID
}

// DetectPackDrift returns the directories whose newest file mtime is
// later than the supervisor's recorded ParsedAt, indicating the operator
// has edited a pack since the supervisor last parsed it.
//
// A pack root with a zero ParsedAt is skipped (no parse time to compare
// against). A missing directory is reported as an error so the caller
// can surface a clear message rather than silently treating it as
// no-drift.
func DetectPackDrift(packRoots []PackRootStatus) ([]string, error) {
	var drifted []string
	for _, root := range packRoots {
		if root.ParsedAt.IsZero() {
			continue
		}
		newest, err := walkNewestMtime(root.Dir)
		if err != nil {
			return nil, fmt.Errorf("pack root %q: %w", root.Dir, err)
		}
		if newest.After(root.ParsedAt) {
			drifted = append(drifted, root.Dir)
		}
	}
	return drifted, nil
}

// walkNewestMtime returns the newest mtime among regular files within
// dir. Directory mtimes are ignored. Returns an error if dir does not
// exist or cannot be walked.
func walkNewestMtime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

// PollReady polls the supervisor's Ping endpoint until it returns nil
// or the timeout elapses. Returns the last error if the timeout is
// exceeded without a successful ping.
func PollReady(client SupervisorClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var lastErr error
	for {
		err := client.Ping(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor not ready within %s: %w", timeout, lastErr)
		}
		// Brief delay between pings — the supervisor is usually slow
		// enough to start that a short sleep avoids a busy loop without
		// noticeably extending the wait for the common case.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return fmt.Errorf("supervisor not ready within %s: %w", timeout, lastErr)
		}
	}
}

// restartSpec describes a single supervisor restart attempt. Built by
// the drift-detection caller from the live state (PID from supervisorAlive,
// ExePath from /proc/<pid>/exe when direct restart needs it, ServiceName from
// supervisorSystemdServiceName, and LaunchdLabel from supervisorLaunchdLabel).
type restartSpec struct {
	// SystemdManaged is true when the supervisor was started via
	// `systemctl --user start`. In that mode the kernel's service
	// manager owns the lifecycle; we delegate the restart to it.
	SystemdManaged bool

	// LaunchdManaged is true when the supervisor is running under macOS
	// launchd. In that mode launchd owns the lifecycle and can restart the
	// service without reading /proc/<pid>/exe, which Darwin does not expose.
	LaunchdManaged bool

	// PID is the running supervisor's process id. Used by the direct
	// branch to send SIGTERM before respawning. Ignored when
	// SystemdManaged or LaunchdManaged is true.
	PID int

	// ExePath is the resolved /proc/<pid>/exe target — the actual
	// binary on disk, not a symlink. Used by the direct branch as
	// the spawned executable. Ignored when SystemdManaged or
	// LaunchdManaged is true.
	ExePath string

	// Argv is the argument vector to pass to the new supervisor
	// (e.g. {"supervisor", "run"}). Ignored when SystemdManaged or
	// LaunchdManaged is true.
	Argv []string

	// ServiceName is the systemd unit name (e.g.
	// "gascity-supervisor.service"). Used by the systemd branch.
	// Ignored when SystemdManaged is false.
	ServiceName string

	// LaunchdLabel is the macOS launchd label. Used by the launchd branch.
	// Empty falls back to supervisorLaunchdLabel().
	LaunchdLabel string
}

// restartHelpers abstracts the side-effecting operations
// restartSupervisor performs so unit tests can exercise both branches
// without spawning real processes.
type restartHelpers struct {
	// Systemctl invokes systemctl with the given args. Production
	// uses supervisorSystemctlRun (which targets `systemctl ...`).
	Systemctl func(args ...string) error

	// Launchctl invokes launchctl with the given args. Production
	// uses supervisorLaunchctlRun.
	Launchctl func(args ...string) error

	// Kill sends SIGTERM to pid. Production uses syscall.Kill.
	Kill func(pid int) error

	// WaitExit blocks until pid has exited (or the helper escalates
	// and gives up). Production polls syscall.Kill(pid, 0) until it
	// returns ESRCH, then SIGKILLs as a fallback. Tests set this to
	// nil or a no-op when they don't model process lifetimes.
	WaitExit func(pid int) error

	// Spawn launches a detached process executing exe with argv.
	// Production starts a backgrounded child via os/exec with
	// backgroundSysProcAttr.
	Spawn func(exe string, argv ...string) error
}

// restartSupervisor restarts the gascity-supervisor process. Behavior
// depends on whether a platform service manager owns the lifecycle:
//
//   - SystemdManaged: a single `systemctl --user restart <unit>` call
//     hands the restart to the service manager. The kill+respawn is
//     systemd's responsibility; attempting to kill the PID ourselves
//     would race with systemd's own respawn.
//
//   - LaunchdManaged: a single `launchctl kickstart -k <target>` call
//     restarts the launchd service. This is the macOS production upgrade
//     path and does not require /proc/<pid>/exe.
//
//   - Direct: we kill the process by PID and spawn a new instance from
//     ExePath. Kill failures abort the restart so we never run two
//     supervisors against the same socket.
//
// The helpers allow unit tests to substitute fakes; production wires
// real systemctl/syscall.Kill/exec invocations.
func restartSupervisor(spec restartSpec, h restartHelpers) error {
	if spec.SystemdManaged && spec.LaunchdManaged {
		return fmt.Errorf("restartSupervisor: supervisor cannot be both systemd- and launchd-managed")
	}
	if spec.SystemdManaged {
		if h.Systemctl == nil {
			return fmt.Errorf("restartSupervisor: nil Systemctl helper")
		}
		if err := h.Systemctl("--user", "restart", spec.ServiceName); err != nil {
			return fmt.Errorf("systemctl --user restart %s: %w", spec.ServiceName, err)
		}
		return nil
	}
	if spec.LaunchdManaged {
		if h.Launchctl == nil {
			return fmt.Errorf("restartSupervisor: nil Launchctl helper")
		}
		target := supervisorLaunchdServiceTarget(spec.LaunchdLabel)
		if err := h.Launchctl("kickstart", "-k", target); err != nil {
			return fmt.Errorf("launchctl kickstart -k %s: %w", target, err)
		}
		return nil
	}
	if h.Kill == nil || h.Spawn == nil {
		return fmt.Errorf("restartSupervisor: nil Kill/Spawn helper")
	}
	if err := h.Kill(spec.PID); err != nil {
		return fmt.Errorf("killing supervisor pid %d: %w", spec.PID, err)
	}
	// Wait for the old supervisor to actually exit before spawning the
	// replacement. Without this gap, the old process still owns the
	// /health port and the new one fails to bind — PollReady then sees
	// the OLD supervisor still serving and returns "ready" without the
	// build_id ever flipping.
	if h.WaitExit != nil {
		if err := h.WaitExit(spec.PID); err != nil {
			return fmt.Errorf("waiting for supervisor pid %d to exit: %w", spec.PID, err)
		}
	}
	if err := h.Spawn(spec.ExePath, spec.Argv...); err != nil {
		return fmt.Errorf("spawning supervisor %s: %w", spec.ExePath, err)
	}
	return nil
}

// restartLoopGuard caps how many supervisor restarts may happen within
// a sliding window. The architecture's threshold is 3 restarts in 60s;
// a fourth within the window is refused so a misbehaving auto-restart
// never thrashes the system.
type restartLoopGuard struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	attempts []time.Time
}

func newRestartLoopGuard(maxAttempts int, window time.Duration) *restartLoopGuard {
	return &restartLoopGuard{max: maxAttempts, window: window}
}

// allowAt records a restart attempt at the given time and returns true
// if the attempt is within the configured budget. Once the budget is
// exhausted, returns false until enough attempts age out of the window.
func (g *restartLoopGuard) allowAt(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := now.Add(-g.window)
	pruned := g.attempts[:0]
	for _, t := range g.attempts {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	g.attempts = pruned
	if len(g.attempts) >= g.max {
		return false
	}
	g.attempts = append(g.attempts, now)
	return true
}
