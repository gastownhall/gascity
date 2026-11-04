package herdr

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
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

// recycleFakeHerdr is a stateful stand-in for the herdr CLI's workspace and tab
// verbs. It keeps the workspace and tab tables in files and reproduces the one
// herdr 0.8.0 behavior ensurePlacement's recycle depends on: closing a
// workspace's last tab closes the workspace with it, after which
// `tab create --workspace <id>` is rejected with a bare workspace_not_found
// envelope on stderr and a non-zero exit (the shape herdrAnswerCode reads, and
// herdrErrorCode does not).
type recycleFakeHerdr struct {
	bin   string
	state string
}

func newRecycleFakeHerdr(t *testing.T) *recycleFakeHerdr {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake herdr CLI is a POSIX shell script")
	}
	f := &recycleFakeHerdr{state: t.TempDir()}
	script := `#!/bin/sh
S='` + f.state + `'
shift 2 # drop --session <name>
echo "$*" >> "$S/log"
next() { n=$(cat "$S/next"); echo $((n + 1)) > "$S/next"; }
case "$1 $2" in
"workspace list")
	out=""
	while read -r id label; do
		[ -n "$id" ] || continue
		out="$out${out:+,}{\"workspace_id\":\"$id\",\"label\":\"$label\"}"
	done < "$S/workspaces"
	printf '{"result":{"workspaces":[%s]}}' "$out" ;;
"workspace create")
	next
	echo "w$n $4" >> "$S/workspaces"
	echo "t$n w$n default" >> "$S/tabs"
	printf '{"result":{"workspace":{"workspace_id":"w%s"},"tab":{"tab_id":"t%s"},"root_pane":{"pane_id":"p%s"}}}' "$n" "$n" "$n" ;;
"tab list")
	out=""
	while read -r id ws label; do
		[ "$ws" = "$4" ] || continue
		out="$out${out:+,}{\"tab_id\":\"$id\",\"label\":\"$label\"}"
	done < "$S/tabs"
	printf '{"result":{"tabs":[%s]}}' "$out" ;;
"tab rename")
	awk -v id="$3" -v l="$4" '$1 == id { $3 = l } 1' "$S/tabs" > "$S/tabs.new" && mv -f "$S/tabs.new" "$S/tabs"
	printf '{"result":{}}' ;;
"tab close")
	ws=$(awk -v id="$3" '$1 == id { print $2 }' "$S/tabs")
	awk -v id="$3" '$1 != id' "$S/tabs" > "$S/tabs.new" && mv -f "$S/tabs.new" "$S/tabs"
	if [ -n "$ws" ] && ! awk -v ws="$ws" '$2 == ws { found = 1 } END { exit !found }' "$S/tabs"; then
		awk -v ws="$ws" '$1 != ws' "$S/workspaces" > "$S/workspaces.new" && mv -f "$S/workspaces.new" "$S/workspaces"
	fi
	printf '{"result":{}}' ;;
"tab create")
	if [ -e "$S/tab_create_error" ]; then
		printf '{"error":{"code":"%s","message":"injected"}}' "$(cat "$S/tab_create_error")" >&2
		exit 1
	fi
	if ! awk -v ws="$4" '$1 == ws { found = 1 } END { exit !found }' "$S/workspaces"; then
		printf '{"error":{"code":"workspace_not_found","message":"workspace %s not found"}}' "$4" >&2
		exit 1
	fi
	next
	echo "t$n $4 $6" >> "$S/tabs"
	printf '{"result":{"tab":{"tab_id":"t%s"},"root_pane":{"pane_id":"p%s"}}}' "$n" "$n" ;;
*) printf '{"result":{}}' ;;
esac
`
	f.bin = writeFakeHerdr(t, script)
	return f
}

// seed starts the fake with one workspace labeled wsLabel (id w1) holding
// exactly one tab labeled tabLabel (id t1).
func (f *recycleFakeHerdr) seed(t *testing.T, wsLabel, tabLabel string) {
	t.Helper()
	for name, body := range map[string]string{
		"workspaces": "w1 " + wsLabel + "\n",
		"tabs":       "t1 w1 " + tabLabel + "\n",
		"next":       "2\n",
	} {
		if err := os.WriteFile(filepath.Join(f.state, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seeding fake herdr %s: %v", name, err)
		}
	}
}

// failTabCreate makes every `tab create` answer with code on stderr.
func (f *recycleFakeHerdr) failTabCreate(t *testing.T, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, "tab_create_error"), []byte(code), 0o600); err != nil {
		t.Fatalf("writing fake herdr tab_create_error: %v", err)
	}
}

// table returns the fields of a state table ("workspaces" or "tabs") in file
// order: id and label per workspace, id, workspace id and label per tab.
func (f *recycleFakeHerdr) table(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, name))
	if err != nil {
		t.Fatalf("reading fake herdr %s: %v", name, err)
	}
	return strings.Fields(string(b))
}

// invoked reports whether the fake saw a command line starting with prefix.
func (f *recycleFakeHerdr) invoked(t *testing.T, prefix string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, "log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading fake herdr log: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// herdr closes a workspace together with its last tab. When the stale tab
// ensurePlacement recycles is the workspace's only tab, the recycle therefore
// deletes the very workspace the replacement tab was about to be created in,
// and `tab create --workspace <id>` fails with workspace_not_found. The
// placement must recover by recreating the workspace, not fail the Start.
func TestEnsurePlacementRecreatesWorkspaceClosedWithItsOnlyStaleTab(t *testing.T) {
	f := newRecycleFakeHerdr(t)
	f.seed(t, "rig", "polecat-x")
	c := &client{session: "gc-test", bin: f.bin}

	tabID, paneID, err := c.ensurePlacement(context.Background(), "rig", "polecat-x", "/work", map[string]string{"K": "V"})
	if err != nil {
		t.Fatalf("ensurePlacement over a workspace whose only tab is stale = %v; want the workspace recreated", err)
	}
	if tabID == "" || paneID == "" || tabID == "t1" {
		t.Errorf("ensurePlacement = (tab %q, pane %q); want a fresh tab and pane, not the recycled t1", tabID, paneID)
	}

	if got := f.table(t, "workspaces"); len(got) != 2 || got[1] != "rig" {
		t.Fatalf("workspaces after ensurePlacement = %q; want exactly one, labeled rig", got)
	}
	wsID := f.table(t, "workspaces")[0]
	if got, want := f.table(t, "tabs"), []string{tabID, wsID, "polecat-x"}; !slices.Equal(got, want) {
		t.Errorf("tabs after ensurePlacement = %q; want only %q (the new tab, in the recreated workspace, under the agent's label)", got, want)
	}
}

// The recovery is for workspace_not_found only: any other tab-create rejection
// is the caller's to see, and must not be answered by minting a workspace.
func TestEnsurePlacementReturnsOtherTabCreateErrors(t *testing.T) {
	f := newRecycleFakeHerdr(t)
	f.seed(t, "rig", "polecat-x")
	f.failTabCreate(t, "internal_error")
	c := &client{session: "gc-test", bin: f.bin}

	_, _, err := c.ensurePlacement(context.Background(), "rig", "polecat-x", "/work", nil)
	if got := herdrAnswerCode(err); got != "internal_error" {
		t.Fatalf("ensurePlacement error = %v (code %q); want the internal_error rejection returned", err, got)
	}
	if f.invoked(t, "workspace create") {
		t.Error("ensurePlacement ran `workspace create` for a rejection other than workspace_not_found")
	}
}
