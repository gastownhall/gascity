package acp

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

var _ runtime.ProcessTableScanner = (*Provider)(nil)

// controlSocketEnv names the environment variable Start sets on every agent to
// the absolute path of the session's control socket. The agent's tool children
// and daemons inherit it.
const controlSocketEnv = "GC_ACP_CONTROL_SOCKET"

// ownerDialTimeout bounds the connect used to test a foreign owner's control
// socket. The owner's kernel completes the connect from its listen backlog, so
// a live owner answers well inside it even while busy.
const ownerDialTimeout = 500 * time.Millisecond

// controlSocketMarker returns the value of [controlSocketEnv] for name: the
// absolute control-socket path, so a gc process with a different state
// directory still resolves it.
func (p *Provider) controlSocketMarker(name string) string {
	sp := p.sockPath(name)
	if abs, err := filepath.Abs(sp); err == nil {
		return abs
	}
	return sp
}

// FindRuntimesBySessionID implements [runtime.ProcessTableScanner]. It scans
// the process table for agent roots carrying GC_SESSION_ID equal to id (all
// roots when id is empty) and reports a root tracked when some live gc process
// owns its session:
//
//   - this Provider holds a live connection for a session with that
//     GC_SESSION_ID, or
//   - the root's [controlSocketEnv] marker names a control socket this Provider
//     created and that session is still in its startup handshake, or
//   - the marker names a control socket this Provider did not create and that
//     socket accepts a connection.
//
// The marker makes ownership positive rather than inferred. A gc process that
// does not own a session (a CLI command next to the controller, possibly with a
// different state directory) must not read the controller's live agent as an
// orphan just because its own connection table is empty or its IsRunning check
// could not reach the socket. The owner's listener closes when the agent
// process exits, and dies with the owner, so after an agent crash or an owner
// (supervisor) restart the socket refuses and the agent and every escaped
// tool child report IsTracked=false and are eligible for orphan reaping. A
// socket this Provider created is judged by its own connection state instead,
// so a crashed agent whose dead connection is still in the table is untracked
// immediately. A root that merely inherited GC_SESSION_ID (a daemon started from
// inside the agent) is tracked exactly as long as its session is.
//
// Roots without a marker (not started by this provider, or started before the
// marker existed) are tracked only by this Provider's live connections.
//
// Residual windows, all resolving toward keeping a process alive: a foreign
// owner's agent exists for a moment between process start and socket bind, and
// a replacement session bound to the same socket path makes the previous
// incarnation's escaped children read as tracked. A foreign agent in that first
// window reads untracked; callers still reap only sessions they have
// independently established are not running.
func (p *Provider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	found, scanErr := proctable.ScanBySessionID(id)
	if len(found) == 0 {
		return found, scanErr
	}

	p.mu.Lock()
	trackedBySessionID := make(map[string]string, len(p.conns))
	// ownedSockets maps each control socket this Provider created (for any conn
	// in the table, live, dead or handshaking) to its session name, and
	// whether that session is still handshaking.
	type ownedSocket struct {
		name        string
		handshaking bool
	}
	ownedSockets := make(map[string]ownedSocket, len(p.conns))
	for name, sc := range p.conns {
		if sc == nil {
			continue
		}
		// Handshake sentinels carry no cmd; they are not yet a runtime.
		handshaking := sc.cmd == nil
		ownedSockets[p.controlSocketMarker(name)] = ownedSocket{name: name, handshaking: handshaking}
		// A dead conn lingers in the table until the next Start or Stop, and
		// must not keep its session's escaped tool children tracked.
		if handshaking || !sc.alive() {
			continue
		}
		if sessionID := envValue(sc.cmd.Env, "GC_SESSION_ID"); sessionID != "" {
			trackedBySessionID[sessionID] = name
		}
	}
	p.mu.Unlock()

	var errs []error
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	foreignAlive := make(map[string]bool)
	for i := range found {
		if name, ok := trackedBySessionID[found[i].SessionID]; ok {
			found[i].IsTracked = true
			found[i].ProviderName = name
			continue
		}
		marker, err := proctable.ProcessEnvValue(found[i].PID, controlSocketEnv)
		if err != nil {
			// Unattributable: keep the root rather than risk reaping a live
			// agent, and surface why.
			found[i].IsTracked = true
			errs = append(errs, fmt.Errorf("acp: reading %s for PID %d: %w", controlSocketEnv, found[i].PID, err))
			continue
		}
		if marker == "" {
			continue
		}
		if owned, ok := ownedSockets[marker]; ok {
			if owned.handshaking {
				found[i].IsTracked = true
				found[i].ProviderName = owned.name
			}
			continue
		}
		alive, seen := foreignAlive[marker]
		if !seen {
			alive = controlSocketAccepts(marker)
			foreignAlive[marker] = alive
		}
		found[i].IsTracked = alive
	}
	return found, errors.Join(errs...)
}

// controlSocketAccepts reports whether a unix socket at path accepts a
// connection. It sends nothing: an accepted connect proves a live listener,
// which only the owning gc process holds, and only while its agent runs.
func controlSocketAccepts(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	conn, err := net.DialTimeout("unix", path, ownerDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
