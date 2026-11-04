package herdr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
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

// failingStartClient returns a client whose herdr binary is a POSIX shell
// script (body receives the path of the server.log startServer must keep beside
// the session socket) and whose socket therefore never comes up, plus that
// log's path. The socket's directory is deliberately not created: herdr makes
// it, and startServer may be the first to need it. readyPolls is cut to one so
// the readiness timeout takes 250ms, not 10s.
func failingStartClient(t *testing.T, body func(logPath string) string) (*client, string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake herdr server is a POSIX shell script")
	}
	sockDir := filepath.Join(t.TempDir(), "sessions", "failing")
	logPath := filepath.Join(sockDir, "server.log")
	c := newClient("failing", t.TempDir())
	c.bin = writeFakeHerdr(t, "#!/bin/sh\n"+body(logPath))
	c.sockPath = filepath.Join(sockDir, "herdr.sock")
	c.readyPolls = 1
	return c, logPath
}

// plainReadyError is the readiness-timeout message startServer returned before
// it captured herdr's stderr, and still returns whenever there is nothing to
// add to it.
func plainReadyError(c *client) string {
	return fmt.Sprintf("herdr server for session %q did not become ready on %q", c.session, c.socketPath())
}

// herdr's startup diagnostics go to stderr, and startServer used to send them
// to /dev/null: a server that exited ~50ms in with "local socket name length
// exceeds capacity of sun_path" surfaced only as a cause-free 10s timeout, read
// as host load (ga-nqlb8q) and as a stale-session restore (ga-0og0ry). The
// timeout error must carry what herdr said; stdout stays discarded.
func TestStartServerTimeoutErrorCarriesServerStderr(t *testing.T) {
	const diag = "local socket name length exceeds capacity of sun_path of sockaddr_un"
	c, logPath := failingStartClient(t, func(string) string {
		return "printf 'stdout-noise-must-stay-discarded\\n'\nprintf '" + diag + "\\n' >&2\nexit 1\n"
	})

	err := c.startServer()
	if err == nil {
		t.Fatal("startServer() = nil for a herdr that never serves; want the readiness timeout")
	}
	if !strings.Contains(err.Error(), plainReadyError(c)) {
		t.Errorf("startServer() error %q lost the readiness-timeout message %q", err, plainReadyError(c))
	}
	if !strings.Contains(err.Error(), diag) {
		t.Errorf("startServer() error %q does not carry herdr's stderr %q", err, diag)
	}
	if strings.Contains(err.Error(), "stdout-noise") {
		t.Errorf("startServer() error %q carries herdr's stdout; only stderr is captured", err)
	}
	b, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("herdr's stderr was not kept at %s beside the socket: %v", logPath, rerr)
	}
	if strings.Contains(string(b), "stdout-noise") {
		t.Errorf("%s holds herdr's stdout %q; stdout must stay discarded", logPath, b)
	}
}

// Each launch starts a fresh log: a prior run's output explains nothing about
// this one, and truncating bounds the file across a long-lived city's restarts.
func TestStartServerTruncatesServerLogPerLaunch(t *testing.T) {
	c, logPath := failingStartClient(t, func(string) string {
		return "printf 'this-launch\\n' >&2\nexit 1\n"
	})
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("prior-launch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := c.startServer()
	if err == nil {
		t.Fatal("startServer() = nil; want the readiness timeout")
	}
	if !strings.Contains(err.Error(), "this-launch") {
		t.Errorf("startServer() error %q does not carry this launch's stderr", err)
	}
	if strings.Contains(err.Error(), "prior-launch") {
		t.Errorf("startServer() error %q carries a prior launch's stderr; the log must be truncated", err)
	}
}

// The error carries only the last 4096 bytes of the log: a runaway herdr must
// not bloat the error string every caller up the chain wraps again.
func TestStartServerErrorBoundsTheStderrTail(t *testing.T) {
	const bound = 4096
	c, _ := failingStartClient(t, func(string) string {
		// ~12KB of filler between a first and a last line.
		return "echo head-marker >&2\ni=0\nwhile [ $i -lt 300 ]; do echo 'filler-filler-filler-filler-filler-filler' >&2; i=$((i+1)); done\necho tail-marker >&2\nexit 1\n"
	})

	err := c.startServer()
	if err == nil {
		t.Fatal("startServer() = nil; want the readiness timeout")
	}
	if !strings.Contains(err.Error(), "tail-marker") {
		t.Errorf("startServer() error does not end with herdr's last line: %.200q", err)
	}
	if strings.Contains(err.Error(), "head-marker") {
		t.Errorf("startServer() error carries herdr's first line; only the last %d bytes belong in it", bound)
	}
	if overhead := len(plainReadyError(c)) + 256; len(err.Error()) > bound+overhead {
		t.Errorf("startServer() error is %d bytes; want the %d-byte tail plus at most %d of message", len(err.Error()), bound, overhead)
	}
}

// Diagnostics are best effort. A server.log that cannot be opened must not stop
// the server launching (stderr falls back to /dev/null), and a log that cannot
// be read back, or that herdr never wrote to, must leave the readiness timeout
// exactly as it was: never replaced, never masked, never a panic.
func TestStartServerDiagnosticsFailuresLeaveThePlainTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		// body is the fake herdr's script; marker is a file it touches when
		// launched. prep runs before startServer.
		body func(logPath, marker string) string
		prep func(t *testing.T, logPath string)
	}{
		{
			name: "log_unopenable",
			body: func(_, marker string) string { return "touch '" + marker + "'\necho unseen >&2\nexit 1\n" },
			prep: func(t *testing.T, logPath string) {
				// A directory where the file belongs: O_WRONLY open fails.
				if err := os.MkdirAll(logPath, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "log_gone_by_read_back",
			body: func(logPath, marker string) string {
				return "touch '" + marker + "'\nrm -f '" + logPath + "'\necho unseen >&2\nexit 1\n"
			},
		},
		{
			name: "herdr_wrote_nothing",
			body: func(_, marker string) string { return "touch '" + marker + "'\nexit 1\n" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "launched")
			c, logPath := failingStartClient(t, func(logPath string) string { return tc.body(logPath, marker) })
			if tc.prep != nil {
				tc.prep(t, logPath)
			}

			err := c.startServer()
			if err == nil {
				t.Fatal("startServer() = nil; want the readiness timeout")
			}
			if got, want := err.Error(), plainReadyError(c); got != want {
				t.Errorf("startServer() error = %q; want the plain %q", got, want)
			}
			if _, serr := os.Stat(marker); serr != nil {
				t.Errorf("herdr was never launched (%v): a diagnostics failure must not block server startup", serr)
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
