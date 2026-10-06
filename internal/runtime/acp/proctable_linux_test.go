//go:build linux

package acp

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// snapshotProcRoot copies the scanner-visible /proc files (environ, stat,
// comm) of the given pids into a fresh fake procfs root. The proctable scanner
// refuses the live /proc under go test (gastownhall/gascity#2839) so a test run
// on a host with a live fleet cannot reap real agents; a snapshot restricted to
// the test's own processes keeps that guarantee while still exercising the
// real root-detection rules against real process state. Pids that are already
// gone are omitted, so a rescan after a kill observes the kill.
func snapshotProcRoot(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, pid := range pids {
		src := filepath.Join("/proc", strconv.Itoa(pid))
		files := make(map[string][]byte, 3)
		complete := true
		for _, name := range []string{"environ", "stat", "comm"} {
			data, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				complete = false
				break
			}
			files[name] = data
		}
		if !complete {
			continue
		}
		dst := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dst, err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
				t.Fatalf("write %s/%s: %v", dst, name, err)
			}
		}
	}
	return root
}

// scanSnapshot points the proctable scanner at a snapshot of pids for the
// duration of fn.
func scanSnapshot(t *testing.T, fn func(), pids ...int) {
	t.Helper()
	restore := proctable.SetScanRootForTesting(snapshotProcRoot(t, pids...))
	defer restore()
	fn()
}

// orphaningACPCommand wraps the fake ACP agent so that, before it execs, it
// starts a tool child in its own session (setsid) — the shape of an agent tool
// subprocess that escapes the agent's process group and outlives it. The tool
// child's pid is written to pidFile. Its stdio is detached so it cannot hold
// the agent's pipes open. Its duration differs from the proctable package's
// setsid repro, which finds its own child with pgrep -f 'sleep 300' and would
// pick up this one when the packages' tests run concurrently.
func orphaningACPCommand(pidFile string) string {
	return fmt.Sprintf("setsid sleep 600 </dev/null >/dev/null 2>&1 & echo $! > %q; %s", pidFile, fakeACPShellCommand())
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("pid file %q holds %q: %v", path, data, err)
	}
	return pid
}

// startOrphaningSession starts a fake ACP agent carrying sessionID on p and
// returns the agent root pid and its setsid'd tool child's pid.
func startOrphaningSession(t *testing.T, p *Provider, name, sessionID, city string) (agentPID, toolPID int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "tool.pid")
	err := p.Start(context.Background(), name, runtime.Config{
		Command: orphaningACPCommand(pidFile),
		WorkDir: t.TempDir(),
		Env: map[string]string{
			"GC_SESSION_ID": sessionID,
			"GC_CITY_PATH":  city,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if sc == nil || sc.cmd == nil || sc.cmd.Process == nil {
		t.Fatalf("no live conn tracked for %q", name)
	}
	agentPID = sc.cmd.Process.Pid
	// The pid file is written before the shell execs the agent, and Start
	// returns only after the agent answered the handshake, so it exists now.
	toolPID = readPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(toolPID, syscall.SIGKILL) })
	return agentPID, toolPID
}

func findOnly(t *testing.T, s runtime.ProcessTableScanner, sessionID string) []runtime.LiveRuntime {
	t.Helper()
	found, err := s.FindRuntimesBySessionID(sessionID)
	if err != nil {
		t.Fatalf("FindRuntimesBySessionID(%q): %v", sessionID, err)
	}
	return found
}

func TestFindRuntimesBySessionIDMarksLiveSessionTracked(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	agentPID, toolPID := startOrphaningSession(t, p, name, sessionID, city)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, sessionID) }, agentPID, toolPID)

	// The tool child's parent is the live agent carrying the same session id,
	// so only the agent is a root.
	if len(found) != 1 {
		t.Fatalf("found = %+v, want exactly the agent root", found)
	}
	got := found[0]
	if got.PID != agentPID || !got.IsTracked || got.ProviderName != name || got.City != city {
		t.Fatalf("found[0] = %+v, want tracked pid %d provider name %q city %q", got, agentPID, name, city)
	}
}

func TestFindRuntimesBySessionIDIgnoresOtherSessions(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	agentPID, toolPID := startOrphaningSession(t, p, name, "sid-"+name, t.TempDir())

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, "sid-someone-else") }, agentPID, toolPID)
	if len(found) != 0 {
		t.Fatalf("found = %+v, want none for a different session id", found)
	}
}

// A supervisor death loses the in-process conn table and the control-socket
// listener: a fresh Provider on the same state directory must report the
// surviving agent and, once the agent is terminated, its escaped tool child as
// untracked roots, and TerminateRuntime must kill both.
func TestProviderRestartReportsAndReapsOrphanedRuntimes(t *testing.T) {
	p1 := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	agentPID, toolPID := startOrphaningSession(t, p1, name, sessionID, city)
	simulateOwnerDeath(t, p1, name)

	p2 := NewProviderWithDir(p1.dir, p1.cfg)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != agentPID || found[0].IsTracked || found[0].ProviderName != "" {
		t.Fatalf("after restart found = %+v, want untracked agent root pid %d", found, agentPID)
	}
	if err := p2.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime(agent): %v", err)
	}

	// The agent is gone (or a zombie awaiting p1's reaper); its setsid'd tool
	// child has been reparented away from it and is now its own root.
	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != toolPID || found[0].IsTracked {
		t.Fatalf("after agent kill found = %+v, want untracked tool root pid %d", found, toolPID)
	}
	if err := p2.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime(tool): %v", err)
	}

	scanSnapshot(t, func() { found = findOnly(t, p2, sessionID) }, agentPID, toolPID)
	if len(found) != 0 {
		t.Fatalf("after reaping found = %+v, want no roots", found)
	}
}

// The seam-backed provider is what production wraps; it must expose the
// scanner or callers type-asserting it see no capability.
func TestSeamBackedProviderForwardsProcessTableScanner(t *testing.T) {
	raw := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	agentPID, toolPID := startOrphaningSession(t, raw, name, sessionID, t.TempDir())

	var sp runtime.Provider = seamBack(raw)
	scanner, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		t.Fatal("seam-backed ACP provider does not implement runtime.ProcessTableScanner")
	}
	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, scanner, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || !found[0].IsTracked || found[0].ProviderName != name {
		t.Fatalf("found = %+v, want the tracked agent root", found)
	}
}

func TestTerminateRuntimeRefusesInitAndInvalidPIDs(t *testing.T) {
	p := newTestProvider(t)
	for _, pid := range []int{-1, 0, 1} {
		if err := p.TerminateRuntime(runtime.LiveRuntime{PID: pid, SessionID: "sid"}); err == nil {
			t.Errorf("TerminateRuntime(PID %d) = nil, want refusal", pid)
		}
	}
}

// The common in-place crash: the agent dies, its dead conn stays in the table
// until the next Start, and the pre-start orphan sweep scans through the same
// Provider. The escaped tool child must be untracked so it is reaped.
func TestFindRuntimesBySessionIDIgnoresDeadConn(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	agentPID, toolPID := startOrphaningSession(t, p, name, sessionID, t.TempDir())

	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if err := syscall.Kill(agentPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill agent: %v", err)
	}
	select {
	case <-sc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent conn did not observe the process exit")
	}
	p.mu.Lock()
	stillThere := p.conns[name] == sc
	p.mu.Unlock()
	if !stillThere {
		t.Fatal("dead conn left the table; the test no longer models the crash window")
	}

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != toolPID || found[0].IsTracked || found[0].ProviderName != "" {
		t.Fatalf("found = %+v, want untracked tool root pid %d", found, toolPID)
	}
}

// During the startup handshake the conn table holds a cmd-less reservation.
// A concurrent scan (a controller tick) must not panic on it, and must count
// the half-started agent as tracked: its Start is in flight in this process,
// so reaping it would kill a session being started.
func TestFindRuntimesBySessionIDDuringHandshake(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	// The agent reports its pid through a fifo, so the test blocks on the
	// write instead of polling for a file.
	pidFIFO := filepath.Join(t.TempDir(), "agent.pid")
	if err := syscall.Mkfifo(pidFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	startErr := make(chan error, 1)
	go func() {
		// The agent never answers initialize, holding Start in the handshake
		// until Stop cancels it.
		startErr <- p.Start(context.Background(), name, runtime.Config{
			// The trailing no-op keeps the shell from exec'ing sleep, so the
			// reported pid keeps a stable identity (an exec in flight can hide
			// the process from a scan).
			Command: fmt.Sprintf("echo $$ > %q; sleep 600; :", pidFIFO),
			WorkDir: t.TempDir(),
			Env:     map[string]string{"GC_SESSION_ID": sessionID},
		})
	}()

	pidData := make(chan []byte, 1)
	go func() {
		data, _ := os.ReadFile(pidFIFO)
		pidData <- data
	}()
	var agentPID int
	select {
	case data := <-pidData:
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			t.Fatalf("agent pid %q: %v", data, err)
		}
		agentPID = pid
	case err := <-startErr:
		t.Fatalf("Start returned before the agent reported its pid: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("agent never reported its pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(-agentPID, syscall.SIGKILL) })

	// The agent's parent is this test process, which carries no GC_SESSION_ID,
	// so the agent shell is a root; its sleep child is not.
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if sc == nil || sc.cmd != nil {
		t.Fatalf("conn = %+v, want the handshake reservation", sc)
	}

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, sessionID) }, agentPID)
	if len(found) != 1 || found[0].PID != agentPID || !found[0].IsTracked || found[0].ProviderName != name {
		t.Fatalf("found = %+v, want handshaking agent root pid %d tracked as %q", found, agentPID, name)
	}

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-startErr:
		if err == nil {
			t.Fatal("Start = nil after Stop during handshake, want an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after Stop canceled the handshake")
	}
}

// simulateOwnerDeath makes name's control socket look the way a dead owner
// leaves it — the socket file present but nothing listening — without killing
// the agent, which in production survives its supervisor.
func simulateOwnerDeath(t *testing.T, p *Provider, name string) {
	t.Helper()
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	lis, ok := sc.listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("control listener is %T, want *net.UnixListener", sc.listener)
	}
	lis.SetUnlinkOnClose(false)
	if err := lis.Close(); err != nil {
		t.Fatalf("close control listener: %v", err)
	}
	if _, err := os.Stat(p.sockPath(name)); err != nil {
		t.Fatalf("stale control socket file missing: %v", err)
	}
}

// A gc process that does not own a session (a CLI command next to the
// controller) builds its own Provider with an empty connection table, possibly
// on a different state directory. It must see the owner's live agent as
// tracked through the control-socket marker, or its pre-start orphan kill
// would terminate the controller's live agent whenever its own IsRunning check
// misses the socket.
func TestFindRuntimesBySessionIDTracksForeignOwnedSession(t *testing.T) {
	owner := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	agentPID, toolPID := startOrphaningSession(t, owner, name, sessionID, t.TempDir())

	nonOwner := newTestProvider(t)
	if nonOwner.dir == owner.dir {
		t.Fatal("non-owner shares the owner's state directory; the test needs them apart")
	}
	if nonOwner.IsRunning(name) {
		t.Fatal("non-owner reaches the owner's socket by name; the test no longer models the miss")
	}

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, nonOwner, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != agentPID || !found[0].IsTracked {
		t.Fatalf("non-owner found = %+v, want the owner's live agent root pid %d tracked", found, agentPID)
	}

	// Once the owner is gone its listener is too, and the same agent is an
	// orphan to everyone.
	simulateOwnerDeath(t, owner, name)
	scanSnapshot(t, func() { found = findOnly(t, nonOwner, sessionID) }, agentPID, toolPID)
	if len(found) != 1 || found[0].PID != agentPID || found[0].IsTracked {
		t.Fatalf("after owner death found = %+v, want untracked agent root pid %d", found, agentPID)
	}
}

// The controller holds many ACP sessions at once and the per-tick sweep scans
// with an empty id. One live session must not mark another session's roots
// tracked: the crashed session's escaped tool child is an orphan while the
// live session's agent is not.
func TestFindRuntimesBySessionIDTracksPerSessionAcrossSessions(t *testing.T) {
	p := newTestProvider(t)
	city := t.TempDir()
	liveName, deadName := testName()+"-live", testName()+"-dead"
	liveAgent, liveTool := startOrphaningSession(t, p, liveName, "sid-"+liveName, city)
	deadAgent, deadTool := startOrphaningSession(t, p, deadName, "sid-"+deadName, city)

	p.mu.Lock()
	deadConn := p.conns[deadName]
	p.mu.Unlock()
	if err := syscall.Kill(deadAgent, syscall.SIGKILL); err != nil {
		t.Fatalf("kill agent: %v", err)
	}
	select {
	case <-deadConn.done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent conn did not observe the process exit")
	}

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, p, "") }, liveAgent, liveTool, deadAgent, deadTool)
	byPID := make(map[int]runtime.LiveRuntime, len(found))
	for _, r := range found {
		byPID[r.PID] = r
	}
	if len(found) != 2 {
		t.Fatalf("found = %+v, want the live agent root and the dead session's tool root", found)
	}
	if r, ok := byPID[liveAgent]; !ok || !r.IsTracked || r.ProviderName != liveName {
		t.Fatalf("live agent = %+v (present %v), want tracked as %q", r, ok, liveName)
	}
	if r, ok := byPID[deadTool]; !ok || r.IsTracked || r.ProviderName != "" {
		t.Fatalf("dead session tool = %+v (present %v), want untracked", r, ok)
	}
}

// The reservation is in the table before the agent's control socket is bound.
// A root carrying this Provider's marker for a handshaking session is tracked
// from its own Start, not from a socket that may not exist yet.
func TestFindRuntimesBySessionIDTracksOwnedHandshakeBeforeSocketBind(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	sessionID := "sid-" + name
	p.mu.Lock()
	p.conns[name] = &sessionConn{done: make(chan struct{}), pending: make(map[int64]chan JSONRPCMessage)}
	p.mu.Unlock()
	if _, err := os.Stat(p.sockPath(name)); !os.IsNotExist(err) {
		t.Fatalf("control socket stat = %v, want not bound", err)
	}

	// Stand the test process in for the agent: its real stat keeps the scan's
	// root rules honest, and its snapshot environ carries what Start sets.
	agentPID := os.Getpid()
	root := snapshotProcRoot(t, agentPID)
	environ := "GC_SESSION_ID=" + sessionID + "\x00" + controlSocketEnv + "=" + p.controlSocketMarker(name) + "\x00"
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(agentPID), "environ"), []byte(environ), 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	restore := proctable.SetScanRootForTesting(root)
	found := findOnly(t, p, sessionID)
	restore()

	if len(found) != 1 || !found[0].IsTracked || found[0].ProviderName != name {
		t.Fatalf("found = %+v, want the handshaking agent tracked as %q", found, name)
	}
}
