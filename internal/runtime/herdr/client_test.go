package herdr

import (
	"path/filepath"
	"runtime"
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
//
// Both tests below are Linux-only: os.UserConfigDir() on darwin ignores
// XDG_CONFIG_HOME entirely and always resolves under
// "$HOME/Library/Application Support" (see the Go stdlib implementation),
// so asserting an XDG- or ".config"-rooted path is only valid on the
// platforms os.UserConfigDir() treats as XDG-following (this repo's
// non-Windows, non-Darwin default case). Whether herdr's own binary uses
// pure-XDG resolution on macOS too is unverified here — the empirical
// check above was run on Linux only — so the tests skip rather than assert
// an unconfirmed cross-platform contract.

func skipUnlessXDGPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("os.UserConfigDir() does not follow XDG_CONFIG_HOME on %s; herdr's own resolution on this platform is unverified (see ga-nqlb8q)", runtime.GOOS)
	}
}

func TestSocketPathHonorsXDGConfigHomeOverHome(t *testing.T) {
	skipUnlessXDGPlatform(t)
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
	skipUnlessXDGPlatform(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := newClient("hometest", "")
	if got, want := c.socketPath(), filepath.Join(home, ".config", "herdr", "sessions", "hometest", "herdr.sock"); got != want {
		t.Errorf("socketPath() = %q; want %q", got, want)
	}
}
