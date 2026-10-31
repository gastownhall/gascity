package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A managed Dolt that is alive and still accepting connections but failed the
// recover query probe is usually busy, not dead: a probe that timed out under
// load says nothing about whether the server would answer a moment later.
// Recovery therefore waits until the server has been continuously
// unresponsive for [daemon].dolt_unresponsive_restart_after before it stops a
// process it can still see listening. The first-unresponsive time lives in a
// file under the pack state dir because every recover is a fresh process.

// managedDoltUnresponsiveSincePath is the file that records when a live
// managed Dolt was first seen unresponsive, so the window spans processes.
func managedDoltUnresponsiveSincePath(layout managedDoltRuntimeLayout) string {
	return filepath.Join(layout.PackStateDir, "recover-unresponsive-since")
}

// managedDoltRecoverLastPath is the file that records when the last
// provider-level recover started, so its cooldown spans processes.
func managedDoltRecoverLastPath(layout managedDoltRuntimeLayout) string {
	return filepath.Join(layout.PackStateDir, "recover-last")
}

// readManagedDoltTimestamp returns the time stored at path. A missing,
// unreadable, or malformed file reads as "no time recorded".
func readManagedDoltTimestamp(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	when, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return when, true
}

func writeManagedDoltTimestamp(path string, when time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(when.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

func removeManagedDoltTimestamp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// recentManagedDoltRecover reports whether a recover started within cooldown
// before now, according to the persisted record.
func recentManagedDoltRecover(cityPath string, now time.Time, cooldown time.Duration) bool {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return false
	}
	last, ok := readManagedDoltTimestamp(managedDoltRecoverLastPath(layout))
	return ok && now.Sub(last) < cooldown
}

// recordManagedDoltRecover persists the start of a recover for
// recentManagedDoltRecover.
func recordManagedDoltRecover(cityPath string, now time.Time) error {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return err
	}
	return writeManagedDoltTimestamp(managedDoltRecoverLastPath(layout), now)
}

// resolveManagedDoltUnresponsiveRestartAfter returns how long a live managed
// Dolt may stay unresponsive before recovery stops it. It reads
// `[daemon].dolt_unresponsive_restart_after` from city.toml and falls back to
// the default when the config cannot be loaded. An empty cityPath returns the
// default without loading a config, as resolveManagedDoltStopTimeout does.
func resolveManagedDoltUnresponsiveRestartAfter(cityPath string) time.Duration {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltUnresponsiveRestartAfter
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return config.DefaultDoltUnresponsiveRestartAfter
	}
	return cfg.Daemon.DoltUnresponsiveRestartAfterDuration()
}

// classifyManagedDoltUnresponsive reports the managed Dolt PID when the
// process is alive, owned by this city, and its port still accepts TCP
// connections. Anything else (no process, a foreign owner, a refused
// connection) is not the "alive but slow" case.
func classifyManagedDoltUnresponsive(cityPath, host, port string) (int, bool) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return 0, false
	}
	pid, _ := findManagedDoltPIDWithOptions(layout, port, false)
	if pid <= 0 || !managedDoltProcessControllable(pid, layout) {
		return 0, false
	}
	if managedDoltTCPProbe(host, port) != nil {
		return 0, false
	}
	return pid, true
}

// managedDoltRecoverCaller describes who asked for the recover: the beads
// order that ran it, else the executable and its parent.
func managedDoltRecoverCaller() string {
	if actor := strings.TrimSpace(os.Getenv("BEADS_ACTOR")); strings.HasPrefix(actor, "order:") {
		return actor
	}
	return fmt.Sprintf("%s ppid=%d", os.Args[0], os.Getppid())
}

// logManagedDoltRecoverDecision appends one line to dolt.log saying what
// recover decided about the managed Dolt and why. A log that cannot be
// written falls back to stderr so the decision is never silent.
func logManagedDoltRecoverDecision(layout managedDoltRuntimeLayout, decision, reason string, pid int, port string, probeErr error, unresponsiveFor time.Duration, now time.Time) {
	line := fmt.Sprintf("%s gc recover-managed: decision=%s reason=%s pid=%d port=%s probe_error=%q unresponsive_for=%s caller=%s\n",
		now.UTC().Format(time.RFC3339), decision, reason, pid, strings.TrimSpace(port), fmt.Sprint(probeErr), unresponsiveFor.Round(time.Second), managedDoltRecoverCaller())
	f, err := os.OpenFile(layout.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		_, err = f.WriteString(line)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc recover-managed: write %s: %v\n%s", layout.LogFile, err, line) //nolint:errcheck
	}
}

// managedDoltRecoverUnresponsiveGate decides whether a failed query probe may
// proceed to stopping the managed Dolt. It returns the reason to stop and how
// long the server has been unresponsive, or an error when the stop must be deferred because the server is alive, listening,
// and has not been unresponsive for the whole window.
func managedDoltRecoverUnresponsiveGate(cityPath, host, port string, layout managedDoltRuntimeLayout, ops managedDoltRecoveryOps, probeErr error, report *managedDoltRecoverReport) (string, time.Duration, error) {
	if ops.unresponsiveRestartAfter <= 0 || ops.classifyUnresponsive == nil {
		return "probe-failed", 0, nil
	}
	now := recoverNow(ops)
	sincePath := managedDoltUnresponsiveSincePath(layout)
	pid, live := ops.classifyUnresponsive(cityPath, host, port)
	if !live {
		if err := removeManagedDoltTimestamp(sincePath); err != nil {
			fmt.Fprintf(os.Stderr, "gc recover-managed: clear %s: %v\n", sincePath, err) //nolint:errcheck
		}
		return "probe-failed-not-live", 0, nil
	}
	since, recorded := readManagedDoltTimestamp(sincePath)
	if !recorded || since.After(now) {
		since = now
		if err := writeManagedDoltTimestamp(sincePath, since); err != nil {
			// Without a persisted start the window could never elapse and a
			// wedged server would never be restarted: fall back to stopping.
			fmt.Fprintf(os.Stderr, "gc recover-managed: record %s: %v\n", sincePath, err) //nolint:errcheck
			return "probe-failed-window-unrecorded", 0, nil
		}
	}
	elapsed := now.Sub(since)
	if elapsed >= ops.unresponsiveRestartAfter {
		if err := removeManagedDoltTimestamp(sincePath); err != nil {
			fmt.Fprintf(os.Stderr, "gc recover-managed: clear %s: %v\n", sincePath, err) //nolint:errcheck
		}
		return "unresponsive-window-elapsed", elapsed, nil
	}
	report.HadPID = true
	report.PID = pid
	logManagedDoltRecoverDecision(layout, "defer", "unresponsive-within-window", pid, port, probeErr, elapsed, now)
	return "", elapsed, fmt.Errorf("managed dolt (pid %d) on port %s is listening but unresponsive (%w); not restarting until it has been unresponsive for %s (so far %s)",
		pid, strings.TrimSpace(port), probeErr, ops.unresponsiveRestartAfter, elapsed.Round(time.Second))
}
