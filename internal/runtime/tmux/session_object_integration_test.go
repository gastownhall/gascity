//go:build integration

package tmux

import (
	"errors"
	"os"
	"path/filepath"
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

// objectOf returns a session's object id and creation time.
func objectOf(t *testing.T, p *Provider, name string) (id, created string) {
	t.Helper()
	id, created, _ = strings.Cut(objectFormat(t, p, name, "#{session_id} #{session_created}"), " ")
	return id, created
}

// makeCorpse turns a running session into a remain-on-exit corpse and
// returns its session object id and creation time.
func makeCorpse(t *testing.T, p *Provider, name string) (id, created string) {
	t.Helper()
	startObjectSession(t, p, name, "sleep 300")
	if err := p.tm.SetRemainOnExit(paneTarget(name), true); err != nil {
		t.Fatalf("remain-on-exit: %v", err)
	}
	if _, err := p.tm.run("respawn-pane", "-k", "-t", paneTarget(name), "true"); err != nil {
		t.Fatalf("respawn-pane: %v", err)
	}
	waitObjectCondition(t, name+" to become a corpse", func() bool { return objectFormat(t, p, name, "#{pane_dead}") == "1" })
	return objectOf(t, p, name)
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

// A fresh read reports the corpse with the id and creation time tmux shows,
// and the corpse kill removes exactly it.
func TestKillCorpseObjectRealTmuxKillsCorpse(t *testing.T) {
	p := newObjectServer(t)
	id, created := makeCorpse(t, p, "corpse")
	got, err := p.ObserveLivenessSince("corpse", nil, time.Now())
	if err != nil || got != (runtime.Liveness{Corpse: true, ObjectID: id, ObjectCreated: created}) {
		t.Fatalf("ObserveLivenessSince = (%+v, %v), want corpse %s created %s", got, err, id, created)
	}
	if res, err := p.KillCorpseObject("corpse", id, created); err != nil || res != runtime.SessionObjectKilled {
		t.Fatalf("KillCorpseObject = %v, %v; want killed", res, err)
	}
	if hasObjectSession(t, p, "corpse") || !hasObjectSession(t, p, "keep") {
		t.Fatal("corpse kill did not remove exactly the corpse")
	}
}

func TestKillCorpseObjectRealTmuxRefusesLivePane(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "live", "sleep 300")
	id, created := objectOf(t, p, "live")
	if res, err := p.KillCorpseObject("live", id, created); err != nil || res != runtime.SessionObjectLive {
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
	id, created := objectOf(t, p, "split")
	if res, err := p.KillCorpseObject("split", id, created); err != nil || res != runtime.SessionObjectLive {
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
	id, created := makeCorpse(t, p, "corpse")
	if err := p.tm.RenameSession("corpse", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if res, err := p.KillCorpseObject("corpse", id, created); err != nil || res != runtime.SessionObjectRenamed {
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
	staleID, staleCreated := makeCorpse(t, p, "corpse")
	if staleID == "$0" {
		t.Fatalf("corpse id = %s, want one a fresh server does not reuse first", staleID)
	}
	if err := p.tm.KillServer(); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	waitObjectCondition(t, "the server to exit", p.tm.serverConfirmedDead)
	newID, _ := makeCorpse(t, p, "corpse")
	if newID == staleID {
		t.Fatalf("restarted server reused id %s", newID)
	}
	if res, err := p.KillCorpseObject("corpse", staleID, staleCreated); err != nil || res != runtime.SessionObjectGone {
		t.Fatalf("KillCorpseObject(stale %s) = %v, %v; want gone", staleID, res, err)
	}
	if !hasObjectSession(t, p, "corpse") {
		t.Fatal("a stale id killed the session that now holds the name")
	}
}

// A restarted server can hand the observed id to a new corpse with the same
// name, which passes every check but the creation time: the kill must refuse
// it as gone, and still kill it by its own creation time.
func TestKillCorpseObjectRealTmuxRefusesReusedIDAfterRestart(t *testing.T) {
	p := newObjectServer(t)
	staleID, staleCreated := makeCorpse(t, p, "corpse")
	createdAt, err := strconv.ParseInt(staleCreated, 10, 64)
	if err != nil {
		t.Fatalf("corpse #{session_created} = %q: %v", staleCreated, err)
	}
	if err := p.tm.KillServer(); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	waitObjectCondition(t, "the server to exit", p.tm.serverConfirmedDead)
	// #{session_created} has one-second resolution: start the new server after
	// the observed second, so the re-created corpse differs only there.
	waitObjectCondition(t, "the clock to pass the corpse's creation second", func() bool { return time.Now().Unix() > createdAt })
	startObjectSession(t, p, "keep", "sleep 300")
	newID, newCreated := makeCorpse(t, p, "corpse")
	if newID != staleID || newCreated == staleCreated {
		t.Fatalf("re-created corpse = %s created %s, want id %s reused with a creation time after %s", newID, newCreated, staleID, staleCreated)
	}
	if res, err := p.KillCorpseObject("corpse", staleID, staleCreated); err != nil || res != runtime.SessionObjectGone {
		t.Fatalf("KillCorpseObject(reused %s, created %s) = %v, %v; want gone", staleID, staleCreated, res, err)
	}
	if !hasObjectSession(t, p, "corpse") {
		t.Fatal("a reused id killed the corpse a restarted server re-created under the name")
	}
	if res, err := p.KillCorpseObject("corpse", newID, newCreated); err != nil || res != runtime.SessionObjectKilled {
		t.Fatalf("KillCorpseObject(%s, created %s) = %v, %v; want killed", newID, newCreated, res, err)
	}
}

func TestKillSessionObjectRealTmuxRefusesEmptyID(t *testing.T) {
	p := newObjectServer(t)
	id, created := makeCorpse(t, p, "corpse")
	for _, tc := range []struct{ id, created string }{{"", created}, {id, ""}} {
		if res, err := p.KillCorpseObject("corpse", tc.id, tc.created); res != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Fatalf("KillCorpseObject(%q, %q) = %v, %v; want ErrInvalidSessionObject", tc.id, tc.created, res, err)
		}
		if res, err := p.KillZombieObject("corpse", tc.id, tc.created, "1"); res != runtime.SessionObjectNotKilled || !errors.Is(err, runtime.ErrInvalidSessionObject) {
			t.Fatalf("KillZombieObject(%q, %q) = %v, %v; want ErrInvalidSessionObject", tc.id, tc.created, res, err)
		}
	}
	if !hasObjectSession(t, p, "corpse") {
		t.Fatal("an empty id or creation time killed something")
	}
}

// The zombie kill is conditioned on the observed pane pid: a mismatch
// refuses, the observed pid kills.
func TestKillZombieObjectRealTmuxPanePID(t *testing.T) {
	p := newObjectServer(t)
	startObjectSession(t, p, "zombie", "sleep 300")
	id, created := objectOf(t, p, "zombie")
	pid := objectFormat(t, p, "zombie", "#{pane_pid}")
	got, err := p.ObserveLivenessSince("zombie", nil, time.Now())
	if err != nil || got.ObjectID != id || got.ObjectCreated != created || got.PanePID != pid {
		t.Fatalf("ObserveLivenessSince = (%+v, %v), want id %s created %s pid %s", got, err, id, created, pid)
	}
	n, _ := strconv.Atoi(pid)
	if res, err := p.KillZombieObject("zombie", id, created, strconv.Itoa(n+1)); err != nil || res != runtime.SessionObjectChanged {
		t.Fatalf("KillZombieObject wrong pid = %v, %v; want changed", res, err)
	}
	if !hasObjectSession(t, p, "zombie") {
		t.Fatal("a pid mismatch killed the session")
	}
	if res, err := p.KillZombieObject("zombie", id, created, pid); err != nil || res != runtime.SessionObjectKilled {
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

// A live server whose whole socket directory was removed is not confirmed
// dead either: the check rejoins the bound path under the longest existing
// ancestor, resolved as tmux resolved it. TMUX_TMPDIR is a private symlink
// (on macOS it also sits under /tmp, itself a symlink), so the removal
// touches no other server's sockets.
func TestServerConfirmedDeadRealTmuxRemovedSocketDir(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	root, err := os.MkdirTemp("/tmp", "gc-cd-")
	if err != nil {
		t.Fatalf("create socket root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Mkdir(filepath.Join(root, "r"), 0o700); err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "r"), filepath.Join(root, "l")); err != nil {
		t.Fatalf("link socket dir: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(root, "l"))
	p := newObjectServer(t)
	pid, err := strconv.Atoi(objectFormat(t, p, "keep", "#{pid}"))
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	// The removed socket cannot be reached by kill-server: stop the server
	// this test started by its pid.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	if p.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = true for a live server")
	}
	if err := os.RemoveAll(filepath.Dir(p.tm.serverSocketPath())); err != nil {
		t.Fatalf("remove socket dir: %v", err)
	}
	if !p.tm.serverConfirmedDead() {
		t.Fatal("legacy serverConfirmedDead() = false after removal; the precondition of this test is gone")
	}
	if p.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = true for a live server whose socket directory was removed")
	}
}

// A server stopped by kill-server is confirmed dead, on macOS too, so
// runtime.StopForCleanup still absorbs its missing-server answer and gc
// suspend over a dead server succeeds. The wait is for the server process:
// until it exits, its listener is still bound.
func TestServerConfirmedDeadRealTmuxKilledServer(t *testing.T) {
	p := newObjectServer(t)
	pid := objectFormat(t, p, "keep", "#{pid}")
	if err := p.tm.KillServer(); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for processAlive(pid) {
		select {
		case <-timer.C:
			t.Fatalf("tmux server %s still running after kill-server", pid)
		case <-ticker.C:
		}
	}
	if !p.ServerConfirmedDead() {
		t.Fatal("ServerConfirmedDead() = false for a server stopped by kill-server")
	}
}
