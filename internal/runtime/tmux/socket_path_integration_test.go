//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSocketPathMatchesRealTmuxSocketPath starts a real server with -L in each
// TMUX_TMPDIR shape and checks SocketPath against the server's own
// #{socket_path}, with TMPDIR pointed elsewhere so it cannot matter.
func TestSocketPathMatchesRealTmuxSocketPath(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	dir := shortResolvedDir(t)
	link := filepath.Join(shortResolvedDir(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	tests := []struct {
		name   string
		set    bool
		tmuxTD string
	}{
		{name: "unset"},
		{name: "empty", set: true},
		{name: "symlink to a directory", set: true, tmuxTD: link},
		{name: "nonexistent directory", set: true, tmuxTD: filepath.Join(dir, "missing")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("TMUX_TMPDIR", tt.tmuxTD)
			} else {
				unsetEnvForTest(t, "TMUX_TMPDIR")
			}
			t.Setenv("TMPDIR", shortResolvedDir(t))
			name := privateSocketName("sp")
			got := realServerSocketPath(t, name)
			if want := SocketPath(name); got != want {
				t.Fatalf("SocketPath(%s) = %q, real server #{socket_path} = %q", name, want, got)
			}
		})
	}
}

// TestServerConfirmedDeadFollowsTmuxMissingDirFallback starts a provider
// server while TMUX_TMPDIR names a missing directory, so tmux binds under
// /tmp. The provider must see it alive, and dead once kill-server's server
// process has exited (until then its listener is still bound).
func TestServerConfirmedDeadFollowsTmuxMissingDirFallback(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(shortResolvedDir(t), "missing"))
	p := newObjectServer(t)
	if p.ServerConfirmedDead() {
		t.Fatalf("ServerConfirmedDead() = true for a live server at %s", SocketPath(p.tm.cfg.SocketName))
	}
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
		t.Fatal("ServerConfirmedDead() = false after kill-server")
	}
}

// realServerSocketPath starts a detached server on the named -L socket in the
// current environment, registers its teardown, and returns #{socket_path}.
func realServerSocketPath(t *testing.T, name string) string {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", name, "new-session", "-d", "-s", "keep", "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("tmux -L %s new-session: %v: %s", name, err, out)
	}
	out, err := exec.Command("tmux", "-L", name, "display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		_ = exec.Command("tmux", "-L", name, "kill-server").Run()
		t.Fatalf("tmux -L %s display-message: %v", name, err)
	}
	path := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", path, "kill-server").Run() })
	return path
}

// shortResolvedDir returns a symlink-resolved directory under /tmp, short
// enough that a tmux socket inside it fits a unix socket address.
func shortResolvedDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", fmt.Sprintf("gcsp%d-", os.Getpid()))
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	return resolved
}
