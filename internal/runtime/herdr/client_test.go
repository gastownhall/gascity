package herdr

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// socketPath must resolve the SAME directory herdr itself uses for its
// config/socket state, or this client dials a path no herdr server ever
// binds: serverAlive() then always reads false against a healthy server
// ("did not become ready"), and every retry launches a redundant herdr
// server contending for the same pane ("agent_pane_busy") — ga-nqlb8q.
// herdr resolves that directory by one rule on every platform it ships
// for — $XDG_CONFIG_HOME, else $HOME/.config (herdr src/config/io.rs
// config_dir) — verified empirically against the herdr binary on Linux
// (ga-nqlb8q) and against herdr 0.8.2 on macOS (#5770): with
// XDG_CONFIG_HOME set, `herdr --help` prints "Config: <dir>/herdr/config.toml"
// regardless of $HOME, and with it unset, "$HOME/.config/herdr/…". The
// tests below assert that contract on every platform, with no skip:
// os.UserConfigDir() is not that rule — on darwin it ignores XDG_CONFIG_HOME
// and resolves under "$HOME/Library/Application Support", a directory herdr
// never binds in, which is what made every start time out on macOS (#5770).

// unsetEnv makes key absent for the rest of the test; t.Setenv's cleanup
// restores the inherited value. Setting it to "" is not the same thing:
// herdr counts a variable that is set but empty as set.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetting %s: %v", key, err)
	}
}

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
	unsetEnv(t, "XDG_CONFIG_HOME")
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := newClient("hometest", "")
	if got, want := c.socketPath(), filepath.Join(home, ".config", "herdr", "sessions", "hometest", "herdr.sock"); got != want {
		t.Errorf("socketPath() = %q; want %q", got, want)
	}
}

// Environments where os.UserConfigDir() fails — a relative XDG_CONFIG_HOME,
// or neither XDG_CONFIG_HOME nor HOME set — must still resolve the socket
// the way herdr does: herdr uses a relative XDG_CONFIG_HOME as-is, and with
// neither variable set it roots under the temp dir. A resolver that
// discards the os.UserConfigDir() error joins onto an empty config root and
// dials the cwd-relative "herdr/sessions/<name>/herdr.sock".
func TestSocketPathMatchesHerdrWhereUserConfigDirFails(t *testing.T) {
	t.Run("relative_xdg", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "rel")
		t.Setenv("HOME", t.TempDir())

		c := newClient("reltest", "")
		if got, want := c.socketPath(), filepath.Join("rel", "herdr", "sessions", "reltest", "herdr.sock"); got != want {
			t.Errorf("socketPath() = %q; want %q", got, want)
		}
	})
	t.Run("no_xdg_no_home", func(t *testing.T) {
		unsetEnv(t, "XDG_CONFIG_HOME")
		unsetEnv(t, "HOME")

		c := newClient("tmptest", "")
		if got, want := c.socketPath(), filepath.Join(os.TempDir(), "herdr", "sessions", "tmptest", "herdr.sock"); got != want {
			t.Errorf("socketPath() = %q; want %q", got, want)
		}
	})
}

// herdr reads XDG_CONFIG_HOME and HOME with std::env::var, which returns a
// variable that is set but empty as Ok(""), and joins that like any other
// value: PathBuf::from("").join("herdr") is the relative "herdr". The XDG
// Base Directory spec's empty-means-unset rule is not herdr's, so it must not
// be socketPath's either.
func TestSocketPathCountsSetButEmptyVariablesAsSet(t *testing.T) {
	t.Run("empty_xdg", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", t.TempDir()) // must be ignored: XDG_CONFIG_HOME is set

		c := newClient("emptyxdg", "")
		if got, want := c.socketPath(), filepath.Join("herdr", "sessions", "emptyxdg", "herdr.sock"); got != want {
			t.Errorf("socketPath() = %q; want %q", got, want)
		}
	})
	t.Run("no_xdg_empty_home", func(t *testing.T) {
		unsetEnv(t, "XDG_CONFIG_HOME")
		t.Setenv("HOME", "")

		c := newClient("emptyhome", "")
		if got, want := c.socketPath(), filepath.Join(".config", "herdr", "sessions", "emptyhome", "herdr.sock"); got != want {
			t.Errorf("socketPath() = %q; want %q", got, want)
		}
	})
}

// A relative socket path resolves against the working directory of whichever
// process uses it: the herdr server runs in the city root, while gc and the
// herdr CLI calls run in gc's own. startServer must refuse one before it
// dials, unlinks or launches anything, with an error that names the variables
// to fix and both directories.
func TestStartServerRefusesRelativeSocketPath(t *testing.T) {
	gcDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	standaloneHome := regexp.MustCompile(`\bHOME\b`) // not the HOME inside XDG_CONFIG_HOME
	for _, tc := range []struct {
		name string
		env  func(t *testing.T)
	}{
		{"relative_xdg", func(t *testing.T) { t.Setenv("XDG_CONFIG_HOME", "rel") }},
		{"empty_xdg", func(t *testing.T) { t.Setenv("XDG_CONFIG_HOME", "") }},
		{"no_xdg_empty_home", func(t *testing.T) {
			unsetEnv(t, "XDG_CONFIG_HOME")
			t.Setenv("HOME", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			tc.env(t)
			fake := newFakeHerdr(t, filepath.Join(t.TempDir(), "unused-probe"))
			cityRoot := t.TempDir()
			c := newClient("reltest", cityRoot)
			c.bin = fake.bin
			sock := c.socketPath()
			if filepath.IsAbs(sock) {
				t.Fatalf("test setup: socketPath() = %q; want a relative path", sock)
			}

			err := c.startServer()
			if err == nil {
				t.Fatal("startServer() = nil for a relative socket path; want a refusal")
			}
			for _, want := range []string{sock, "XDG_CONFIG_HOME", cityRoot, gcDir} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("startServer() error %q does not name %q", err, want)
				}
			}
			if !standaloneHome.MatchString(err.Error()) {
				t.Errorf("startServer() error %q does not name HOME", err)
			}
			if calls := fake.logLines(t); len(calls) != 0 {
				t.Errorf("startServer() ran herdr %q; want no launch", calls)
			}
		})
	}
}

// TestGetAgentResolvesASessionNamedLikeAKindToItsOwnPane guards the other side
// of matching by registered name only: a session whose registered name really
// is a kind name resolves to its own pane, both when `agent get` answers for
// it and when the lookup falls back to scanning `agent list`, where panes of
// that kind nobody named and other sessions of that kind are listed first.
func TestGetAgentResolvesASessionNamedLikeAKindToItsOwnPane(t *testing.T) {
	const namedOpencode = `{"agent":"opencode","name":"opencode","pane_id":"w5:p1","agent_status":"idle"}`
	listed := []string{unnamedOpencodePane, namedOpencodeAlpha, namedOpencodeBeta, namedOpencode}
	for _, tc := range []struct {
		name, getReply string
	}{
		{"agent get answers", `{"result":{"agent":` + namedOpencode + `}}`},
		{"agent get answers not-found", agentGetNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := lookupFake(t, tc.getReply, listed...)
			c := &client{session: "gc-test", bin: bin}
			a, ok, err := c.getAgent(context.Background(), "opencode")
			if err != nil || !ok || a.PaneID != "w5:p1" || a.Name != "opencode" {
				t.Errorf("getAgent(%q) = (%+v, %v, %v); want the session's own pane w5:p1", "opencode", a, ok, err)
			}
		})
	}
}
