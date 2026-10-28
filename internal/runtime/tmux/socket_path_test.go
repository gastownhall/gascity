package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// resolvedTmpBase is the root tmux falls back to: the realpath of /tmp — on
// some hosts /tmp is itself a symlink — or "/tmp" when even that cannot be
// resolved.
func resolvedTmpBase() string {
	if resolved, err := filepath.EvalSymlinks("/tmp"); err == nil {
		return resolved
	}
	return "/tmp"
}

// TestSocketPathResolvesTheBaseTheWayTmuxDoes pins the resolution the
// namespace fix relies on: SocketPath answers the path tmux itself binds —
// the realpath of the socket root, never a verbatim TMUX_TMPDIR join that
// can point where no server ever listens. tmux realpaths its base and falls
// back to /tmp when TMUX_TMPDIR names a missing directory; TMPDIR never
// takes part.
func TestSocketPathResolvesTheBaseTheWayTmuxDoes(t *testing.T) {
	uidDir := fmt.Sprintf("tmux-%d", os.Getuid())
	under := func(base string) string { return filepath.Join(base, uidDir, "gc-test") }

	t.Run("empty TMUX_TMPDIR is the same as unset", func(t *testing.T) {
		t.Setenv("TMUX_TMPDIR", "")
		if got, want := SocketPath("gc-test"), under(resolvedTmpBase()); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})

	t.Run("TMUX_TMPDIR naming a real directory resolves to its realpath", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("resolve temp dir: %v", err)
		}
		t.Setenv("TMUX_TMPDIR", dir)
		if got, want := SocketPath("gc-test"), under(dir); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})

	t.Run("TMUX_TMPDIR naming a symlink resolves to what it points at", func(t *testing.T) {
		root := t.TempDir()
		socketDir := filepath.Join(root, "r")
		link := filepath.Join(root, "l")
		if err := os.Mkdir(socketDir, 0o700); err != nil {
			t.Fatalf("create socket dir: %v", err)
		}
		if err := os.Symlink(socketDir, link); err != nil {
			t.Fatalf("link %s -> %s: %v", link, socketDir, err)
		}
		t.Setenv("TMUX_TMPDIR", link)
		if got, want := SocketPath("gc-test"), under(socketDir); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})

	t.Run("TMUX_TMPDIR naming a missing directory falls back to tmp", func(t *testing.T) {
		t.Setenv("TMUX_TMPDIR", filepath.Join(t.TempDir(), "absent"))
		if got, want := SocketPath("gc-test"), under(resolvedTmpBase()); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})

	t.Run("a trailing slash resolves like the bare directory", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("resolve temp dir: %v", err)
		}
		t.Setenv("TMUX_TMPDIR", dir+string(os.PathSeparator))
		if got, want := SocketPath("gc-test"), under(dir); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})

	t.Run("TMPDIR is never consulted", func(t *testing.T) {
		root := t.TempDir()
		socketDir := filepath.Join(root, "r")
		link := filepath.Join(root, "l")
		if err := os.Mkdir(socketDir, 0o700); err != nil {
			t.Fatalf("create socket dir: %v", err)
		}
		if err := os.Symlink(socketDir, link); err != nil {
			t.Fatalf("link %s -> %s: %v", link, socketDir, err)
		}
		t.Setenv("TMUX_TMPDIR", link)
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "must-not-be-used"))
		if got, want := SocketPath("gc-test"), under(socketDir); got != want {
			t.Fatalf("SocketPath() = %q, want %q", got, want)
		}
	})
}
