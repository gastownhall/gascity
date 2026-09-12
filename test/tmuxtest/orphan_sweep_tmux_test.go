package tmuxtest

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval covers the
// gap behind ga-t33q83: a tmux server whose socket lives inside a
// PID-prefixed socket parent dir must not survive that dir being swept.
// SweepOrphanPIDPrefixedDirs currently removes the dir with os.RemoveAll
// once its creator is confirmed dead, but never checks for or kills a tmux
// server still bound to a socket underneath it first -- RemoveAll unlinks
// the socket file out from under the server without touching the server
// process, leaving it running and orphaned (matching the "reparented to
// systemd" symptom in the bug report).
//
// This spawns a real tmux server inside a synthetic dead-creator fixture
// (the same fixture shape TestSweepOrphanPIDPrefixedDirsRemovesStaleDeadPIDWithNilDiagnostics
// uses) and asserts the server process itself -- checked by PID via
// pidutil.Alive, not by socket reachability, since the socket path is
// unlinked by the sweep regardless of whether the server process survives
// -- is gone after the sweep runs.
func TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval(t *testing.T) {
	RequireTmux(t)

	// Unix domain socket paths are capped at ~108 bytes (sizeof sun_path).
	// t.TempDir() embeds the full test name and is too long for that limit,
	// so this uses a short root directly under /tmp -- the same pattern
	// (and the same path-length reason) production's own
	// NewSocketParentDir("/tmp", ...) call sites use.
	root, err := os.MkdirTemp("/tmp", "tmuxtest-orphan-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	dir := pidPrefixedTestDir(t, root, "pfx-", nonLivePID(t))

	socket := filepath.Join(dir, "sock")
	if out, err := exec.Command("tmux", "-S", socket, "new-session", "-d", "-s", "orphantest", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("starting real tmux server at %s: %v: %s", socket, err, out)
	}

	pidOut, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", "orphantest", "#{pid}").Output()
	if err != nil {
		t.Fatalf("querying tmux server pid: %v", err)
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		t.Fatalf("parsing tmux server pid %q: %v", pidOut, err)
	}
	t.Cleanup(func() {
		// The socket may already be unlinked by the sweep under test, so
		// killing by socket path can't be relied on alone -- fall back to a
		// direct PID kill of the exact server this test spawned and
		// verified, never a bare/default-socket kill-server.
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run()
		if pidutil.Alive(serverPID) {
			_ = syscall.Kill(serverPID, syscall.SIGKILL)
		}
	})

	if !pidutil.Alive(serverPID) {
		t.Fatalf("tmux server pid %d not alive right after starting it", serverPID)
	}

	// Backdate only now that the server has finished creating its socket
	// file inside dir -- an earlier backdate would be overwritten by that
	// write touching dir's mtime again, leaving it under the sweep's
	// min-age guard (the same ordering TestSweepOrphanPIDPrefixedDirsRemovesFreeSentinel
	// uses relative to HoldAliveSentinel).
	backdatePastSweepAge(t, dir)

	SweepOrphanPIDPrefixedDirs(root, "pfx-", io.Discard)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("socket parent dir survived sweep, fixture invalid: %s", dir)
	}
	if pidutil.Alive(serverPID) {
		t.Errorf("tmux server pid %d at %s is still alive after its socket parent dir was swept -- orphaned, matches ga-t33q83", serverPID, socket)
	}
}
