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
	Passed  int                 `json:"passed"`
	Warned  int                 `json:"warned"`
	Failed  int                 `json:"failed"`
	Results []doctorCheckResult `json:"results"`
}

type doctorCheckResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
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

// beadsTopologyCheck reports whether a doctor check's subject is the bead
// store's storage topology — the surface this feature owns.
//
// Everything else a Tier A city warns about (unset provider catalogs, the
// builtin packs' own formula requirements, agents with no sessions, the
// host-wide fork rate) is unrelated to which process holds the Dolt database
// and is the same before and after this change, so counting it would make the
// assertion a measure of the harness rather than of the feature.
func beadsTopologyCheck(name string) bool {
	name = strings.TrimPrefix(name, "rig:")
	if idx := strings.Index(name, ":"); idx >= 0 && strings.HasPrefix(name, "custom-types") {
		return true
	} else if idx >= 0 {
		name = name[idx+1:]
	}
	switch name {
	case "beads-store", "bead-store-preflight", "bd-split-store", "dolt-topology", "dolt-drift",
		"dolt-server", "dolt-backup", "dolt-local-only-remote", "beads":
		return true
	}
	return strings.HasPrefix(name, "custom-types")
}

// assertDoctorGreen runs the real `gc doctor --json` front door and requires
// exit 0, zero failures anywhere, and zero warnings from any check whose
// subject is the bead store's topology.
//
// Failures are absolute because R3 says a proxied city is a healthy city: the
// three timeouts this branch used to produce on a one-rig city
// (order-firing-current, v2-routed-to-namespace, pool-idle-routed-work) were
// not about Dolt at all, and a scoped assertion would have missed them.
//
// Topology warnings are absolute for the opposite reason: the ones this feature
// can produce are permanent. A rig's `dolt-backup` check cannot ever be
// satisfied on a bd-owned proxy root — no `gc dolt backup` invocation registers
// anything there — so it was a line every proxied city would carry forever, and
// a doctor report nobody can get to zero is a doctor report nobody reads.
func assertDoctorGreen(t *testing.T, city *helpers.City, label string) {
	t.Helper()
	out, err := city.GC("doctor", "--json")
	var report doctorReport
	lastJSONLine(t, out, &report)
	if err != nil {
		t.Fatalf("gc doctor --json exited non-zero on %s: %v\n%s", label, err, out)
	}
	topologyWarnings := 0
	for _, r := range report.Results {
		if r.Status == "ok" {
			continue
		}
		t.Logf("%s: %s — %s", r.Status, r.Name, r.Message)
		if r.Status == "warning" && beadsTopologyCheck(r.Name) {
			topologyWarnings++
		}
	}
	if report.Failed != 0 || topologyWarnings != 0 {
		t.Fatalf("gc doctor on %s reported %d failure(s) and %d bead-topology warning(s), want none",
			label, report.Failed, topologyWarnings)
	}
	for _, r := range report.Results {
		switch r.Name {
		case "beads-store", "dolt-server", "custom-types:city":
			if r.Status != "ok" {
				t.Errorf("%s = %s on %s: %s", r.Name, r.Status, label, r.Message)
			}
		}
	}
}

// assertProxiedBackupAdvisory pins the city-level `proxied-backup-coverage`
// line through the real `gc doctor --json` front door.
//
// It is the one place doctor says out loud that a bd-owned proxied scope has no
// backup at all — rc.2 refuses `bd backup` on that path, gc registers nothing
// against a proxy root it does not own, and the per-scope checks correctly go
// quiet, which between them made a default city read as covered. The advisory
// is deliberately StatusOK (R3: a proxied city is a healthy city, and a warning
// no operator can clear is a line nobody reads), so assertDoctorGreen cannot
// see it and a unit test cannot prove it is registered on a real city. Both
// directions are asserted: present with the scopes named for a proxied city,
// absent for a city with no proxied scope, so the registration gate is real
// rather than an unconditional line.
func assertProxiedBackupAdvisory(t *testing.T, city *helpers.City, label string, want bool, wantScopes ...string) {
	t.Helper()
	out, err := city.GC("doctor", "--json")
	if err != nil {
		t.Fatalf("gc doctor --json exited non-zero on %s: %v\n%s", label, err, out)
	}
	var report doctorReport
	lastJSONLine(t, out, &report)

	var found *doctorCheckResult
	for i := range report.Results {
		if report.Results[i].Name == "proxied-backup-coverage" {
			found = &report.Results[i]
			break
		}
	}
	if !want {
		if found != nil {
			t.Errorf("%s reported the proxied backup advisory: %s", label, found.Message)
		}
		return
	}
	if found == nil {
		t.Fatalf("%s has no proxied-backup-coverage advisory; doctor reported %d checks", label, len(report.Results))
	}
	if found.Status != "ok" {
		t.Errorf("proxied-backup-coverage = %s on %s, want ok (it must not gate a healthy city): %s",
			found.Status, label, found.Message)
	}
	for _, want := range append([]string{"no backup"}, wantScopes...) {
		if !strings.Contains(found.Message, want) {
			t.Errorf("proxied-backup-coverage on %s does not mention %q: %s", label, want, found.Message)
		}
	}
}

// makeCityLookLegacyManaged rewrites a freshly initialised proxied city into
// the on-disk shape a pre-proxied-default Gas City left behind: bd metadata in
// direct server mode, a canonical config whose endpoint origin is the managed
// city, and no ownership journal at all. bd's proxy and its store are retired
// and removed first, because the managed lifecycle roots its own Dolt data dir
// at the same place.
//
// It exists because no `gc init` on this branch can produce that shape — every
// fresh scope is journaled provider-owned — and the grandfathering claim is
// specifically about cities that predate the journal. Everything after this
// runs through the real front doors.
func makeCityLookLegacyManaged(t *testing.T, env *helpers.Env, bdPath, cityRoot string) {
	t.Helper()
	beadsDir := filepath.Join(cityRoot, ".beads")

	stop := exec.Command(bdPath, "dolt", "stop") //nolint:gosec // resolved test binary
	stop.Dir = cityRoot
	stop.Env = env.List()
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("bd dolt stop on the fixture city: %v\n%s", err, out)
	}
	if leaked := waitForNoDoltProcesses(t, cityRoot, 15*time.Second); len(leaked) > 0 {
		t.Fatalf("the fixture city's proxy survived bd dolt stop:\n%s", strings.Join(leaked, "\n"))
	}

	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(beadsDir, "metadata.json"), &metadata)

	for _, name := range []string{"dolt", "proxied_server_client_info.json"} {
		if err := os.RemoveAll(filepath.Join(beadsDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(cityRoot, ".gc", "scope-ownership.json")); err != nil {
		t.Fatal(err)
	}
	legacyMetadata := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q}`+"\n",
		metadata.DoltDatabase)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(legacyMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	// No issue_prefix: `bd init` leaves it commented out in its template (the
	// same fact the adopt subtest hand-writes around), and gc's own
	// canonicalisation supplies it on the next lifecycle command. What the
	// fixture has to state is the part gc reads as "this city's Dolt is mine":
	// the managed-city endpoint origin and direct server mode.
	legacyConfig := "gc.endpoint_origin: managed_city\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.mode: server\n" +
		"dolt.auto-start: false\n" +
		"export.auto: false\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
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
		assertDoctorGreen(t, city, "a fresh proxied city")
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

	t.Run("doctor-green-with-rig", func(t *testing.T) {
		// The zero-rig run above is the easy case. A rig adds its own proxied
		// scope, its own per-rig checks, and — before this was measured — a
		// per-bd-command readiness fan-out that multiplied every store read by
		// the number of provider-owned scopes until three checks died on their
		// timeouts. The one-rig topology is the default one an operator has.
		assertDoctorGreen(t, city, "a proxied city with a rig")
		// Green is not the same as covered. The city and its rig are both
		// bd-owned proxied scopes with no backup anywhere, and this is the only
		// doctor line that says so.
		assertProxiedBackupAdvisory(t, city, "a proxied city with a rig", true, "city", filepath.Base(rigDir))
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

	t.Run("bd-owned-direct-unchanged", func(t *testing.T) {
		// A scope whose persisted metadata says server mode keeps the direct
		// lifecycle whatever the fresh-init default is. This is the bd-owned
		// direct city the escape hatch produces; the GC-managed shape is the
		// subtest below.
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
			t.Fatalf("gc init direct: %v\n%s", err, out)
		}
		// Re-running init must not reclassify the scope as proxied.
		if out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start",
			"--provider", "claude", directRoot); err != nil {
			t.Fatalf("re-init of an existing direct city: %v\n%s", err, out)
		}
		var metadata proxiedBeadsMetadata
		readJSONFile(t, filepath.Join(directRoot, ".beads", "metadata.json"), &metadata)
		if !strings.EqualFold(metadata.DoltMode, "server") {
			t.Fatalf("an existing direct city was reclassified to %q by the proxied default", metadata.DoltMode)
		}
		if procs := doltProcessesUnder(t, directRoot); len(procs) != 1 {
			t.Errorf("existing direct city = %d dolt process(es), want 1:\n%s", len(procs), strings.Join(procs, "\n"))
		}
	})

	t.Run("legacy-managed-city-unchanged", func(t *testing.T) {
		// The grandfathering claim this branch has to keep is about the cities
		// that exist today: GC-managed direct servers — metadata dolt_mode
		// server, canonical config gc.endpoint_origin managed_city, no
		// scope-ownership journal, gc's own sql-server under
		// .gc/runtime/packs/dolt. No gc on this branch can create one, because
		// `gc init` journals every fresh scope as provider-owned, so the
		// fixture is built by writing the pre-PR on-disk shape and then driving
		// the real front doors over it.
		legacy := helpers.NewCity(t, env)
		legacyRoot := legacy.Dir
		legacyRig := filepath.Join(filepath.Dir(createGitRig(t)), "legacy-rig")
		if err := os.Rename(filepath.Join(filepath.Dir(legacyRig), "testrig"), legacyRig); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			helpers.RunGC(env, legacyRoot, "stop", legacyRoot)     //nolint:errcheck
			helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck
			for _, root := range []string{legacyRoot, legacyRig} {
				if leaked := waitForNoDoltProcesses(t, root, 15*time.Second); len(leaked) > 0 {
					t.Errorf("legacy city processes outlived the test under %s:\n%s", root, strings.Join(leaked, "\n"))
				}
			}
		})

		out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start",
			"--provider", "claude", legacyRoot)
		if err != nil {
			t.Fatalf("gc init: %v\n%s", err, out)
		}
		makeCityLookLegacyManaged(t, env, bdPath, legacyRoot)

		legacy.StartWithSupervisor()

		// gc, not bd, owns the Dolt process: its runtime state is written and
		// the sql-server is the one gc launched from its own pack state dir.
		doltState := filepath.Join(legacyRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
		if _, err := os.Stat(doltState); err != nil {
			t.Fatalf("gc did not write managed-Dolt runtime state for a grandfathered city: %v", err)
		}
		procs := doltProcessesUnder(t, legacyRoot)
		var managed, proxies int
		for _, p := range procs {
			if strings.Contains(p, "db-proxy-child") {
				proxies++
			}
			if strings.Contains(p, "sql-server") && strings.Contains(p, filepath.Join(".gc", "runtime", "packs", "dolt")) {
				managed++
			}
		}
		if proxies != 0 {
			t.Errorf("a grandfathered city got a bd proxy:\n%s", strings.Join(procs, "\n"))
		}
		if managed != 1 {
			t.Errorf("gc-managed sql-server count = %d, want 1:\n%s", managed, strings.Join(procs, "\n"))
		}

		// A rig added to it joins that one server rather than acquiring a
		// lifecycle owner of its own.
		if out, err := helpers.RunGC(env, legacyRoot, "rig", "add", legacyRig); err != nil {
			t.Fatalf("gc rig add on a grandfathered city: %v\n%s", err, out)
		}
		var journal scopeOwnershipDoc
		if data, err := os.ReadFile(filepath.Join(legacyRoot, ".gc", "scope-ownership.json")); err == nil {
			if err := json.Unmarshal(data, &journal); err != nil {
				t.Fatalf("parse ownership journal: %v\n%s", err, data)
			}
			for key := range journal.Scopes {
				t.Errorf("a grandfathered city journaled %q as provider-owned", key)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		rigConfig, err := os.ReadFile(filepath.Join(legacyRig, ".beads", "config.yaml"))
		if err != nil {
			t.Fatalf("read rig canonical config: %v", err)
		}
		if !strings.Contains(string(rigConfig), "inherited_city") {
			t.Errorf("rig did not inherit the city endpoint:\n%s", rigConfig)
		}
		if leaked := doltProcessesUnder(t, legacyRig); len(leaked) != 0 {
			t.Errorf("the rig got a Dolt process of its own:\n%s", strings.Join(leaked, "\n"))
		}

		// The proxied backup advisory is gated on the city actually having a
		// proxied scope, and a grandfathered city has none: gc registers its
		// backups the ordinary way here, so the line would be false. This is
		// the negative half of the gate — without it, an unconditional advisory
		// would pass the positive assertion just as well.
		assertProxiedBackupAdvisory(t, legacy, "a grandfathered managed city", false)

		// And gc stop takes the server it started back down.
		if out, err := helpers.RunGC(env, "", "supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop --wait: %v\n%s", err, out)
		}
		if out, err := helpers.RunGC(env, legacyRoot, "stop", legacyRoot); err != nil {
			t.Fatalf("gc stop on a grandfathered city: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, legacyRoot, 15*time.Second); len(leaked) > 0 {
			t.Errorf("the gc-managed server survived gc stop:\n%s", strings.Join(leaked, "\n"))
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
