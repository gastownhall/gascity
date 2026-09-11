//go:build acceptance_a

// Proxied-local default acceptance test.
//
// This is the front-door proof for the beads v1.3.0-rc.2 proxied-local
// default: a fresh `gc init` with no transport selector must produce a store
// whose Dolt process belongs to bd (a `bd db-proxy-child` supervising a
// `dolt sql-server` under the scope's proxy root), and every ordinary command
// — doctor, bd, rig add, start, status, stop — must work against it and leave
// no process behind.
//
// It needs a real bd with proxied-server support and a real dolt, so it skips
// when either is missing. Everything else is the ordinary Tier A harness: the
// real gc binary, an isolated GC_HOME and XDG_RUNTIME_DIR, and the idle
// provider double so agents start without inference.
package acceptance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const proxiedTimingReportPath = "/data/tmp/gc-dolt-takeover/10-timing.md"

// proxiedBeadsMetadata is the subset of bd's .beads/metadata.json this test
// reads. bd writes the topology only here; config.yaml has no mode.
type proxiedBeadsMetadata struct {
	Backend      string `json:"backend"`
	DoltMode     string `json:"dolt_mode"`
	DoltDatabase string `json:"dolt_database"`
}

type proxiedSidecar struct {
	RootPath    string `json:"root_path"`
	IdleTimeout int    `json:"idle_timeout"`
}

type scopeOwnershipDoc struct {
	Version int `json:"version"`
	Scopes  map[string]struct {
		ScopePath      string `json:"scope_path"`
		LifecycleOwner string `json:"lifecycle_owner"`
		State          string `json:"state"`
		Intent         struct {
			Transport string `json:"transport"`
			Target    string `json:"target"`
		} `json:"intent"`
	} `json:"scopes"`
}

type doctorReport struct {
	Passed  int `json:"passed"`
	Warned  int `json:"warned"`
	Failed  int `json:"failed"`
	Results []struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"results"`
}

// requireProxiedTooling resolves the bd and dolt this test needs, or skips.
func requireProxiedTooling(t *testing.T) (string, string) {
	t.Helper()
	bdPath := helpers.FindBD()
	if bdPath == "" {
		t.Skip("bd not available; set GC_ACCEPTANCE_BD_BIN to a bd >= 1.3.0")
	}
	out, err := exec.Command(bdPath, "init", "--help").CombinedOutput() //nolint:gosec // resolved test binary
	if err != nil || !strings.Contains(string(out), "--proxied-server") {
		t.Skipf("bd at %s has no proxied-server support; set GC_ACCEPTANCE_BD_BIN to a bd >= 1.3.0", bdPath)
	}
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	return bdPath, doltPath
}

// proxiedEnv builds this test's own environment: the shared Tier A env with a
// real Dolt-backed bd store instead of the default file store, and the test's
// bd and dolt ahead of any host copies but behind the hermetic provider
// doubles, which must stay first.
func proxiedEnv(t *testing.T, bdPath, doltPath string) *helpers.Env {
	t.Helper()
	linkDir := filepath.Join(helpers.TempDir(t), "bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"bd": bdPath, "dolt": doltPath} {
		if err := os.Symlink(target, filepath.Join(linkDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	env := testEnv.Clone()
	entries := filepath.SplitList(env.Get("PATH"))
	path := append([]string{entries[0], linkDir}, entries[1:]...)
	return env.With("PATH", strings.Join(path, string(os.PathListSeparator))).
		With("GC_BEADS", "bd").
		Without("GC_DOLT")
}

// doltProcessesUnder returns the command lines of every live bd proxy or dolt
// sql-server whose argv names root. It reads the process table rather than a
// pid file because the leak worth catching is a process whose record is gone.
func doltProcessesUnder(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "args=").Output()
	if err != nil {
		t.Fatalf("read process table: %v", err)
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, root) {
			continue
		}
		if strings.Contains(line, "db-proxy-child") || strings.Contains(line, "sql-server") {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

func waitForNoDoltProcesses(t *testing.T, root string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for {
		last = doltProcessesUnder(t, root)
		if len(last) == 0 || time.Now().After(deadline) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func readJSONFile(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
}

// lastJSONLine decodes the final JSON document in out. gc's --json commands
// print one document on stdout, but config-load advisories can precede it.
func lastJSONLine(t *testing.T, out string, into any) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "[") {
			continue
		}
		if err := json.Unmarshal([]byte(line), into); err == nil {
			return
		}
	}
	// Fall back to the whole payload: gc pretty-prints some documents across
	// several lines.
	if err := json.Unmarshal([]byte(out), into); err != nil {
		t.Fatalf("no JSON document in output: %v\n%s", err, out)
	}
}

func assertProxiedScope(t *testing.T, scopeRoot, label string) {
	t.Helper()
	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	if !strings.EqualFold(metadata.Backend, "dolt") {
		t.Errorf("%s backend = %q, want dolt", label, metadata.Backend)
	}
	if !strings.EqualFold(metadata.DoltMode, "proxied-server") {
		t.Fatalf("%s dolt_mode = %q, want proxied-server", label, metadata.DoltMode)
	}

	var sidecar proxiedSidecar
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json"), &sidecar)
	if sidecar.IdleTimeout != -1 {
		t.Errorf("%s idle_timeout = %d, want -1 (bd's IdleTimeoutNever)", label, sidecar.IdleTimeout)
	}

	root := filepath.Join(scopeRoot, ".beads", "dolt")
	if sidecar.RootPath != "" {
		root = sidecar.RootPath
	}
	if _, err := os.Stat(filepath.Join(root, "proxy.pid")); err != nil {
		t.Errorf("%s has no live proxy record at %s: %v", label, root, err)
	}

	procs := doltProcessesUnder(t, root)
	var proxies, servers int
	for _, p := range procs {
		if strings.Contains(p, "db-proxy-child") {
			proxies++
		}
		if strings.Contains(p, "sql-server") {
			servers++
		}
	}
	if proxies != 1 || servers != 1 {
		t.Errorf("%s topology = %d proxy / %d sql-server, want 1 each:\n%s",
			label, proxies, servers, strings.Join(procs, "\n"))
	}
}

func TestBeadsProxiedDefault(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	env := proxiedEnv(t, bdPath, doltPath)

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	// Both rig workspaces are created here, in the parent: a subtest's
	// temporary directory is removed when that subtest ends, and a rig the city
	// still has registered has to outlive it or the next `gc start` cannot even
	// reach the scope.
	rigDir := createGitRig(t)
	// createGitRig always names its workspace "testrig", and a city cannot hold
	// two rigs under one name, so the adopted workspace gets its own.
	adoptedDir := filepath.Join(filepath.Dir(createGitRig(t)), "adopted-rig")
	if err := os.Rename(filepath.Join(filepath.Dir(adoptedDir), "testrig"), adoptedDir); err != nil {
		t.Fatal(err)
	}

	// Whatever the subtests do, nothing bd started for this city may outlive
	// the test. Registered before Init so it runs even if init itself leaves a
	// half-built scope behind.
	t.Cleanup(func() {
		helpers.RunGC(env, cityRoot, "stop", cityRoot)         //nolint:errcheck
		helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck
		for _, root := range []string{cityRoot, rigDir, adoptedDir} {
			if leaked := waitForNoDoltProcesses(t, root, 15*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})

	t.Run("init-default", func(t *testing.T) {
		city.Init("claude")

		assertProxiedScope(t, cityRoot, "city")

		var journal scopeOwnershipDoc
		readJSONFile(t, filepath.Join(cityRoot, ".gc", "scope-ownership.json"), &journal)
		entry, ok := journal.Scopes["city"]
		if !ok {
			t.Fatalf("ownership journal has no city scope: %+v", journal)
		}
		if entry.LifecycleOwner != "provider" || entry.State != "ready" {
			t.Errorf("city ownership = %+v, want provider/ready", entry)
		}
		if entry.Intent.Transport != "" || entry.Intent.Target != "" {
			t.Errorf("ready city ownership retains intent %+v", entry.Intent)
		}

		// bd owns the lifecycle, so gc's managed-Dolt runtime state must never
		// be written: a dolt-state.json here would mean two owners.
		if _, err := os.Stat(filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
			t.Error("gc wrote managed-Dolt runtime state for a bd-owned scope")
		}
	})

	t.Run("doctor-green", func(t *testing.T) {
		out, err := city.GC("doctor", "--json")
		var report doctorReport
		lastJSONLine(t, out, &report)
		if err != nil {
			t.Fatalf("gc doctor --json exited non-zero: %v\n%s", err, out)
		}
		if report.Failed != 0 {
			for _, r := range report.Results {
				if r.Status != "ok" {
					t.Logf("%s: %s — %s", r.Status, r.Name, r.Message)
				}
			}
			t.Fatalf("gc doctor reported %d failure(s) on a fresh proxied city", report.Failed)
		}
		for _, r := range report.Results {
			switch r.Name {
			case "beads-store", "dolt-server", "custom-types:city":
				if r.Status != "ok" {
					t.Errorf("%s = %s on a bd-owned proxied city: %s", r.Name, r.Status, r.Message)
				}
			}
		}
	})

	var createdBead string
	t.Run("bd-front-door", func(t *testing.T) {
		out, err := city.GCStdout("bd", "create", "e2e proxied default", "--json")
		if err != nil {
			t.Fatalf("gc bd create: %v\n%s", err, out)
		}
		var created struct {
			ID string `json:"id"`
		}
		lastJSONLine(t, out, &created)
		if strings.TrimSpace(created.ID) == "" {
			t.Fatalf("gc bd create returned no id:\n%s", out)
		}
		createdBead = created.ID

		list, err := city.GCStdout("bd", "list", "--json")
		if err != nil {
			t.Fatalf("gc bd list: %v\n%s", err, list)
		}
		if !strings.Contains(list, createdBead) {
			t.Fatalf("gc bd list does not contain %s:\n%s", createdBead, list)
		}
		show, err := city.GCStdout("bd", "show", createdBead, "--json")
		if err != nil {
			t.Fatalf("gc bd show %s: %v\n%s", createdBead, err, show)
		}
	})

	t.Run("rig-inherits", func(t *testing.T) {
		city.RigAdd(rigDir, "")
		assertProxiedScope(t, rigDir, "rig")

		var journal scopeOwnershipDoc
		readJSONFile(t, filepath.Join(cityRoot, ".gc", "scope-ownership.json"), &journal)
		key := "rig:" + filepath.Base(rigDir)
		entry, ok := journal.Scopes[key]
		if !ok {
			t.Fatalf("ownership journal has no %s: %+v", key, journal.Scopes)
		}
		if entry.State != "ready" {
			t.Errorf("%s state = %q, want ready", key, entry.State)
		}

		out, err := city.GCStdout("bd", "--rig", filepath.Base(rigDir), "create", "rig bead", "--json")
		if err != nil {
			t.Fatalf("gc bd --rig create: %v\n%s", err, out)
		}
	})

	t.Run("start-default-pack", func(t *testing.T) {
		city.StartWithSupervisor()

		status, err := city.GC("status")
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, status)
		}

		// The bd pack imports the dolt pack, whose orders fire on every city.
		// mol-dog-stale-db's front door is `gc dolt-cleanup --json --probe`;
		// on a bd-owned scope it has to be a typed no-op rather than a probe of
		// a managed server that does not exist. Driving the front door directly
		// is the same proof as waiting for the cron tick, without the wait.
		cleanup, err := city.GCStdout("dolt-cleanup", "--json", "--probe")
		if err != nil {
			t.Fatalf("gc dolt-cleanup --json --probe: %v\n%s", err, cleanup)
		}
		var report struct {
			Skipped *struct {
				Reason string `json:"reason"`
			} `json:"skipped"`
		}
		lastJSONLine(t, cleanup, &report)
		if report.Skipped == nil || report.Skipped.Reason != "bd-owned-proxied-scope" {
			t.Fatalf("dolt cleanup did not report the bd-owned no-op:\n%s", cleanup)
		}
		// What must never appear is gc's managed-Dolt runtime state: writing it
		// would mean a second owner for bd's process.
		if _, err := os.Stat(filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
			t.Error("a dolt order wrote managed-Dolt state on a bd-owned scope")
		}
	})

	t.Run("stop-quiescent", func(t *testing.T) {
		// Retire the readers before the store. On rc.2's proxied path any bd
		// read restarts the proxy (R2), so `bd dolt stop` has to be the last
		// thing that touches the scope — the order the design specifies:
		// agents, then the supervisor, then the provider's own processes.
		if out, err := helpers.RunGC(env, "", "supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop --wait: %v\n%s", err, out)
		}
		out, err := helpers.RunGC(env, cityRoot, "stop", cityRoot)
		if err != nil {
			t.Fatalf("gc stop: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, cityRoot, 10*time.Second); len(leaked) > 0 {
			t.Errorf("city proxy survived gc stop:\n%s", strings.Join(leaked, "\n"))
		}
		if leaked := waitForNoDoltProcesses(t, rigDir, 10*time.Second); len(leaked) > 0 {
			t.Errorf("rig proxy survived gc stop:\n%s", strings.Join(leaked, "\n"))
		}
		// Re-runnable: "there was nothing to stop" is success.
		if out, err := helpers.RunGC(env, cityRoot, "stop", cityRoot); err != nil {
			t.Fatalf("second gc stop: %v\n%s", err, out)
		}
	})

	t.Run("restart", func(t *testing.T) {
		city.StartWithSupervisor()
		assertProxiedScope(t, cityRoot, "restarted city")
		assertProxiedScope(t, rigDir, "restarted rig")

		// The store has to be the same one, not a fresh empty proxy: the bead
		// created before the stop must still be there.
		list, err := city.GCStdout("bd", "list", "--json")
		if err != nil {
			t.Fatalf("gc bd list after restart: %v\n%s", err, list)
		}
		if !strings.Contains(list, createdBead) {
			t.Fatalf("restarted city lost %s; the proxy came back over a different store:\n%s", createdBead, list)
		}
		if out, err := helpers.RunGC(env, cityRoot, "stop", cityRoot); err != nil {
			t.Fatalf("gc stop after restart: %v\n%s", err, out)
		}
	})

	t.Run("direct-escape-hatch", func(t *testing.T) {
		direct := helpers.NewCity(t, env)
		directRoot := direct.Dir
		t.Cleanup(func() {
			helpers.RunGC(env, directRoot, "stop", directRoot) //nolint:errcheck
			if leaked := waitForNoDoltProcesses(t, directRoot, 15*time.Second); len(leaked) > 0 {
				t.Errorf("direct city processes outlived the test:\n%s", strings.Join(leaked, "\n"))
			}
		})
		out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start",
			"--provider", "claude", "--beads-transport", "direct", "--beads-target", "local", directRoot)
		if err != nil {
			t.Fatalf("gc init --beads-transport direct: %v\n%s", err, out)
		}

		var metadata proxiedBeadsMetadata
		readJSONFile(t, filepath.Join(directRoot, ".beads", "metadata.json"), &metadata)
		if !strings.EqualFold(metadata.DoltMode, "server") {
			t.Fatalf("direct city dolt_mode = %q, want server", metadata.DoltMode)
		}
		for _, name := range []string{"dolt-server.pid", "dolt-server.port"} {
			if _, err := os.Stat(filepath.Join(directRoot, ".beads", name)); err != nil {
				t.Errorf("bd-owned direct city has no %s: %v", name, err)
			}
		}
		var journal scopeOwnershipDoc
		readJSONFile(t, filepath.Join(directRoot, ".gc", "scope-ownership.json"), &journal)
		if entry := journal.Scopes["city"]; entry.State != "ready" || entry.LifecycleOwner != "provider" {
			t.Errorf("direct city ownership = %+v, want provider/ready", entry)
		}
		if procs := doltProcessesUnder(t, directRoot); len(procs) != 1 {
			t.Errorf("direct city = %d dolt process(es), want 1:\n%s", len(procs), strings.Join(procs, "\n"))
		}

		if out, err := helpers.RunGC(env, directRoot, "stop", directRoot); err != nil {
			t.Fatalf("gc stop on the direct city: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, directRoot, 10*time.Second); len(leaked) > 0 {
			t.Errorf("direct city server survived gc stop:\n%s", strings.Join(leaked, "\n"))
		}
	})

	t.Run("adopt-un-journaled", func(t *testing.T) {
		// A workspace bd initialised on its own carries the proxied binding
		// with no gc journal entry — the migrated and cloned shapes R1 covers.
		adopted := adoptedDir
		initCmd := exec.Command(bdPath, "init", "--proxied-server", "--proxied-server-idle-timeout", "0", //nolint:gosec // resolved test binary
			"-p", "adopt", "--quiet", "--skip-hooks", "--skip-agents", "--non-interactive", adopted)
		initCmd.Dir = adopted
		initCmd.Env = env.List()
		if out, err := initCmd.CombinedOutput(); err != nil {
			t.Fatalf("bd init --proxied-server: %v\n%s", err, out)
		}
		// bd started this workspace's proxy; if the adoption below fails the
		// city never learns about the scope, so retire it here rather than
		// leave it to the city-wide stop.
		t.Cleanup(func() {
			stop := exec.Command(bdPath, "dolt", "stop") //nolint:gosec // resolved test binary
			stop.Dir = adopted
			stop.Env = env.List()
			stop.Run() //nolint:errcheck // best effort
		})

		// gc's adopt gate reads the issue prefix from .beads/config.yaml, and
		// `bd init` records its prefix in the store instead — its generated
		// config leaves issue-prefix commented out, and bd refuses
		// `bd config set issue_prefix` outright. So adopting a bd-initialised
		// workspace means writing that one line by hand today. Worth closing:
		// gc could read the prefix back from bd rather than require the file.
		configPath := filepath.Join(adopted, ".beads", "config.yaml")
		existing, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, append([]byte("issue_prefix: adopt\n"), existing...), 0o600); err != nil {
			t.Fatal(err)
		}

		// A workspace that already holds a beads store is an adoption, which gc
		// makes explicit rather than inferring.
		owned, addErr := helpers.RunGC(env, cityRoot, "rig", "add", "--adopt", "--prefix", "adopt", adopted)
		if addErr != nil {
			t.Fatalf("gc rig add --adopt on a bd-initialised proxied workspace: %v\n%s", addErr, owned)
		}
		assertProxiedScope(t, adopted, "adopted rig")

		// The clone shape: metadata says proxied-server, but bd's store is
		// gitignored and never came along. gc must refuse rather than let bd
		// create an empty one.
		clone := filepath.Join(helpers.TempDir(t), "cloned")
		if err := os.MkdirAll(filepath.Join(clone, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		metadata, err := os.ReadFile(filepath.Join(adopted, ".beads", "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clone, ".beads", "metadata.json"), metadata, 0o600); err != nil {
			t.Fatal(err)
		}
		// bd commits .beads/config.yaml alongside metadata.json, so a clone
		// carries both — and only the store is missing.
		if err := os.WriteFile(filepath.Join(clone, ".beads", "config.yaml"), []byte("issue_prefix: clonedrig\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, cloneErr := helpers.RunGC(env, cityRoot, "rig", "add", "--adopt", "--prefix", "clonedrig", clone)
		if cloneErr == nil {
			t.Fatalf("gc rig add accepted a proxied clone with no store:\n%s", out)
		}
		if !strings.Contains(out, "proxied-server") {
			t.Errorf("refusal does not name the proxied binding:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(clone, ".beads", "dolt")); statErr == nil {
			t.Error("the refused clone had a store created under it anyway")
		}
	})

	t.Run("legacy-unchanged", func(t *testing.T) {
		// Grandfathering: a scope whose persisted metadata says server mode
		// keeps the direct lifecycle whatever the fresh-init default is. The
		// direct city above is exactly that shape, written by bd itself.
		legacy := helpers.NewCity(t, env)
		legacyRoot := legacy.Dir
		t.Cleanup(func() {
			helpers.RunGC(env, legacyRoot, "stop", legacyRoot) //nolint:errcheck
			if leaked := waitForNoDoltProcesses(t, legacyRoot, 15*time.Second); len(leaked) > 0 {
				t.Errorf("legacy city processes outlived the test:\n%s", strings.Join(leaked, "\n"))
			}
		})
		out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start",
			"--provider", "claude", "--beads-transport", "direct", "--beads-target", "local", legacyRoot)
		if err != nil {
			t.Fatalf("gc init direct: %v\n%s", err, out)
		}
		// Re-running init must not reclassify the scope as proxied.
		if out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start",
			"--provider", "claude", legacyRoot); err != nil {
			t.Fatalf("re-init of an existing direct city: %v\n%s", err, out)
		}
		var metadata proxiedBeadsMetadata
		readJSONFile(t, filepath.Join(legacyRoot, ".beads", "metadata.json"), &metadata)
		if !strings.EqualFold(metadata.DoltMode, "server") {
			t.Fatalf("an existing direct city was reclassified to %q by the proxied default", metadata.DoltMode)
		}
		if procs := doltProcessesUnder(t, legacyRoot); len(procs) != 1 {
			t.Errorf("existing direct city = %d dolt process(es), want 1:\n%s", len(procs), strings.Join(procs, "\n"))
		}
	})

	t.Run("timing", func(t *testing.T) {
		// R6: informational only. D2 routes proxied scopes through the bd CLI
		// front door, which is a measured regression against a native store;
		// the numbers belong in the native-over-proxy follow-up, not in a
		// threshold nobody can tune. The city is stopped at this point, so the
		// first sample also pays bd's proxy cold start — which is the number
		// that matters for a controller tick after a quiet period.
		var samples []string
		for _, run := range []struct {
			label string
			args  []string
		}{
			{"gc status", []string{"status"}},
			{"gc bd list --json", []string{"bd", "list", "--json"}},
		} {
			start := time.Now()
			if _, err := helpers.RunGC(env, cityRoot, run.args...); err != nil {
				t.Logf("%s: %v", run.label, err)
			}
			samples = append(samples, fmt.Sprintf("| %s | proxied-local | %s |", run.label, time.Since(start).Round(time.Millisecond)))
		}
		for _, s := range samples {
			t.Log(s)
		}
		report := "# Slice 1 timing (R6, informational)\n\n" +
			"Measured on the Tier A acceptance harness against a bd v1.3.0-rc.2\n" +
			"proxied-local city. Every command goes through the bd CLI front door\n" +
			"(decision D2): there is no library open for a proxied workspace in rc.2.\n\n" +
			"| command | topology | wall clock |\n|---|---|---|\n" +
			strings.Join(samples, "\n") + "\n"
		if err := os.WriteFile(proxiedTimingReportPath, []byte(report), 0o644); err != nil {
			t.Logf("timing report not written to %s: %v", proxiedTimingReportPath, err)
		}
	})
}
