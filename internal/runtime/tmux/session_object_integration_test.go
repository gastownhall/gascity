//go:build integration

package tmux

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// newObjectServer returns a Provider on a private tmux server holding a
// "keep" session (so killing the target never empties it), torn down after.
func newObjectServer(t *testing.T) *Provider {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("obj")
	p := NewProviderWithConfig(cfg)
	t.Cleanup(func() { _ = p.tm.KillServer() })
	startObjectSession(t, p, "keep", "sleep 300")
	return p
}

func startObjectSession(t *testing.T, p *Provider, name, command string) {
	t.Helper()
	if err := p.tm.NewSessionWithCommand(name, t.TempDir(), command); err != nil {
		t.Fatalf("new session %s: %v", name, err)
	}
}

// objectFormat reads one format for a session through its exact pane target.
func objectFormat(t *testing.T, p *Provider, name, format string) string {
	t.Helper()
	out, err := p.tm.run("display-message", "-p", "-t", paneTarget(name), format)
	if err != nil {
		t.Fatalf("display-message %s %s: %v", name, format, err)
	}
	return strings.TrimSpace(out)
}

// makeCorpse turns a running session into a remain-on-exit corpse and
// returns its session object id.
func makeCorpse(t *testing.T, p *Provider, name string) string {
	t.Helper()
	startObjectSession(t, p, name, "sleep 300")
	if err := p.tm.SetRemainOnExit(paneTarget(name), true); err != nil {
		t.Fatalf("remain-on-exit: %v", err)
	}
	if _, err := p.tm.run("respawn-pane", "-k", "-t", paneTarget(name), "true"); err != nil {
		t.Fatalf("respawn-pane: %v", err)
	}
	waitObjectCondition(t, name+" to become a corpse", func() bool { return objectFormat(t, p, name, "#{pane_dead}") == "1" })
	return objectFormat(t, p, name, "#{session_id}")
}

// waitObjectCondition polls ready until it holds, failing after 5s.
func waitObjectCondition(t *testing.T, what string, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-ticker.C:
		}
	}
}

func hasObjectSession(t *testing.T, p *Provider, name string) bool {
	t.Helper()
	has, err := p.tm.HasSession(name)
	if err != nil {
		t.Fatalf("HasSession %s: %v", name, err)
	}
	return has
}

// A fresh read reports the corpse with the id tmux shows, and the corpse kill
// removes exactly it.
func TestKillCorpseObjectRealTmuxKillsCorpse(t *testing.T) {
	p := newObjectServer(t)
	id := makeCorpse(t, p, "corpse")
	got, err := p.ObserveLivenessSince("corpse", nil, time.Now())
	if err != nil || got != (runtime.Liveness{Corpse: true, ObjectID: id}) {
		t.Fatalf("ObserveLivenessSince = (%+v, %v), want corpse %s", got, err, id)
	}
	if res, err := p.KillCorpseObject("corpse", id); err != nil || res != runtime.SessionObjectKilled {
		t.Fatalf("KillCorpseObject = %v, %v; want killed", res, err)
	}
	if hasObjectSession(t, p, "corpse") || !hasObjectSession(t, p, "keep") {
		t.Fatal("corpse kill did not remove exactly the corpse")
	}
}

func TestKillCorpseObjectRealTmuxRefusesLivePane(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "live", "sleep 300")
	id := objectFormat(t, p, "live", "#{session_id}")
	if res, err := p.KillCorpseObject("live", id); err != nil || res != runtime.SessionObjectLive {
		t.Fatalf("KillCorpseObject(live) = %v, %v; want refused live", res, err)
	}
	if !hasObjectSession(t, p, "live") {
		t.Fatal("a live session was killed")
	}
}

// #{pane_dead} reads only the active pane: a dead active pane beside a live
// one must refuse, so the live pane is never killed with the session.
func TestKillCorpseObjectRealTmuxRefusesDeadActivePaneBesideLivePane(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "split", "sleep 300")
	if _, err := p.tm.run("set-option", "-w", "-t", paneTarget("split"), "remain-on-exit", "on"); err != nil {
		t.Fatalf("remain-on-exit: %v", err)
	}
	if _, err := p.tm.run("split-window", "-t", paneTarget("split"), "true"); err != nil {
		t.Fatalf("split-window: %v", err)
	}
	waitObjectCondition(t, "the active pane to die", func() bool { return objectFormat(t, p, "split", "#{pane_dead} #{window_panes}") == "1 2" })
	id := objectFormat(t, p, "split", "#{session_id}")
	if res, err := p.KillCorpseObject("split", id); err != nil || res != runtime.SessionObjectLive {
		t.Fatalf("KillCorpseObject(dead active pane beside a live one) = %v, %v; want refused live", res, err)
	}
	if !hasObjectSession(t, p, "split") {
		t.Fatal("a session with a live pane was killed")
	}
}

// The name now held by another object (here: the observed object renamed)
// refuses, so the kill never follows a name.
func TestKillCorpseObjectRealTmuxRefusesNameMismatch(t *testing.T) {
	p := newObjectServer(t)
	id := makeCorpse(t, p, "corpse")
	if err := p.tm.RenameSession("corpse", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if res, err := p.KillCorpseObject("corpse", id); err != nil || res != runtime.SessionObjectRenamed {
		t.Fatalf("KillCorpseObject after rename = %v, %v; want renamed", res, err)
	}
	if !hasObjectSession(t, p, "renamed") {
		t.Fatal("a renamed session was killed")
	}
}

// Session ids restart at $0 with the server, so an id read before a restart
// must refuse rather than hit whatever holds it now.
func TestKillCorpseObjectRealTmuxRefusesStaleIDAfterRestart(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "filler", "sleep 300")
	staleID := makeCorpse(t, p, "corpse")
	if staleID == "$0" {
		t.Fatalf("corpse id = %s, want one a fresh server does not reuse first", staleID)
	}
	if err := p.tm.KillServer(); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	waitObjectCondition(t, "the server to exit", p.tm.serverConfirmedDead)
	newID := makeCorpse(t, p, "corpse")
	if newID == staleID {
		t.Fatalf("restarted server reused id %s", newID)
	}
	if res, err := p.KillCorpseObject("corpse", staleID); err != nil || res != runtime.SessionObjectGone {
		t.Fatalf("KillCorpseObject(stale %s) = %v, %v; want gone", staleID, res, err)
	}
	if !hasObjectSession(t, p, "corpse") {
		t.Fatal("a stale id killed the session that now holds the name")
	}
}

func TestKillSessionObjectRealTmuxRefusesEmptyID(t *testing.T) {
	p := newObjectServer(t)
	makeCorpse(t, p, "corpse")
	if res, err := p.KillCorpseObject("corpse", ""); res != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
		t.Fatalf("KillCorpseObject empty id = %v, %v; want ErrInvalidSessionObject", res, err)
	}
	if res, err := p.KillZombieObject("corpse", "", "1"); res != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
		t.Fatalf("KillZombieObject empty id = %v, %v; want ErrInvalidSessionObject", res, err)
	}
	if !hasObjectSession(t, p, "corpse") {
		t.Fatal("an empty id killed something")
	}
}

// The zombie kill is conditioned on the observed pane pid: a mismatch
// refuses, the observed pid kills.
func TestKillZombieObjectRealTmuxPanePID(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "zombie", "sleep 300")
	id := objectFormat(t, p, "zombie", "#{session_id}")
	pid := objectFormat(t, p, "zombie", "#{pane_pid}")
	got, err := p.ObserveLivenessSince("zombie", nil, time.Now())
	if err != nil || got.ObjectID != id || got.PanePID != pid {
		t.Fatalf("ObserveLivenessSince = (%+v, %v), want id %s pid %s", got, err, id, pid)
	}
	n, _ := strconv.Atoi(pid)
	if res, err := p.KillZombieObject("zombie", id, strconv.Itoa(n+1)); err != nil || res != runtime.SessionObjectChanged {
		t.Fatalf("KillZombieObject wrong pid = %v, %v; want changed", res, err)
	}
	if !hasObjectSession(t, p, "zombie") {
		t.Fatal("a pid mismatch killed the session")
	}
	if res, err := p.KillZombieObject("zombie", id, pid); err != nil || res != runtime.SessionObjectKilled {
		t.Fatalf("KillZombieObject observed pid = %v, %v; want killed", res, err)
	}
}

// A live server whose socket file was unlinked reads dead to legacy's socket
// check, but not to ServerConfirmedDead, which finds its listener.
func TestServerConfirmedDeadRealTmuxUnlinkedSocket(t *testing.T) {
	p := newObjectServer(t)
	pid, err := strconv.Atoi(objectFormat(t, p, "keep", "#{pid}"))
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	// The unlinked socket cannot be reached by kill-server: stop the server
	// this test started by its pid.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	if p.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = true for a live server")
	}
	if err := os.Remove(p.tm.serverSocketPath()); err != nil {
		t.Fatalf("unlink socket: %v", err)
	}
	if !p.tm.serverConfirmedDead() {
		t.Fatal("legacy serverConfirmedDead() = false after unlink; the precondition of this test is gone")
	}
	if p.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = true for a live server whose socket was unlinked")
	}
}
