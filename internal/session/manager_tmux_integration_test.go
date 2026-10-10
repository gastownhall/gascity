//go:build integration && linux

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// TestSuspend_RealTmuxDeletedSocketFailsAndDeadServerSucceeds pins
// runtime.StopForCleanup's missing-server rule against real tmux. A live
// server whose socket file was deleted answers "no server running" exactly as
// a dead one does, while its session keeps running; suspend must report that
// failure rather than record the seat suspended. Once the server is really
// dead, the same suspend succeeds.
func TestSuspend_RealTmuxDeletedSocketFailsAndDeadServerSucceeds(t *testing.T) {
	testSuspendDeletedSocketAndDeadServer(t, func(cfg tmux.Config) runtime.Provider { return tmux.NewProviderWithConfig(cfg) })
}

// TestSuspend_AutoRealTmuxDeletedSocketFailsAndDeadServerSucceeds is the same
// check through the auto router over the seam-backed tmux leaf, the shape a
// city with ACP agents composes: auto must neither merge the deleted socket's
// answer into success nor hide the tmux backend's confirmer from
// StopForCleanup.
func TestSuspend_AutoRealTmuxDeletedSocketFailsAndDeadServerSucceeds(t *testing.T) {
	testSuspendDeletedSocketAndDeadServer(t, func(cfg tmux.Config) runtime.Provider {
		return sessionauto.New(tmux.NewSeamBackedWithConfig(cfg), runtime.NewFake())
	})
}

// testSuspendDeletedSocketAndDeadServer runs the deleted-socket and
// dead-server suspends against the real tmux provider newProvider builds.
func testSuspendDeletedSocketAndDeadServer(t *testing.T, newProvider func(tmux.Config) runtime.Provider) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A private socket root, short enough for a unix socket path, keeps this
	// server off every other test's and the operator's sockets.
	socketRoot, err := os.MkdirTemp("/tmp", "gc-sus-")
	if err != nil {
		t.Fatalf("create socket root: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", socketRoot)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	cfg := tmux.DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-sus-%d", time.Now().UnixNano()%1e9)
	socketPath := filepath.Join(socketRoot, fmt.Sprintf("tmux-%d", os.Getuid()), cfg.SocketName)
	tm := tmux.NewTmuxWithConfig(cfg)

	mgr := newTestManager(t, beads.NewMemStore(), newProvider(cfg))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{ExplicitName: "sky", Template: "helper", Title: "test", Command: "sleep 600", WorkDir: t.TempDir(), Provider: "", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	panePID, err := tm.GetPanePID(info.SessionName)
	if err != nil {
		t.Fatalf("read pane pid: %v", err)
	}
	serverPID := parentPID(t, panePID)
	t.Cleanup(func() {
		// SIGUSR1 makes tmux recreate a deleted socket, so kill-server can
		// reach it. Each attempt is a tmux round trip, which paces the retry;
		// a server that never answers is stopped by its own pid.
		_ = syscall.Kill(serverPID, syscall.SIGUSR1)
		stopped := false
		for attempt := 0; attempt < 100 && !stopped; attempt++ {
			stopped = tm.KillServer() == nil || syscall.Kill(serverPID, 0) != nil
		}
		if !stopped {
			_ = syscall.Kill(serverPID, syscall.SIGTERM)
		}
		_ = os.RemoveAll(socketRoot)
	})

	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("delete live server socket: %v", err)
	}
	if err := mgr.Suspend(info.ID); err == nil {
		t.Fatal("Suspend = nil over a live server whose socket was deleted; its session is still running")
	}
	if err := syscall.Kill(serverPID, 0); err != nil {
		t.Fatalf("tmux server %d gone after the refused suspend: %v", serverPID, err)
	}
	if got, err := mgr.Get(info.ID); err != nil || got.State == StateSuspended {
		t.Fatalf("after the refused suspend: state = %q, err = %v; want the seat not suspended", got.State, err)
	}

	// Bring the socket back and stop the server for real. KillServer reads
	// "no server running" as success, so a kill-server that runs before the
	// server has recreated its socket stops nothing: wait for the socket
	// first, and for the server process to be gone after.
	if err := syscall.Kill(serverPID, syscall.SIGUSR1); err != nil {
		t.Fatalf("signal server to recreate its socket: %v", err)
	}
	waitFor(t, "the server to recreate its socket", func() bool {
		_, err := os.Stat(socketPath)
		return err == nil
	})
	if err := tm.KillServer(); err != nil {
		t.Fatalf("kill-server after SIGUSR1: %v", err)
	}
	waitFor(t, "the tmux server to exit", func() bool { return processGone(serverPID) })
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend against a dead server: %v", err)
	}
	if got, err := mgr.Get(info.ID); err != nil || got.State != StateSuspended {
		t.Fatalf("after suspending against a dead server: state = %q, err = %v; want suspended", got.State, err)
	}
}

// waitFor polls cond for up to 10s, failing the test with what it waited
// for if cond never holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// processGone reports whether pid has exited: /proc no longer lists it, or
// it is a zombie (a reaper in this sandbox may never collect it).
func processGone(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// parentPID returns the parent of pid from /proc: a pane's parent is the tmux
// server that spawned it.
func parentPID(t *testing.T, pid string) int {
	t.Helper()
	stat, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(pid), "stat"))
	if err != nil {
		t.Fatalf("read /proc/%s/stat: %v", pid, err)
	}
	// The command name may hold spaces; the fields after its closing paren are
	// state, then ppid.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if len(fields) < 2 {
		t.Fatalf("/proc/%s/stat = %q, want state and ppid", pid, stat)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid <= 1 {
		t.Fatalf("parent of pane %s = %q, want the tmux server's pid", pid, fields[1])
	}
	return ppid
}
