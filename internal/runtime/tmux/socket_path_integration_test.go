//go:build integration

package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortTempDir returns a directory under /tmp, short enough that a tmux
// socket below it fits a unix sun_path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gcsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startPrivateTmuxServer starts a tmux server on the -L socket name in this
// process's environment, holding session, and kills it on cleanup.
func startPrivateTmuxServer(t *testing.T, name, session string) {
	t.Helper()
	if out, err := exec.Command("tmux", "-L", name, "new-session", "-d", "-s", session, "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("tmux -L %s new-session: %v: %s", name, err, out)
	}
	killPrivateTmuxServerOnCleanup(t, name)
}

// killPrivateTmuxServerOnCleanup kills the tmux server on the -L socket name,
// in this process's current environment, when t ends.
func killPrivateTmuxServerOnCleanup(t *testing.T, name string) {
	t.Helper()
	env := os.Environ()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "tmux", "-L", name, "kill-server")
		cmd.Env = env
		_ = cmd.Run()
	})
}

func tmuxReportedSocketPath(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("tmux", "-L", name, "display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("tmux -L %s display-message: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// AC-4: SocketPath agrees with the socket a real tmux server binds for -L,
// whatever TMUX_TMPDIR holds, and TMPDIR never moves it.
func TestSocketPathMatchesRealTmuxServer(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	root := shortTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cases := []struct {
		name  string
		unset bool
		value string
	}{
		{name: "unset", unset: true},
		{name: "empty", value: ""},
		{name: "symlink to directory", value: link},
		{name: "nonexistent directory", value: filepath.Join(root, "missing")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX_TMPDIR", tc.value)
			if tc.unset {
				if err := os.Unsetenv("TMUX_TMPDIR"); err != nil {
					t.Fatalf("unset TMUX_TMPDIR: %v", err)
				}
			}
			t.Setenv("TMPDIR", filepath.Join(root, "tmpdir-must-not-be-used"))
			name := privateSocketName("sp")
			startPrivateTmuxServer(t, name, "s1")
			if got, want := SocketPath(name), tmuxReportedSocketPath(t, name); got != want {
				t.Fatalf("SocketPath(%q) = %q, want tmux's #{socket_path} %q", name, got, want)
			}
		})
	}
	// A TMUX_TMPDIR naming a file binds nothing anywhere: tmux fails to
	// create its directory below the file instead of falling back to /tmp,
	// and SocketPath stays below the file with it.
	t.Run("file", func(t *testing.T) {
		file := filepath.Join(root, "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv("TMUX_TMPDIR", file)
		name := privateSocketName("sp")
		out, err := exec.Command("tmux", "-L", name, "new-session", "-d", "-s", "s1", "sleep 300").CombinedOutput()
		if err == nil {
			killPrivateTmuxServerOnCleanup(t, name)
			t.Fatalf("tmux started a server with TMUX_TMPDIR naming a file: %s", out)
		}
		dir := SocketDir(file)
		if !strings.Contains(string(out), dir) {
			t.Fatalf("tmux failed with %q, want it to name SocketDir %s", out, dir)
		}
		if got, want := SocketPath(name), filepath.Join(dir, name); got != want {
			t.Fatalf("SocketPath(%q) = %q, want %q", name, got, want)
		}
	})
}

// AC-4: with TMUX_TMPDIR naming a missing directory tmux binds under /tmp;
// ServerConfirmedDead must look there, so a live server is not dead and a
// killed one is. kill-server leaves the socket file refusing connections, so
// ProbeSession, resolving the same path, attests the killed server's session
// dead rather than holding it as unobservable.
func TestServerConfirmedDeadFollowsTmuxFallbackForMissingTMUXTMPDIR(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	t.Setenv("TMUX_TMPDIR", filepath.Join(shortTempDir(t), "missing"))
	name := privateSocketName("cd")
	cfg := DefaultConfig()
	cfg.SocketName = name
	p := NewProviderWithConfig(cfg)
	startPrivateTmuxServer(t, name, "s1")

	if p.ServerConfirmedDead() {
		t.Fatalf("ServerConfirmedDead() = true with a live server at %s, want false", tmuxReportedSocketPath(t, name))
	}
	if got, err := ProbeSession(context.Background(), name, "s1"); got != PresenceAlive {
		t.Fatalf("ProbeSession() = %v, want PresenceAlive on the live server (err=%v)", got, err)
	}
	if out, err := exec.Command("tmux", "-L", name, "kill-server").CombinedOutput(); err != nil {
		t.Fatalf("kill-server: %v: %s", err, out)
	}
	eventually(t, "ServerConfirmedDead() after kill-server", p.ServerConfirmedDead)
	if _, err := os.Lstat(SocketPath(name)); err != nil {
		t.Fatalf("socket file after kill-server: %v, want tmux to leave it in place", err)
	}
	if got, err := ProbeSession(context.Background(), name, "s1"); got != PresenceDead {
		t.Fatalf("ProbeSession() after kill-server = %v, want PresenceDead (err=%v)", got, err)
	}
}
