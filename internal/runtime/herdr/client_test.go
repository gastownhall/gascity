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
// herdr applies XDG Base Directory precedence on every platform it ships
// for — $XDG_CONFIG_HOME, else $HOME/.config (herdr src/config/io.rs
// config_dir) — verified empirically against the herdr binary on Linux
// (ga-nqlb8q) and against herdr 0.8.2 on macOS (#5770): with
// XDG_CONFIG_HOME set, `herdr --help` prints "Config: <dir>/herdr/config.toml"
// regardless of $HOME, and with it unset, "$HOME/.config/herdr/…". The
// tests below assert that contract on every platform, with no skip:
// os.UserConfigDir() is not that rule — on darwin it ignores XDG_CONFIG_HOME
// and resolves under "$HOME/Library/Application Support", a directory herdr
// never binds in, which is what made every start time out on macOS (#5770).

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
