package acceptancehelpers

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/schema"
)

// These tests are the guard for the 2026-09-29 incident, in which an acceptance
// run's `bd init --server` inherited the operator's HOME, read
// `dolt.shared-server: true` from ~/.beads/config.yaml, and started the
// operator's own shared Dolt server from the run's temp dolt binary.
//
// Each one stands the test process up the way that host was — HOME (and the XDG
// directories, and bd's own env switches) pointing at a canary home whose bd
// config says shared-server: true — then drives a harness path that forks bd,
// and requires that nothing the child saw names the canary and that the canary
// is byte-for-byte untouched afterwards.

// hostCanary makes the test process look like a developer box with bd's
// shared-server mode on, and returns the canary home plus a check that it was
// left exactly as found.
func hostCanary(t *testing.T) (string, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX-only")
	}
	canary := t.TempDir()
	shared := []byte("dolt:\n  shared-server: true\n")
	for _, p := range []string{
		filepath.Join(canary, ".beads", "config.yaml"),
		filepath.Join(canary, ".config", "bd", "config.yaml"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, shared, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"HOME":                     canary,
		"XDG_CONFIG_HOME":          filepath.Join(canary, ".config"),
		"XDG_DATA_HOME":            filepath.Join(canary, ".local", "share"),
		"XDG_CACHE_HOME":           filepath.Join(canary, ".cache"),
		"XDG_STATE_HOME":           filepath.Join(canary, ".local", "state"),
		"BEADS_DOLT_SHARED_SERVER": "1",
		"BD_DOLT_SHARED_SERVER":    "true",
		"BEADS_DIR":                filepath.Join(canary, ".beads"),
		"BEADS_SHARED_SERVER_DIR":  filepath.Join(canary, ".beads", "shared-server"),
	} {
		t.Setenv(k, v)
	}
	before := snapshotTree(t, canary)
	return canary, func() {
		t.Helper()
		if after := snapshotTree(t, canary); after != before {
			t.Errorf("the host home was modified:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}
}

// snapshotTree renders every entry under root with its mode, size, mtime and
// content hash, so any write, create or delete changes the result.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		line := fmt.Sprintf("%s %v %d %d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// envRecordingBD is a bd stand-in that saves its whole environment, one file
// per invocation, and does just enough of each verb for the harness path under
// test to succeed.
func envRecordingBD(t *testing.T) (bdPath, logDir string) {
	t.Helper()
	dir := t.TempDir()
	logDir = filepath.Join(dir, "envs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdPath = filepath.Join(dir, "bd")
	script := fmt.Sprintf(`#!/bin/sh
env > %s/$$.env
case "$1" in
migrate) echo 'Schema already at v%d' ;;
init) mkdir -p "$BEADS_DIR" && echo '{"project_id":"canary-test"}' > "$BEADS_DIR/metadata.json" ;;
esac
exit 0
`, shellQuote(logDir), schema.LatestVersion())
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return bdPath, logDir
}

// recordedEnvs reads every environment envRecordingBD saved.
func recordedEnvs(t *testing.T, logDir string) []map[string]string {
	t.Helper()
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(logDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		vars := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				vars[k] = v
			}
		}
		out = append(out, vars)
	}
	if len(out) == 0 {
		t.Fatal("bd was never invoked; the guard proves nothing")
	}
	return out
}

// assertBdSawNoHostHome fails for any bd environment that names the canary or
// could put bd in shared-server mode.
func assertBdSawNoHostHome(t *testing.T, canary string, envs []map[string]string) {
	t.Helper()
	for i, vars := range envs {
		for k, v := range vars {
			if strings.Contains(v, canary) {
				t.Errorf("bd invocation %d: %s=%s names the host home %s", i, k, v, canary)
			}
		}
		if vars["HOME"] == "" {
			t.Errorf("bd invocation %d: no HOME at all (bd falls back to the passwd home)", i)
		}
		if got := vars[bdSharedServerConfigEnv]; got != "false" {
			t.Errorf("bd invocation %d: %s=%q, want \"false\"", i, bdSharedServerConfigEnv, got)
		}
		if got, ok := vars["BEADS_DOLT_SHARED_SERVER"]; ok {
			t.Errorf("bd invocation %d: host BEADS_DOLT_SHARED_SERVER=%q leaked through", i, got)
		}
	}
}

func newCanaryEnv(t *testing.T) *Env {
	t.Helper()
	root := t.TempDir()
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, d := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return NewEnv("", gcHome, runtimeDir)
}

// The TestMain schema probe runs before any Env exists, on the test process's
// own environment.
func TestBdSchemaProbeNeverSeesTheHostHome(t *testing.T) {
	canary, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	if err := RequireBdSchemaParity(bd); err != nil {
		t.Fatal(err)
	}
	assertBdSawNoHostHome(t, canary, recordedEnvs(t, logDir))
	untouched()
}

// ProvisionBeadsDatabase is the exact call that started the operator's shared
// server.
func TestProvisionBeadsDatabaseNeverSeesTheHostHome(t *testing.T) {
	canary, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	env := newCanaryEnv(t)
	if env.Get("HOME") != canary {
		t.Fatalf("Env HOME = %q, want the (canary) host HOME gc needs", env.Get("HOME"))
	}
	up := &ExternalDolt{Host: "127.0.0.1", Port: "1", Database: "canary_db"}
	up.ProvisionBeadsDatabase(t, env, bd, filepath.Join(t.TempDir(), "provision"), "hosted")

	envs := recordedEnvs(t, logDir)
	assertBdSawNoHostHome(t, canary, envs)
	for _, vars := range envs {
		if !strings.HasSuffix(vars["BEADS_DIR"], filepath.Join("provision", ".beads")) {
			t.Errorf("BEADS_DIR = %q, want the provisioning workspace's", vars["BEADS_DIR"])
		}
	}
	untouched()
}

// gc forks bd with the Env's real HOME. The bd it finds on a topology's PATH
// must re-home itself.
func TestTopologyPathBdNeverSeesTheHostHome(t *testing.T) {
	canary, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	env := TopologyEnv(t, newCanaryEnv(t), t.TempDir(), bd, "/bin/true")

	found := findInPath(env.Get("PATH"), "bd")
	if found == "" {
		t.Fatal("no bd on the topology PATH")
	}
	cmd := exec.Command(found, "list") //nolint:gosec // resolved test wrapper
	cmd.Env = env.List()
	if env.Get("HOME") != canary {
		t.Fatalf("the gc-side env should keep the (canary) host HOME, got %q", env.Get("HOME"))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd via PATH: %v\n%s", err, out)
	}
	assertBdSawNoHostHome(t, canary, recordedEnvs(t, logDir))
	untouched()
}

// Every Env stages the configured bd behind the wrapper ahead of the host PATH,
// so a gc that resolves bd itself (a Tier A run with
// GC_ACCEPTANCE_BEADS_PROVIDER=bd, Tier C, worker inference) re-homes it too.
func TestEnvPathBdNeverSeesTheHostHome(t *testing.T) {
	canary, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	t.Setenv("GC_ACCEPTANCE_BD_BIN", bd)
	env := newCanaryEnv(t)

	found := findInPath(env.Get("PATH"), "bd")
	if found == "" || found == bd {
		t.Fatalf("bd on the Env PATH = %q, want the tool-home wrapper around %s", found, bd)
	}
	cmd := exec.Command(found, "list") //nolint:gosec // resolved test wrapper
	cmd.Env = env.List()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd via PATH: %v\n%s", err, out)
	}
	assertBdSawNoHostHome(t, canary, recordedEnvs(t, logDir))
	untouched()
}

// A test that seeds a user-level bd layer on purpose (F9) keeps it: an explicit
// XDG_CONFIG_HOME survives, and BD_DOLT_SHARED_SERVER="" is left for bd to read
// as unset. HOME still never reaches bd.
func TestToolHomeHonoursExplicitUserLevelLayers(t *testing.T) {
	_, untouched := hostCanary(t)
	bd, logDir := envRecordingBD(t)
	seeded := filepath.Join(t.TempDir(), "xdg-config")
	env := TopologyEnv(t, newCanaryEnv(t), t.TempDir(), bd, "/bin/true").
		With("XDG_CONFIG_HOME", seeded).
		With(bdSharedServerConfigEnv, "")

	direct := exec.Command(bd, "list") //nolint:gosec // test stub
	direct.Env = env.ToolList()
	viaPath := exec.Command(findInPath(env.Get("PATH"), "bd"), "list") //nolint:gosec // resolved test wrapper
	viaPath.Env = env.List()
	for _, cmd := range []*exec.Cmd{direct, viaPath} {
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd.Path, err, out)
		}
	}
	for i, vars := range recordedEnvs(t, logDir) {
		if vars["XDG_CONFIG_HOME"] != seeded {
			t.Errorf("invocation %d: XDG_CONFIG_HOME = %q, want the seeded %q", i, vars["XDG_CONFIG_HOME"], seeded)
		}
		if v, ok := vars[bdSharedServerConfigEnv]; !ok || v != "" {
			t.Errorf("invocation %d: %s = %q (set=%v), want explicitly empty", i, bdSharedServerConfigEnv, v, ok)
		}
		if vars["HOME"] != env.ToolHome() {
			t.Errorf("invocation %d: HOME = %q, want the tool home %q", i, vars["HOME"], env.ToolHome())
		}
	}
	untouched()
}

// The end-to-end form of the incident, against a real bd: `bd init --server`
// under a shared-server host config starts the host-wide Dolt server with
// whatever dolt is on PATH. Here that dolt is a recorder that refuses to run,
// so the test can see what bd tried without ever starting a server.
func TestRealBdProvisionLeavesTheHostHomeAlone(t *testing.T) {
	realBD := FindBD()
	if realBD == "" {
		t.Skip("no bd available (set GC_ACCEPTANCE_BD_BIN)")
	}
	canary, untouched := hostCanary(t)

	doltDir := t.TempDir()
	doltLog := filepath.Join(doltDir, "calls.log")
	doltStub := fmt.Sprintf("#!/bin/sh\necho \"HOME=$HOME $*\" >> %s\nexit 1\n", shellQuote(doltLog))
	if err := os.WriteFile(filepath.Join(doltDir, "dolt"), []byte(doltStub), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	env := newCanaryEnv(t)
	env.With("PATH", doltDir+string(os.PathListSeparator)+env.Get("PATH"))

	workspace := filepath.Join(t.TempDir(), "provision")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	up := &ExternalDolt{Host: "127.0.0.1", Port: "1", Database: "canary_db"}
	cmd := up.provisionCommand(env, realBD, workspace, "hosted")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run() // nothing listens on port 1; only what bd touched on the way matters

	if calls, err := os.ReadFile(doltLog); err == nil && strings.Contains(string(calls), canary) {
		t.Errorf("bd ran dolt against the host home:\n%s\nbd output:\n%s", calls, out.String())
	}
	if _, err := os.Stat(filepath.Join(canary, ".beads", "shared-server")); err == nil {
		t.Errorf("bd created the host's shared-server root\nbd output:\n%s", out.String())
	}
	untouched()
}
