package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/nudgepoller"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// Nudge pollers are spawned as their own process-group leaders (Setpgid in
// ensureSessionSubmitPoller/ensureNudgePoller) and released, so session
// teardown — which signals the session's process group — structurally never
// reaches them. Without an active reap they outlive their session and keep
// polling the store every cycle forever. reapLiveNudgePollers is that reap:
// unlike reapStaleNudgePollers (dead-pidfile cleanup only), it signals live,
// identity-verified pollers whose target the caller no longer wants polled.
// A reaped target self-heals: the next nudge send to that session re-spawns
// its poller via maybeStartNudgePoller.

// pollerDrainReapGrace is how long a session may sit drained before the
// reconcile tick considers its nudge poller orphaned and reaps it.
const pollerDrainReapGrace = 15 * time.Minute

// pollerReapKillWait is how long a reap waits after SIGTERM before SIGKILL.
const pollerReapKillWait = 2 * time.Second

// reapLiveNudgePollers scans the city's poller pidfiles and reaps every live
// poller whose (sessionName, agentName) target keep() rejects. Dead or
// unparseable pidfiles are removed exactly like reapStaleNudgePollers. A live
// PID is only ever signalled after its own argv has been identity-verified as
// a nudge poller for this city AND the pidfile has been confirmed to be the
// stem for that argv's target — a recycled PID can never be killed by mistake.
// Best-effort: per-file errors are reported to stderr and do not abort the
// sweep. Returns the reaped "session/agent" targets.
func reapLiveNudgePollers(cityPath string, keep func(sessionName, agentName string) bool, stderr io.Writer) []string {
	pollersDir := citylayout.RuntimePath(cityPath, "nudges", "pollers")
	entries, err := os.ReadDir(pollersDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		fmt.Fprintf(stderr, "reapLiveNudgePollers: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	var reaped []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		pidPath := filepath.Join(pollersDir, entry.Name())
		target, err := reapLiveNudgePollerPIDFile(pidPath, cityPath, keep)
		if err != nil {
			fmt.Fprintf(stderr, "reapLiveNudgePollers: %s: %v\n", entry.Name(), err) //nolint:errcheck // best-effort stderr
			continue
		}
		if target != "" {
			reaped = append(reaped, target)
		}
	}
	return reaped
}

// reapLiveNudgePollerPIDFile handles a single pidfile under the same per-file
// lock the lease path uses, so it cannot race a concurrently starting poller.
// It returns the reaped "session/agent" target, or "" when nothing was killed.
func reapLiveNudgePollerPIDFile(pidPath, cityPath string, keep func(sessionName, agentName string) bool) (string, error) {
	var reapedTarget string
	err := withNudgePollerPIDLock(pidPath, func() error {
		data, err := os.ReadFile(pidPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read nudge poller pid %q: %w", pidPath, err)
		}
		var pid int
		if n, parseErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); parseErr != nil || n != 1 || pid <= 0 {
			return removeNudgePollerPIDFile(pidPath)
		}
		argv, cmdErr := pidutil.Cmdline(pid)
		if cmdErr != nil || len(argv) == 0 {
			if !pidutil.Alive(pid) {
				return removeNudgePollerPIDFile(pidPath)
			}
			// Alive but cmdline unreadable: identity cannot be verified, so
			// the PID must not be signalled. Leave it for a later pass.
			return nil
		}
		sessionName, agentName, ok := nudgepoller.TargetFromArgv(cityPath, argv)
		if !ok {
			// Live PID that is not a poller for this city — the pidfile is a
			// leftover pointing at a recycled PID. Remove the file, never the
			// process.
			return removeNudgePollerPIDFile(pidPath)
		}
		// Ownership check: this pidfile must be the stem for the target the
		// process itself claims via argv. A mismatch means the file describes
		// some other tuple; leave both alone.
		if nudgePollerPIDPath(cityPath, sessionName, agentName) != pidPath {
			return nil
		}
		if keep(sessionName, agentName) {
			return nil
		}
		matcher := nudgepoller.CmdlineMatcher(cityPath, sessionName, agentName)
		_ = syscall.Kill(pid, syscall.SIGTERM) //nolint:errcheck // re-checked below
		deadline := time.Now().Add(pollerReapKillWait)
		for time.Now().Before(deadline) && pidutil.AliveWithCmdline(pid, matcher) {
			time.Sleep(50 * time.Millisecond)
		}
		if pidutil.AliveWithCmdline(pid, matcher) {
			_ = syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // best-effort escalation
		}
		reapedTarget = sessionName + "/" + agentName
		return removeNudgePollerPIDFile(pidPath)
	})
	return reapedTarget, err
}

// removeNudgePollerPIDFile removes ONLY the .pid file. The sibling .pid.lock
// is deliberately left in place — it is the stable per-key flock mutex inode
// (see reapStaleNudgePoller for why removing it would break mutual exclusion).
func removeNudgePollerPIDFile(pidPath string) error {
	if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove nudge poller pid %q: %w", pidPath, err)
	}
	return nil
}

// reapNudgePollersForClosedSession reaps every live poller polling for the
// given (now closed) session name. Used by the session close path so the reap
// takes effect immediately instead of waiting for the next reconcile tick.
func reapNudgePollersForClosedSession(cityPath, closedSessionName string, stderr io.Writer) []string {
	closedSessionName = strings.TrimSpace(closedSessionName)
	if cityPath == "" || closedSessionName == "" {
		return nil
	}
	return reapLiveNudgePollers(cityPath, func(sessionName, _ string) bool {
		return sessionName != closedSessionName
	}, stderr)
}
