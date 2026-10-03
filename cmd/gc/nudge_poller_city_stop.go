package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A city's nudge pollers must not outlive the city (#6857). A poller is a
// detached `gc nudge poll` sidecar, and each observation it makes reads the
// city bead store. With the proxied bd transport any read restarts the bd
// proxy and its Dolt child, so a poller left running after `gc stop` brings
// back the store the stop just retired. Two guards close that:
//
//   - stopCityNudgePollers terminates the city's live pollers. The city stop
//     paths run it, through shutdownBeadsProvider, before they retire the
//     store.
//   - nudgePollCityStopped lets a poller that still survives notice that its
//     city is down and exit before its next store read.
//
// Neither touches the nudge queue: queued nudges stay queued and deliver once
// a poller runs again after the next start.

const (
	// nudgePollerStopGrace bounds how long stopCityNudgePoller waits for a
	// poller to exit after SIGTERM before escalating to SIGKILL, and again
	// after SIGKILL before reporting failure.
	nudgePollerStopGrace = 5 * time.Second
	// nudgePollerStopPollInterval is the liveness re-check cadence while
	// waiting for a signaled poller to exit.
	nudgePollerStopPollInterval = 25 * time.Millisecond
)

// nudgePollControllerAlive is the poll loop's controller probe. It is a test
// seam; tests that replace it must stay serial.
var nudgePollControllerAlive = controllerAlive

// nudgePollCityStopped reports whether the poller's city is down: no
// controller, standalone or supervisor-hosted, answers on the city's
// controller socket, and the target session's runtime is not running. It
// reads neither the bead store nor the nudge queue.
//
// Both conditions are required. A running session with no controller is a
// supervisor restart that preserves sessions, or a crashed controller; its
// poller keeps delivering. A missing session with a live controller may be a
// wake in progress, which the missing-session grace in the poll loop covers.
func nudgePollCityStopped(target nudgeTarget, sp runtime.Provider) bool {
	if nudgePollControllerAlive(target.cityPath) != 0 {
		return false
	}
	return !sp.IsRunning(target.sessionName)
}

// stopCityNudgePollers terminates every live nudge poller of cityPath, found
// through the PID files under the city's pollers directory.
//
// Only a PID whose live command line is this city's poller for exactly the
// session/target tuple its PID file is named after is signaled, so a reused
// PID or another city's poller is never touched. A missing pollers directory
// is a no-op. Per-poller failures are joined and returned without aborting
// the sweep.
func stopCityNudgePollers(cityPath string) error {
	pollersDir := citylayout.RuntimePath(cityPath, "nudges", "pollers")
	entries, err := os.ReadDir(pollersDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading nudge pollers dir: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		if err := stopCityNudgePoller(cityPath, filepath.Join(pollersDir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopCityNudgePoller terminates the poller pidPath names, if it is live and
// is cityPath's poller for that file. It holds the per-file lock the spawn
// and lease paths take, so no replacement poller starts for the same key
// while this one is being stopped. Once the poller is gone its PID file is
// removed; the sibling .pid.lock stays, as in reapStaleNudgePoller.
func stopCityNudgePoller(cityPath, pidPath string) error {
	return withNudgePollerPIDLock(pidPath, func() error {
		data, err := os.ReadFile(pidPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read nudge poller pid %q: %w", pidPath, err)
		}
		var pid int
		if n, parseErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); parseErr != nil || n != 1 || pid <= 0 {
			// No process to stop; reapStaleNudgePollers owns stale-file cleanup.
			return nil
		}
		isPoller := nudgepoller.FileStemMatcher(cityPath, strings.TrimSuffix(filepath.Base(pidPath), ".pid"))
		if !pidutil.AliveWithCmdline(pid, isPoller) {
			return nil
		}
		if err := signalNudgePollerAndWait(pid, isPoller); err != nil {
			return fmt.Errorf("stopping nudge poller pid %d (%s): %w", pid, pidPath, err)
		}
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stopped nudge poller pid %q: %w", pidPath, err)
		}
		return nil
	})
}

// signalNudgePollerAndWait sends SIGTERM, escalating to SIGKILL after
// nudgePollerStopGrace. Identity is re-checked right before the SIGKILL: if
// the poller exited and its PID was reused during the grace period, the
// forced kill would land on an unrelated process.
func signalNudgePollerAndWait(pid int, isPoller func([]string) bool) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("SIGTERM: %w", err)
	}
	if waitForNudgePollerExit(pid) {
		return nil
	}
	if !pidutil.AliveWithCmdline(pid, isPoller) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("SIGKILL: %w", err)
	}
	if waitForNudgePollerExit(pid) {
		return nil
	}
	return fmt.Errorf("still running %s after SIGKILL", nudgePollerStopGrace)
}

// waitForNudgePollerExit reports whether pid exits within
// nudgePollerStopGrace. pidutil.Alive treats a zombie as exited, so a
// signaled poller that is still an unreaped child of this process counts as
// gone.
func waitForNudgePollerExit(pid int) bool {
	deadline := time.Now().Add(nudgePollerStopGrace)
	for {
		if !pidutil.Alive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(nudgePollerStopPollInterval)
	}
}
