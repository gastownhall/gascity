package acp

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

var _ runtime.ProcessTableScanner = (*Provider)(nil)

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner]. It scans
// the process table for agent roots carrying GC_SESSION_ID equal to id (all
// roots when id is empty) and marks a root tracked when a live in-process
// connection started a session with that GC_SESSION_ID.
//
// Only this Provider instance's connections count as tracked. After a
// supervisor restart the conn table is empty, so every surviving agent, and
// every tool child that escaped the agent's process group and reparented away
// from it, reports IsTracked=false and is eligible for orphan reaping. A root
// that merely inherited GC_SESSION_ID (a daemon started from inside the agent)
// is likewise untracked once its session has no live conn; that matches the
// tmux and subprocess providers.
//
// Tracking is owner-process-only. A second Provider on the same state
// directory (another gc process) sees the session through its control socket
// in IsRunning and ListRunning, yet reports its root untracked here. The same
// holds for a session still in its startup handshake, whose conn is only a
// reservation. Callers must reap untracked roots only for sessions they have
// independently established are not running.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	found, scanErr := proctable.ScanBySessionID(id)

	p.mu.Lock()
	trackedBySessionID := make(map[string]string, len(p.conns))
	for name, sc := range p.conns {
		// Handshake sentinels carry no cmd; they are not yet a runtime. A dead
		// conn lingers in the table until the next Start or Stop, and must not
		// keep its session's escaped tool children tracked.
		if sc == nil || sc.cmd == nil || !sc.alive() {
			continue
		}
		if sessionID := envValue(sc.cmd.Env, "GC_SESSION_ID"); sessionID != "" {
			trackedBySessionID[sessionID] = name
		}
	}
	p.mu.Unlock()

	for i := range found {
		if name, ok := trackedBySessionID[found[i].SessionID]; ok {
			found[i].IsTracked = true
			found[i].ProviderName = name
		}
	}
	return found, scanErr
}

// TerminateRuntime implements [runtime.ProcessTableScanner]. It terminates the
// runtime's process group (falling back to the pid) with SIGTERM then SIGKILL,
// bound to the process's start-time identity so a recycled pid is never
// signaled, and returns nil when the process is already gone.
func (p *Provider) TerminateRuntime(r runtime.LiveRuntime) error {
	if r.PID <= 1 {
		return fmt.Errorf("acp: invalid PID %d for session %s", r.PID, r.SessionID)
	}
	if err := proctable.KillByPID(r.PID); err != nil {
		return fmt.Errorf("acp: terminate runtime PID %d for session %s: %w", r.PID, r.SessionID, err)
	}
	return nil
}

// envValue returns the last value of key in env, matching exec.Cmd's
// last-wins handling of duplicate entries.
func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return env[i][len(prefix):]
		}
	}
	return ""
}
