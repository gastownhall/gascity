//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// startTmuxServerInPrivateTmp starts a detached tmux server inside a new mount
// namespace whose /tmp is a private tmpfs -- the shape of a Bazel
// linux-sandbox test action, which mounts its own _hermetic_tmp over /tmp --
// with TMUX_TMPDIR=root created inside that private /tmp. With removeRoot the
// script deletes root after the server starts, leaving a live server whose
// root is gone in its own namespace. It returns the server's PID and skips
// when bwrap cannot build the namespace on this host. Every command runs
// through runGCBeadsBdCommand, the package's one shared command construction,
// so this file adds no call site to the subprocess census.
func startTmuxServerInPrivateTmp(t *testing.T, root string, removeRoot bool) int {
	t.Helper()
	tmuxtest.RequireTmux(t)
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed; cannot build a private-/tmp mount namespace")
	}
	if _, stderr, err := runGCBeadsBdCommand(t, os.Environ(), "bwrap", "--dev-bind", "/", "/", "--tmpfs", "/tmp", "--", "true"); err != nil {
		t.Skipf("bwrap cannot create a mount namespace on this host: %v: %s", err, stderr)
	}
	remove := "0"
	if removeRoot {
		remove = "1"
	}
	script := `set -eu
mkdir -p "$TMUX_TMPDIR"
tmux -L mntns new-session -d -s victim 'sleep 120'
tmux -L mntns display-message -p -t victim '#{pid}'
if [ "$GC_TEST_REMOVE_ROOT" = 1 ]; then rm -rf "$TMUX_TMPDIR"; fi`
	env := append(os.Environ(), "TMUX_TMPDIR="+root, "GC_TEST_REMOVE_ROOT="+remove)
	out, stderr, err := runGCBeadsBdCommand(t, env, "bwrap", "--dev-bind", "/", "/", "--tmpfs", "/tmp", "--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("start tmux server in a private /tmp: %v: %s", err, stderr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		t.Fatalf("bad tmux server pid %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if got, ok := procEnvValue(pid, "TMUX_TMPDIR"); !ok || got != root {
		t.Fatalf("server %d TMUX_TMPDIR = %q (found %v), want %q", pid, got, ok, root)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root %s is visible from this mount namespace (err=%v); the fixture needs it private", root, err)
	}
	return pid
}

// TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace
// pins ga-5hohvc: a live server whose socket root exists only in its own
// mount namespace -- a Bazel-sandboxed test's server, seen from an
// unsandboxed sweeper -- must survive the startup sweep. The fixture root sits
// outside the shape production startup sweeps own, and the sweep is handed a
// rule that owns exactly it.
func TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc; startup sweep degrades to no-op")
	}
	root := filepath.Join("/tmp", fmt.Sprintf("gct-mntnskept%d", os.Getpid()), "tmux")
	pid := startTmuxServerInPrivateTmp(t, root, false)

	var out bytes.Buffer
	sweepStaleTmuxServers("test", &out, func(r string) bool { return r == root })

	if !pidAlive(pid) {
		t.Fatalf("sweep reaped live tmux server %d whose socket root %s exists in its own mount namespace:\n%s", pid, root, out.String())
	}
	if strings.Contains(out.String(), fmt.Sprintf("pid=%d", pid)) {
		t.Fatalf("sweep report names the protected pid %d: %q", pid, out.String())
	}
}

// TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace
// is the other side of the same boundary: judging the root in the server's own
// namespace must still reap real residue -- a server whose root is gone where
// its run created it.
func TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc; startup sweep degrades to no-op")
	}
	root := filepath.Join("/tmp", fmt.Sprintf("gct-mntnsgone%d", os.Getpid()), "tmux")
	pid := startTmuxServerInPrivateTmp(t, root, true)

	var out bytes.Buffer
	sweepStaleTmuxServers("test", &out, func(r string) bool { return r == root })

	waitForPIDGone(t, pid)
	if !strings.Contains(out.String(), fmt.Sprintf("pid=%d", pid)) {
		t.Fatalf("sweep report missing reaped pid %d: %q", pid, out.String())
	}
}

// TestSocketRootGoneInOwnMountNamespace pins the predicate's fail-closed
// edges without a second namespace: only an observable process whose root is
// missing reads as gone.
func TestSocketRootGoneInOwnMountNamespace(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc")
	}
	present := t.TempDir()
	// A shell that prints its own PID and exits: Run has reaped it by the time
	// the PID comes back, so it names a process that is gone.
	out, _, err := runGCBeadsBdCommand(t, os.Environ(), "sh", "-c", "echo $$")
	if err != nil {
		t.Fatalf("run sh: %v", err)
	}
	exitedPID, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || exitedPID <= 0 {
		t.Fatalf("bad exited pid %q: %v", out, err)
	}
	for _, tc := range []struct {
		name string
		pid  int
		root string
		want bool
	}{
		{"root present", os.Getpid(), present, false},
		{"root missing", os.Getpid(), filepath.Join(present, "missing"), true},
		{"relative root", os.Getpid(), "missing/tmux", false},
		{"process gone", exitedPID, filepath.Join(present, "missing"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := socketRootGoneInOwnMountNamespace(tc.pid, tc.root); got != tc.want {
				t.Errorf("socketRootGoneInOwnMountNamespace(%d, %q) = %v, want %v", tc.pid, tc.root, got, tc.want)
			}
		})
	}
}
