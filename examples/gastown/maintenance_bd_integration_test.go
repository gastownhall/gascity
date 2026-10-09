//go:build integration || dolt_integration

package gastown_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// These tests drive the shipped maintenance orders against REAL bd scopes of
// every topology — bd-owned proxied, direct server (the gc-managed shape) and
// a mix of both — through a thin `gc` router that does what `gc bd --city C
// [--rig R]` does for a scope: point bd at the scope's .beads directory and,
// for a proxied scope, select bd's proxied route. The orders themselves never
// learn which topology they are on.
//
// bd comes from GC_TEST_BD_BIN or PATH (CI pins v1.3.0). Steps that need a bd
// newer than the pin skip with the reason when bd lacks them:
//   - the reaper's Step 3 needs `bd purge --wisps-plane --limit` (the beads
//     purge hotfix);
//   - backing up a proxied scope needs proxied `bd backup` (beads PR 6879).
// Fixtures live in t.TempDir with an isolated HOME and never touch port 3307.

type bdTopologyScope struct {
	name    string // "" for the city
	dir     string
	db      string
	prefix  string
	proxied bool
}

type bdTopologyCity struct {
	t        *testing.T
	bd       string
	root     string
	cityDir  string
	binDir   string
	gcLog    string
	env      []string
	scopes   []bdTopologyScope
	serverPt int
}

func requireRealBd(t *testing.T) string {
	t.Helper()
	bd := os.Getenv("GC_TEST_BD_BIN")
	if bd == "" {
		var err error
		bd, err = exec.LookPath("bd")
		if err != nil {
			t.Skip("bd not found (set GC_TEST_BD_BIN or put bd on PATH)")
		}
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not found")
	}
	return bd
}

func newBdTopologyCity(t *testing.T, bd string) *bdTopologyCity {
	t.Helper()
	root := t.TempDir()
	c := &bdTopologyCity{
		t:       t,
		bd:      bd,
		root:    root,
		cityDir: filepath.Join(root, "city"),
		binDir:  filepath.Join(root, "bin"),
		gcLog:   filepath.Join(root, "gc.log"),
	}
	home := filepath.Join(root, "home")
	for _, dir := range []string{c.cityDir, c.binDir, home, filepath.Join(root, "xdg"), filepath.Join(root, "gchome")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testutil.SeedDoltGlobalConfig(home); err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		key := entry[:strings.IndexByte(entry, '=')]
		if strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") || strings.HasPrefix(key, "GC_") ||
			key == "HOME" || key == "XDG_CONFIG_HOME" || key == "PATH" {
			continue
		}
		c.env = append(c.env, entry)
	}
	c.env = append(c.env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(root, "xdg"),
		"GC_HOME="+filepath.Join(root, "gchome"),
		"BD_DISABLE_METRICS=1",
		"BEADS_NO_DAEMON=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"PATH="+c.binDir+string(os.PathListSeparator)+filepath.Dir(bd)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY="+c.cityDir,
		"GC_CITY_PATH="+c.cityDir,
		"GC_ESCALATE_SCRIPT="+filepath.Join(c.binDir, "escalate.sh"),
		"DOLT_ESCALATE_SCRIPT="+filepath.Join(c.binDir, "escalate.sh"),
		"GC_CALL_LOG="+c.gcLog,
	)
	// Every HTTPS request a child makes goes to a black hole. A proxied scope's
	// `bd init` probes `dolt version`, whose update check is a network call
	// unless HOME's dolt config disables it; on a healthy network that call
	// returns in 0.2 s and hides a missing config, so the slow network is
	// reproduced deterministically instead of waited for.
	c.env = append(c.env, testutil.UnroutableHTTPSProxyEnv()...)
	writeExecutable(t, filepath.Join(c.binDir, "escalate.sh"), "#!/bin/sh\nprintf 'ESCALATION %s\\n' \"$*\" >> \""+c.gcLog+"\"\n")
	t.Cleanup(c.stopProcesses)
	return c
}

func (c *bdTopologyCity) run(t *testing.T, dir string, extra []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, c.env...), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *bdTopologyCity) scopeEnv(s bdTopologyScope) []string {
	env := []string{"BEADS_DIR=" + filepath.Join(s.dir, ".beads")}
	if s.proxied {
		env = append(env, "BEADS_DOLT_PROXIED_SERVER=1")
	}
	return env
}

// bdIn runs bd in a scope and returns its stdout (bd's warnings go to
// stderr, which is reported only on failure).
func (c *bdTopologyCity) bdIn(t *testing.T, s bdTopologyScope, args ...string) string {
	t.Helper()
	cmd := exec.Command(c.bd, args...)
	cmd.Dir = s.dir
	cmd.Env = append(append([]string{}, c.env...), c.scopeEnv(s)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bd %s in %s: %v\n%s%s", strings.Join(args, " "), s.dir, err, out, stderr.String())
	}
	return string(out)
}

// addScope initializes a bd scope of the given topology.
func (c *bdTopologyCity) addScope(t *testing.T, name, db, prefix string, proxied bool) bdTopologyScope {
	t.Helper()
	dir := c.cityDir
	if name != "" {
		dir = filepath.Join(c.root, "rigs", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run(t, dir, nil, "git", "init", "-q", "."); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
	s := bdTopologyScope{name: name, dir: dir, db: db, prefix: prefix, proxied: proxied}
	args := []string{"init", "--quiet"}
	if proxied {
		args = append(args, "--proxied-server", "--proxied-server-idle-timeout", "120s")
	} else {
		args = append(args, "--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(c.ensureServer(t)))
	}
	args = append(args, "-p", prefix, "--database", db, "--skip-hooks", "--skip-agents", dir)
	if proxied {
		out, err := retryOnBdProxyStartTimeout(
			func() (string, error) { return c.run(t, dir, c.scopeEnv(s), c.bd, args...) },
			func() error {
				c.signalScopeProcesses(s, syscall.SIGKILL)
				return os.RemoveAll(filepath.Join(dir, ".beads"))
			},
		)
		if err != nil {
			t.Fatalf("bd %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
	} else {
		c.bdIn(t, s, args...)
	}
	c.scopes = append(c.scopes, s)
	c.writeRouter(t)
	return s
}

// ensureServer starts one dolt sql-server for the direct-server scopes on a
// free port (never 3307).
func (c *bdTopologyCity) ensureServer(t *testing.T) int {
	t.Helper()
	if c.serverPt != 0 {
		return c.serverPt
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dataDir := filepath.Join(c.root, "server-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Dir = dataDir
	cmd.Env = c.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logFile, err := os.Create(filepath.Join(c.root, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		_ = logFile.Close()
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not listen on %d", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.serverPt = port
	return port
}

// writeRouter installs the `gc` router for the scopes known so far.
func (c *bdTopologyCity) writeRouter(t *testing.T) {
	t.Helper()
	var rigs []string
	var cases strings.Builder
	for _, s := range c.scopes {
		key := "city"
		if s.name != "" {
			key = "rig:" + s.name
			rigs = append(rigs, fmt.Sprintf(`{"name":%q,"hq":false,"path":%q}`, s.name, s.dir))
		}
		proxied := ""
		if s.proxied {
			proxied = "export BEADS_DOLT_PROXIED_SERVER=1"
		}
		fmt.Fprintf(&cases, "  %q) scope_dir=%q; %s ;;\n", key, s.dir, proxied)
	}
	rigList := `{"rigs":[` + strings.Join(rigs, ",") + `]}`
	writeExecutable(t, filepath.Join(c.binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$GC_CALL_LOG"
case "${1:-} ${2:-}" in
  "rig list") printf '%%s\n' %s; exit 0 ;;
  "session prune") printf '{"count":0}\n'; exit 0 ;;
  "session nudge"|"mail send") exit 0 ;;
esac
[ "${1:-}" = "bd" ] || exit 0
shift
[ "${1:-}" = "--city" ] && shift 2
scope=city
if [ "${1:-}" = "--rig" ]; then scope="rig:$2"; shift 2; fi
unset BEADS_DOLT_PROXIED_SERVER
case "$scope" in
%s  *) echo "router: unknown scope $scope" >&2; exit 1 ;;
esac
cd "$scope_dir" || exit 1
# ROUTER_REJECT_CLOSE=<id> stands in for bd rejecting that bead's close at
# run time (e.g. it was re-claimed after the reaper selected it).
if [ "${1:-}" = "close" ] && [ -n "${ROUTER_REJECT_CLOSE:-}" ]; then
  case " $* " in
    *" $ROUTER_REJECT_CLOSE "*)
      printf 'close %%s: claimed by another actor since it was selected\n' "$ROUTER_REJECT_CLOSE" >&2
      exit 1
      ;;
  esac
fi
BEADS_DIR="$scope_dir/.beads" exec %s "$@"
`, shellSingleQuote(rigList), cases.String(), shellSingleQuote(c.bd)))
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// stopProcesses kills the bd proxies and Dolt children this fixture's proxied
// scopes started, found through their own pidfiles.
func (c *bdTopologyCity) stopProcesses() {
	for _, s := range c.scopes {
		if s.proxied {
			c.signalScopeProcesses(s, syscall.SIGTERM)
		}
	}
}

// signalScopeProcesses signals the processes a proxied scope's pid records
// name: the proxy (proxy.pid) and its dolt backend (proxy-child.pid).
func (c *bdTopologyCity) signalScopeProcesses(s bdTopologyScope, sig syscall.Signal) {
	for _, name := range []string{"proxy.pid", "proxy-child.pid"} {
		data, err := os.ReadFile(filepath.Join(s.dir, ".beads", "dolt", name))
		if err != nil {
			continue
		}
		var rec struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.PID <= 1 {
			continue
		}
		_ = syscall.Kill(rec.PID, sig)
	}
}

// bdProxyStartTimeout is how bd reports a proxied first start that missed its
// fixed 15 s open deadline (beads internal/storage/dbproxy/proxy/endpoint.go).
// Two host conditions cause it, neither a fault of the scope being created: the
// backend Dolt port that bd picked by bind-and-close for .beads/dolt/config.yaml
// was taken before dolt sql-server bound it (beads#7184 recovers from that, in
// no release yet), or a disk stall held the backend's boot past the deadline.
// The failed init leaves .beads behind with that port recorded, and bd init
// then refuses the scope, so a retry has to start from a clean .beads.
const bdProxyStartTimeout = "timeout waiting for proxy to become ready on its OS-assigned port"

// retryOnBdProxyStartTimeout runs init once more, after reset, when the first
// attempt failed with bdProxyStartTimeout. Any other failure is returned as is.
func retryOnBdProxyStartTimeout(init func() (string, error), reset func() error) (string, error) {
	out, err := init()
	if err == nil || !strings.Contains(out, bdProxyStartTimeout) {
		return out, err
	}
	if resetErr := reset(); resetErr != nil {
		return out, fmt.Errorf("%w; resetting the scope for a retry: %w", err, resetErr)
	}
	return init()
}

func TestRetryOnBdProxyStartTimeoutRetriesOnlyKnownSignature(t *testing.T) {
	timeoutOut := "Error: failed to open uow provider: uow: get proxy endpoint: " + bdProxyStartTimeout
	errExit := fmt.Errorf("exit status 1")

	t.Run("retries once from a reset scope", func(t *testing.T) {
		var calls []string
		out, err := retryOnBdProxyStartTimeout(
			func() (string, error) {
				calls = append(calls, "init")
				if len(calls) == 1 {
					return timeoutOut, errExit
				}
				return "ok", nil
			},
			func() error { calls = append(calls, "reset"); return nil },
		)
		if err != nil || out != "ok" {
			t.Fatalf("got (%q, %v), want (ok, nil)", out, err)
		}
		if got := strings.Join(calls, ","); got != "init,reset,init" {
			t.Fatalf("calls = %s, want init,reset,init", got)
		}
	})
	t.Run("does not retry other failures", func(t *testing.T) {
		calls := 0
		_, err := retryOnBdProxyStartTimeout(
			func() (string, error) { calls++; return "Error: database not found", errExit },
			func() error { t.Fatal("reset called for an unrelated failure"); return nil },
		)
		if err == nil || calls != 1 {
			t.Fatalf("err = %v after %d call(s), want the first failure after 1 call", err, calls)
		}
	})
	t.Run("retries at most once", func(t *testing.T) {
		calls := 0
		out, err := retryOnBdProxyStartTimeout(
			func() (string, error) { calls++; return timeoutOut, errExit },
			func() error { return nil },
		)
		if err == nil || calls != 2 || !strings.Contains(out, bdProxyStartTimeout) {
			t.Fatalf("got (%q, %v) after %d calls, want the second timeout after 2 calls", out, err, calls)
		}
	})
	t.Run("a failed reset ends the attempt", func(t *testing.T) {
		calls := 0
		_, err := retryOnBdProxyStartTimeout(
			func() (string, error) { calls++; return timeoutOut, errExit },
			func() error { return fmt.Errorf("remove .beads: busy") },
		)
		if err == nil || calls != 1 || !strings.Contains(err.Error(), "remove .beads: busy") {
			t.Fatalf("err = %v after %d call(s), want the reset error after 1 call", err, calls)
		}
	})
}

func (c *bdTopologyCity) sqlValue(t *testing.T, s bdTopologyScope, query string) string {
	t.Helper()
	out := c.bdIn(t, s, "sql", "--csv", query)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (c *bdTopologyCity) bdSupportsPurgeWispsPlane(t *testing.T) bool {
	t.Helper()
	out, _ := c.run(t, c.root, nil, c.bd, "purge", "--help")
	return strings.Contains(out, "--wisps-plane") && strings.Contains(out, "--limit")
}

// seedScope creates, in one scope: an old closed parent wisp with an old open
// child (Step 1 closes the child), an old stale durable task (Step 5 closes it
// in the city), an expired unassigned gc:nudge bead (Step 4 closes it in any
// scope) and an old closed standalone wisp (Step 3 purges it).
func (c *bdTopologyCity) seedScope(t *testing.T, s bdTopologyScope) map[string]string {
	t.Helper()
	ids := map[string]string{}
	create := func(key string, args ...string) {
		ids[key] = strings.TrimSpace(c.bdIn(t, s, append([]string{"create", "--silent"}, args...)...))
	}
	create("parent", "-t", "molecule", "stale parent", "--ephemeral")
	create("child", "-t", "task", "stale child", "--ephemeral", "--parent", ids["parent"])
	create("purgeable", "-t", "task", "closed standalone wisp", "--ephemeral")
	create("protected", "-t", "molecule", "closed parent of a live wisp", "--ephemeral")
	create("live", "-t", "task", "young live child", "--ephemeral", "--parent", ids["protected"])
	// Three levels: closed root -> stale mid -> stale leaf. bd refuses to close
	// mid while leaf is open, so the reaper must close leaf first.
	create("chainRoot", "-t", "molecule", "closed chain root", "--ephemeral")
	create("chainMid", "-t", "task", "stale chain mid", "--ephemeral", "--parent", ids["chainRoot"])
	create("chainLeaf", "-t", "task", "stale chain leaf", "--ephemeral", "--parent", ids["chainMid"])
	// Closed root -> stale mid -> YOUNG leaf: mid must stay open (never closed
	// over a live child) and nothing escalates.
	create("heldRoot", "-t", "molecule", "closed held root", "--ephemeral")
	create("heldMid", "-t", "task", "stale mid over a young leaf", "--ephemeral", "--parent", ids["heldRoot"])
	create("heldYoung", "-t", "task", "young leaf", "--ephemeral", "--parent", ids["heldMid"])
	create("stale", "-t", "task", "-p", "3", "stale durable task")
	create("nudge", "-t", "task", "expired nudge", "-l", "gc:nudge", "--metadata", `{"expires_at":"2020-01-01T00:00:00Z"}`)
	c.bdIn(t, s, "close", ids["parent"], ids["purgeable"], ids["protected"], ids["chainRoot"], ids["heldRoot"], "--force", "--reason", "seed")
	c.bdIn(t, s, "sql", fmt.Sprintf(
		"UPDATE wisps SET created_at = DATE_SUB(NOW(), INTERVAL 48 HOUR), updated_at = DATE_SUB(NOW(), INTERVAL 48 HOUR), closed_at = IF(status = 'closed', DATE_SUB(NOW(), INTERVAL 400 HOUR), closed_at) WHERE id IN ('%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s')",
		ids["parent"], ids["child"], ids["purgeable"], ids["protected"], ids["chainRoot"], ids["chainMid"], ids["chainLeaf"], ids["heldRoot"], ids["heldMid"]))
	c.bdIn(t, s, "sql", fmt.Sprintf("UPDATE issues SET updated_at = DATE_SUB(NOW(), INTERVAL 800 HOUR) WHERE id = '%s'", ids["stale"]))
	if !s.proxied {
		c.bdIn(t, s, "dolt", "commit", "-m", "seed maintenance fixture")
	}
	return ids
}

func (c *bdTopologyCity) status(t *testing.T, s bdTopologyScope, id string) string {
	t.Helper()
	return c.sqlValue(t, s, fmt.Sprintf(
		"SELECT COALESCE((SELECT status FROM wisps WHERE id = '%s'), (SELECT status FROM issues WHERE id = '%s'), 'gone')", id, id))
}

func (c *bdTopologyCity) runOrder(t *testing.T, script string, extra ...string) (string, string) {
	t.Helper()
	outcome := filepath.Join(c.root, "outcome-"+filepath.Base(script)+".json")
	_ = os.Remove(outcome)
	out, err := c.run(t, c.cityDir, append([]string{"GC_ORDER_OUTCOME_FILE=" + outcome}, extra...), script)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	data, _ := os.ReadFile(outcome)
	return out, string(data)
}

func TestMaintenanceOrdersOnRealBdTopologies(t *testing.T) {
	bd := requireRealBd(t)
	topologies := []struct {
		name     string
		cityProx bool
		rigProx  []bool
	}{
		{"proxied city and proxied rigs", true, []bool{true, true}},
		{"direct-server city and rig", false, []bool{false}},
		{"mixed: proxied city with a direct-server rig", true, []bool{false}},
	}
	for _, topo := range topologies {
		t.Run(topo.name, func(t *testing.T) {
			c := newBdTopologyCity(t, bd)
			city := c.addScope(t, "", "mcity", "mc", topo.cityProx)
			var rigs []bdTopologyScope
			for i, prox := range topo.rigProx {
				rigs = append(rigs, c.addScope(t, fmt.Sprintf("rig%d", i+1), fmt.Sprintf("mrig%d", i+1), fmt.Sprintf("r%d", i+1), prox))
			}
			seeds := map[string]map[string]string{"": c.seedScope(t, city)}
			for _, rig := range rigs {
				seeds[rig.name] = c.seedScope(t, rig)
			}

			// Reaper.
			out, outcome := c.runOrder(t, coreScriptPath("reaper.sh"))
			for _, s := range append([]bdTopologyScope{city}, rigs...) {
				ids := seeds[s.name]
				if got := c.status(t, s, ids["child"]); got != "closed" {
					t.Errorf("%s: stale child wisp %s = %s, want closed (Step 1)\n%s", s.dir, ids["child"], got, out)
				}
				for _, key := range []string{"chainMid", "chainLeaf"} {
					if got := c.status(t, s, ids[key]); got != "closed" {
						t.Errorf("%s: %s %s = %s, want closed (Step 1 closes an orphaned subtree leaf-first)", s.dir, key, ids[key], got)
					}
				}
				for _, key := range []string{"heldMid", "heldYoung"} {
					if got := c.status(t, s, ids[key]); got != "open" {
						t.Errorf("%s: %s %s = %s, want open (a stale wisp over a young child is never closed)", s.dir, key, ids[key], got)
					}
				}
				if got := c.status(t, s, ids["nudge"]); got != "closed" {
					t.Errorf("%s: expired nudge %s = %s, want closed (Step 4)", s.dir, ids["nudge"], got)
				}
				wantStale := "open"
				if s.name == "" {
					wantStale = "closed"
				}
				if got := c.status(t, s, ids["stale"]); got != wantStale {
					t.Errorf("%s: stale task %s = %s, want %s (Step 5 closes city issues only)", s.dir, ids["stale"], got, wantStale)
				}
				if c.bdSupportsPurgeWispsPlane(t) {
					if got := c.status(t, s, ids["purgeable"]); got != "gone" {
						t.Errorf("%s: old closed wisp %s = %s, want purged (Step 3)", s.dir, ids["purgeable"], got)
					}
					// bd purge keeps a closed wisp that a live wisp still
					// depends on (parent-child here).
					if got := c.status(t, s, ids["protected"]); got != "closed" {
						t.Errorf("%s: closed parent %s of a live child = %s, want kept (Step 3 live-dependent protection)", s.dir, ids["protected"], got)
					}
					if got := c.status(t, s, ids["live"]); got != "open" {
						t.Errorf("%s: young live child %s = %s, want open", s.dir, ids["live"], got)
					}
				}
				if !s.proxied {
					// Every mutation went through a bd verb that committed it.
					if dirty := c.sqlValue(t, s, "SELECT COUNT(*) FROM dolt_status WHERE table_name NOT LIKE 'wisp%'"); dirty != "0" {
						t.Errorf("%s: %s durable table(s) left uncommitted after the reaper", s.dir, dirty)
					}
				}
			}
			if !c.bdSupportsPurgeWispsPlane(t) && !strings.Contains(outcome, "bd-purge-unsupported") {
				t.Errorf("bd without wisps-plane purge must be declared, outcome = %s", outcome)
			}
			if gcLog, _ := os.ReadFile(c.gcLog); strings.Contains(string(gcLog), "ESCALATION") {
				for _, bad := range []string{"unreachable", "open child", "close failed"} {
					if strings.Contains(string(gcLog), bad) {
						t.Errorf("reaper escalated %q:\n%s", bad, gcLog)
					}
				}
			}

			// JSONL export: one bd-export snapshot per scope database, which
			// bd import can read back.
			archive := filepath.Join(c.root, "archive")
			if _, outcome := c.runOrder(t, coreScriptPath("jsonl-export.sh"), "GC_JSONL_ARCHIVE_REPO="+archive); strings.Contains(outcome, "unreachable") {
				t.Errorf("jsonl-export could not reach a scope: %s", outcome)
			}
			for _, s := range append([]bdTopologyScope{city}, rigs...) {
				snapshot := filepath.Join(archive, s.db, "issues.jsonl")
				data, err := os.ReadFile(snapshot)
				if err != nil {
					t.Fatalf("snapshot for %s: %v", s.db, err)
				}
				if !strings.Contains(string(data), `"`+seeds[s.name]["stale"]+`"`) {
					t.Errorf("%s snapshot is missing the durable task:\n%s", s.db, data)
				}
				if strings.Contains(string(data), seeds[s.name]["child"]) {
					t.Errorf("%s snapshot archived a wisp:\n%s", s.db, data)
				}
				// bd prints the dry-run verdict on stderr.
				if dry, err := c.run(t, s.dir, c.scopeEnv(s), bd, "import", "--dry-run", "--allow-stale", snapshot); err != nil || !strings.Contains(dry, "Would import") {
					t.Errorf("bd import cannot read the %s snapshot: %v\n%s", s.db, err, dry)
				}
			}

			// Backup: every scope bd can back up is synced; a scope whose
			// transport bd cannot back up yet is declared, not failed.
			doltPack := filepath.Join(repoRootForExamples(t), "examples", "bd", "dolt")
			backupOut, backupOutcome := c.runOrder(t, filepath.Join(doltPack, "assets", "scripts", "mol-dog-backup.sh"), "GC_PACK_DIR="+doltPack)
			for _, s := range append([]bdTopologyScope{city}, rigs...) {
				status, err := c.run(t, s.dir, c.scopeEnv(s), bd, "backup", "status", "--json")
				unsupported := err != nil && strings.Contains(status, "proxy.backup.unsupported")
				switch {
				case unsupported:
					if !strings.Contains(backupOutcome, "bd-backup-unsupported") {
						t.Errorf("%s: unsupported proxied backup not declared: %s\n%s", s.dir, backupOutcome, backupOut)
					}
				case err != nil:
					t.Errorf("%s: bd backup status failed: %v\n%s", s.dir, err, status)
				case !strings.Contains(status, `"last_sync"`):
					t.Errorf("%s: backup order did not sync the scope:\n%s\n%s", s.dir, status, backupOut)
				}
			}
		})
	}
}

func repoRootForExamples(t *testing.T) string {
	t.Helper()
	// coreScriptPath is <repo>/internal/bootstrap/packs/core/assets/scripts/<name>.
	return filepath.Clean(filepath.Join(filepath.Dir(coreScriptPath("reaper.sh")), "..", "..", "..", "..", "..", ".."))
}

// TestBackupOrderTakesOverALegacyBackupDestination covers a city backed up
// before the switch to `bd backup`: the old mol-dog-backup registered a
// Dolt backup named <db>-backup at <city>/.dolt-backup/<db>. The order must
// register bd's own destination at the same URL (bd replaces the conflicting
// legacy entry), sync to it, and leave bd reporting the sync, so backups
// continue into the same artifact directory.
func TestBackupOrderTakesOverALegacyBackupDestination(t *testing.T) {
	bd := requireRealBd(t)
	c := newBdTopologyCity(t, bd)
	city := c.addScope(t, "", "legacydb", "lg", false)
	c.bdIn(t, city, "create", "--silent", "-t", "task", "something to back up")
	c.bdIn(t, city, "dolt", "commit", "-m", "seed")

	// The order names the destination from the city's canonical path (`pwd -P`
	// in mol-dog-backup.sh), and Dolt matches backup URLs as strings. On macOS
	// t.TempDir() lives under /var, a symlink to /private/var, so the legacy
	// entry must be registered at the canonical path too or the order sees no
	// conflict to take over.
	cityDir, err := filepath.EvalSymlinks(c.cityDir)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(cityDir, ".dolt-backup", city.db)
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		t.Fatal(err)
	}
	url := "file://" + artifact
	c.bdIn(t, city, "sql", fmt.Sprintf("CALL DOLT_BACKUP('add', '%s-backup', '%s')", city.db, url))
	c.bdIn(t, city, "sql", fmt.Sprintf("CALL DOLT_BACKUP('sync', '%s-backup')", city.db))

	doltPack := filepath.Join(repoRootForExamples(t), "examples", "bd", "dolt")
	out, outcome := c.runOrder(t, filepath.Join(doltPack, "assets", "scripts", "mol-dog-backup.sh"), "GC_PACK_DIR="+doltPack)
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("backup order did not sync the legacy-backed scope:\n%s\noutcome: %s", out, outcome)
	}
	backups := c.bdIn(t, city, "sql", "--csv", "SELECT name, url FROM dolt_backups")
	if !strings.Contains(backups, "default,"+url) {
		t.Fatalf("bd's default destination is not at the legacy artifact URL:\n%s", backups)
	}
	if strings.Contains(backups, city.db+"-backup") {
		t.Fatalf("the legacy destination was not replaced by bd's:\n%s", backups)
	}
	status := c.bdIn(t, city, "backup", "status", "--json")
	if !strings.Contains(status, `"last_sync"`) || !strings.Contains(status, url) {
		t.Fatalf("bd backup status does not report the sync to the artifact dir:\n%s", status)
	}
}

// TestReaperOwnerProtectionOnRealBd pins Step 1's "never close over an open
// child" rule on real bd in the two cases the subtree selection alone cannot
// see: a child close bd rejects at run time, and a stale wisp held open by a
// descendant the reaper must not close (an open durable issue, a blocked
// wisp). The held wisps are reported on every run and never escalated.
func TestReaperOwnerProtectionOnRealBd(t *testing.T) {
	bd := requireRealBd(t)
	c := newBdTopologyCity(t, bd)
	city := c.addScope(t, "", "ownerdb", "ow", true)
	ids := map[string]string{}
	create := func(key string, args ...string) {
		ids[key] = strings.TrimSpace(c.bdIn(t, city, append([]string{"create", "--silent"}, args...)...))
	}
	// closed root -> assigned stale mid -> stale leaf whose close is rejected.
	create("root", "-t", "molecule", "closed root", "--ephemeral")
	create("mid", "-t", "task", "assigned stale mid", "--ephemeral", "--parent", ids["root"], "-a", "someone-else")
	create("leaf", "-t", "task", "stale leaf", "--ephemeral", "--parent", ids["mid"])
	// closed root -> stale wisp over a blocked wisp: held.
	create("heldRoot", "-t", "molecule", "closed held root", "--ephemeral")
	create("heldOverBlocked", "-t", "task", "stale wisp over a blocked wisp", "--ephemeral", "--parent", ids["heldRoot"])
	create("blocked", "-t", "task", "blocked wisp", "--ephemeral", "--parent", ids["heldOverBlocked"])
	c.bdIn(t, city, "update", ids["blocked"], "--status", "blocked")
	c.bdIn(t, city, "close", ids["root"], ids["heldRoot"], "--force", "--reason", "seed")
	c.bdIn(t, city, "sql", fmt.Sprintf(
		"UPDATE wisps SET created_at = DATE_SUB(NOW(), INTERVAL 48 HOUR), updated_at = DATE_SUB(NOW(), INTERVAL 48 HOUR) WHERE id IN ('%s', '%s', '%s', '%s', '%s', '%s')",
		ids["root"], ids["mid"], ids["leaf"], ids["heldRoot"], ids["heldOverBlocked"], ids["blocked"]))

	for run := 1; run <= 2; run++ {
		out, outcome := c.runOrder(t, coreScriptPath("reaper.sh"), "ROUTER_REJECT_CLOSE="+ids["leaf"])
		for _, key := range []string{"mid", "leaf", "heldOverBlocked"} {
			if got := c.status(t, city, ids[key]); got == "closed" || got == "gone" {
				t.Fatalf("run %d: %s %s = %s; a bead with an open child must stay open\n%s", run, key, ids[key], got, out)
			}
		}
		if !strings.Contains(out, "held_wisps:1") {
			t.Fatalf("run %d: summary does not report the held wisp:\n%s", run, out)
		}
		if !strings.Contains(outcome, "1 stale wisp(s) held open under a live descendant") {
			t.Fatalf("run %d: outcome does not name the held wisp: %s", run, outcome)
		}
	}
	calls, err := os.ReadFile(c.gcLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "close "+ids["mid"]+" ") {
		t.Fatalf("reaper tried to close the mid wisp over its open (rejected) leaf:\n%s", calls)
	}
	if strings.Contains(string(calls), "ESCALATION") && strings.Contains(string(calls), ids["heldOverBlocked"]) {
		t.Fatalf("a held wisp was escalated:\n%s", calls)
	}
}
