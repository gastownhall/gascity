package herdr

import (
	"path/filepath"
	"testing"
)

// socketPath must resolve the SAME directory herdr itself uses for its
// config/socket state, or this client dials a path no herdr server ever
// binds: serverAlive() then always reads false against a healthy server
// ("did not become ready"), and every retry launches a redundant herdr
// server contending for the same pane ("agent_pane_busy") — ga-nqlb8q.
// Verified empirically against the herdr binary itself: `XDG_CONFIG_HOME=X
// herdr --help` prints "Config: X/herdr/config.toml" regardless of $HOME,
// and with XDG_CONFIG_HOME unset it falls back to "$HOME/.config/herdr/…" —
// standard XDG Base Directory precedence, which os.UserConfigDir()
// implements and the old os.UserHomeDir()+".config" join did not: a sandbox
// that sets XDG_CONFIG_HOME to the real user's config dir while redirecting
// $HOME elsewhere (this fleet's agent sandboxes do exactly that) made the
// old code compute a path no herdr process ever binds.

func TestSocketPathHonorsXDGConfigHomeOverHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // deliberately different; must be ignored

	c := newClient("xdgtest", "")
	if got, want := c.socketPath(), filepath.Join(xdg, "herdr", "sessions", "xdgtest", "herdr.sock"); got != want {
		t.Errorf("socketPath() = %q; want %q", got, want)
	}

	c.session = "default"
	if got, want := c.socketPath(), filepath.Join(xdg, "herdr", "herdr.sock"); got != want {
		t.Errorf("socketPath() (default session) = %q; want %q", got, want)
	}
}

func TestSocketPathFallsBackToHomeConfigWhenXDGUnset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := newClient("hometest", "")
	if got, want := c.socketPath(), filepath.Join(home, ".config", "herdr", "sessions", "hometest", "herdr.sock"); got != want {
		t.Errorf("socketPath() = %q; want %q", got, want)
	}
}
