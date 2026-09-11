//go:build acceptance_a

// AC-X: a city initialised the OLD way, migrated to bd's proxied-server
// topology on beads v1.3.0-rc.2.
//
// The shape under test is the one real operators have: a gc-owned
// `dolt sql-server` over `<city>/.beads/dolt`, metadata dolt_mode=server,
// gc.endpoint_origin=managed_city, no ownership journal, and a rig whose
// database lives inside the city's multi-database data dir. `gc stop` retires
// that server, `gc beads city migrate-proxied` hands both scopes to bd, and
// everything afterwards — doctor, bd reads, start, stop, restart — must work
// against the bd-owned proxy with the issues, dependencies and project
// identity intact.
//
// Two real binaries are required and the test skips typed without either:
//   - GC_ACCEPTANCE_BD_BIN — a bd >= 1.3.0-rc.2 with proxied-server support
//     (plus a real dolt on PATH), same as TestBeadsProxiedDefault.
//   - GC_ACCEPTANCE_LEGACY_GC_BIN — a gc built from a revision that still
//     initialises the legacy GC-managed direct topology. The migration cannot
//     be proved against a fixture the current binary writes: the point is that
//     a city gc wrote BEFORE this feature existed still comes up.
//
// Locally that second one is a gc built from main at the rc.2 bump, e.g.
//
//	go build -o /tmp/gc-main ./cmd/gc   # from a clean checkout of main
//	GC_ACCEPTANCE_BD_BIN=/data/tmp/bd-rc2/bd \
//	GC_ACCEPTANCE_LEGACY_GC_BIN=/tmp/gc-main \
//	  go test -tags acceptance_a -run TestBeadsMigrate ./test/acceptance
//
// This is the interim rc.2 path; the journaled ownership handoff (beads #6281)
// supersedes it. See engdocs/runbooks/beads-migrate-proxied.md.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// requireLegacyGCBinary resolves the old-way gc, or skips.
func requireLegacyGCBinary(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_LEGACY_GC_BIN"))
	if raw == "" {
		t.Skip("no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology")
	}
	bin, err := filepath.Abs(raw)
	if err != nil {
		t.Fatalf("resolving GC_ACCEPTANCE_LEGACY_GC_BIN: %v", err)
	}
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		t.Fatalf("GC_ACCEPTANCE_LEGACY_GC_BIN %s is not an executable file: %v", bin, err)
	}
	return bin
}

// legacyGCEnv points every gc invocation — the direct one and any gc re-exec a
// provider script performs — at the old binary.
func legacyGCEnv(t *testing.T, env *helpers.Env, legacyGC string) *helpers.Env {
	t.Helper()
	linkDir := filepath.Join(helpers.TempDir(t), "legacy-bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "gc")
	if err := os.Symlink(legacyGC, link); err != nil {
		t.Fatal(err)
	}
	entries := filepath.SplitList(env.Get("PATH"))
	// Index 0 stays the hermetic provider doubles; the legacy gc goes directly
	// behind them, ahead of the binary under test.
	path := append([]string{entries[0], linkDir}, entries[1:]...)
	return env.Clone().
		With("PATH", strings.Join(path, string(os.PathListSeparator))).
		With("GC_ACCEPTANCE_GC_BIN", legacyGC)
}

// assertLegacyManagedScope proves the fixture really is the old way before the
// migration touches it. Without this the test could pass against a city the
// new binary already initialised as proxied.
func assertLegacyManagedScope(t *testing.T, scopeRoot, label string) {
	t.Helper()
	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	if !strings.EqualFold(metadata.DoltMode, "server") {
		t.Fatalf("%s dolt_mode = %q, want server — GC_ACCEPTANCE_LEGACY_GC_BIN is not an old-way gc", label, metadata.DoltMode)
	}
	config := readScopeConfig(t, scopeRoot)
	if !strings.Contains(config, "gc.endpoint_origin:") {
		t.Fatalf("%s config.yaml records no gc endpoint origin:\n%s", label, config)
	}
	if _, err := os.Stat(filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json")); err == nil {
		t.Fatalf("%s already has a bd proxy sidecar; the fixture is not legacy", label)
	}
}

func readScopeConfig(t *testing.T, scopeRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read %s: %v", scopeRoot, err)
	}
	return string(data)
}

type migrateProxiedScopeReport struct {
	Scope       string `json:"scope"`
	Path        string `json:"path"`
	Status      string `json:"status"`
	DoltMode    string `json:"dolt_mode"`
	DoltDataDir string `json:"dolt_data_dir"`
	Detail      string `json:"detail"`
	Error       string `json:"error"`
}

type migrateProxiedCityReport struct {
	City   string                      `json:"city"`
	DryRun bool                        `json:"dry_run"`
	Scopes []migrateProxiedScopeReport `json:"scopes"`
	Failed int                         `json:"failed"`
}

func TestBeadsMigrateLegacyCityToProxied(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	legacyGC := requireLegacyGCBinary(t)

	newEnv := proxiedEnv(t, bdPath, doltPath)
	oldEnv := legacyGCEnv(t, newEnv, legacyGC)

	rigDir := createGitRig(t)
	city := helpers.NewCity(t, oldEnv)
	cityRoot := city.Dir

	// Registered before init so a failure anywhere below still retires every
	// process this test can start — the legacy sql-server and the proxies.
	t.Cleanup(func() {
		_, _ = helpers.RunGC(newEnv, cityRoot, "stop", cityRoot)
		_, _ = helpers.RunGC(newEnv, cityRoot, "supervisor", "stop", "--wait")
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Errorf("processes survived cleanup under %s:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})

	// --- the old way -----------------------------------------------------
	city.InitNoStart("claude")
	city.RigAdd(rigDir, "")
	assertLegacyManagedScope(t, cityRoot, "legacy city")
	assertLegacyManagedScope(t, rigDir, "legacy rig")

	cityIDs := seedLegacyScope(t, city, "")
	rigIDs := seedLegacyScope(t, city, "testrig")

	cityProjectID := readScopeProjectID(t, cityRoot)
	rigProjectID := readScopeProjectID(t, rigDir)
	cityDatabase := readScopeDoltDatabase(t, cityRoot)
	rigDatabase := readScopeDoltDatabase(t, rigDir)

	// gc stop is mandatory and is the legacy binary's job: it owns the server.
	if out, err := helpers.RunGC(oldEnv, cityRoot, "stop", cityRoot); err != nil {
		t.Fatalf("legacy gc stop: %v\n%s", err, out)
	}

	// --- the migration ---------------------------------------------------
	city.Env = newEnv

	t.Run("dry-run-changes-nothing", func(t *testing.T) {
		before := readScopeConfig(t, cityRoot)
		out, err := city.GCStdout("beads", "city", "migrate-proxied", "--json", "--dry-run")
		if err != nil {
			t.Fatalf("migrate-proxied --dry-run: %v\n%s", err, out)
		}
		var report migrateProxiedCityReport
		lastJSONLine(t, out, &report)
		if !report.DryRun || report.Failed != 0 || len(report.Scopes) != 2 {
			t.Fatalf("dry-run report = %+v", report)
		}
		if got := readScopeConfig(t, cityRoot); got != before {
			t.Fatalf("dry run rewrote the city config:\n%s", got)
		}
	})

	var report migrateProxiedCityReport
	t.Run("migrate", func(t *testing.T) {
		out, err := city.GCStdout("beads", "city", "migrate-proxied", "--json")
		if err != nil {
			t.Fatalf("gc beads city migrate-proxied: %v\n%s", err, out)
		}
		lastJSONLine(t, out, &report)
		if report.Failed != 0 || len(report.Scopes) != 2 {
			t.Fatalf("migrate report = %+v", report)
		}
		for _, scope := range report.Scopes {
			if scope.Status != "migrated" {
				t.Errorf("%s status = %q (%s)", scope.Scope, scope.Status, scope.Error)
			}
			if scope.DoltMode != "proxied-server" {
				t.Errorf("%s dolt_mode = %q", scope.Scope, scope.DoltMode)
			}
		}
	})

	t.Run("topology", func(t *testing.T) {
		assertProxiedScope(t, cityRoot, "migrated city")
		assertProxiedScope(t, rigDir, "migrated rig")

		// The rig shares the city's proxy root, so its metadata has to name it
		// — and relatively, because beads drops an absolute dolt_data_dir on
		// save and bd saves the config mid-migration.
		var rigMetadata struct {
			DoltDataDir string `json:"dolt_data_dir"`
		}
		readJSONFile(t, filepath.Join(rigDir, ".beads", "metadata.json"), &rigMetadata)
		if rigMetadata.DoltDataDir == "" || filepath.IsAbs(rigMetadata.DoltDataDir) {
			t.Fatalf("rig dolt_data_dir = %q, want a relative path to the city's data dir", rigMetadata.DoltDataDir)
		}
		resolved := filepath.Join(rigDir, ".beads", rigMetadata.DoltDataDir)
		want := filepath.Join(cityRoot, ".beads", "dolt")
		if filepath.Clean(resolved) != filepath.Clean(want) {
			t.Fatalf("rig dolt_data_dir resolves to %s, want %s", resolved, want)
		}

		for label, root := range map[string]string{"city": cityRoot, "rig": rigDir} {
			if config := readScopeConfig(t, root); strings.Contains(config, "dolt.mode") {
				t.Errorf("%s config.yaml still carries dolt.mode:\n%s", label, config)
			}
		}
		if got := readScopeProjectID(t, cityRoot); got != cityProjectID {
			t.Errorf("city project_id = %q, want the pre-migration %q", got, cityProjectID)
		}
		if got := readScopeProjectID(t, rigDir); got != rigProjectID {
			t.Errorf("rig project_id = %q, want the pre-migration %q", got, rigProjectID)
		}
		if got := readScopeDoltDatabase(t, cityRoot); got != cityDatabase {
			t.Errorf("city dolt_database = %q, want %q", got, cityDatabase)
		}
		if got := readScopeDoltDatabase(t, rigDir); got != rigDatabase {
			t.Errorf("rig dolt_database = %q, want %q", got, rigDatabase)
		}
	})

	t.Run("doctor-green", func(t *testing.T) {
		// The legacy city's types.custom predates a required bead type, which
		// is an old-init-on-new-gc gap rather than a migration one. The runbook
		// tells operators to run this too.
		if out, err := city.GC("doctor", "--fix"); err != nil {
			t.Logf("gc doctor --fix reported work outstanding: %v\n%s", err, out)
		}
		assertDoctorGreen(t, city, "a migrated legacy city")
	})

	t.Run("data-survived", func(t *testing.T) {
		assertSeededScope(t, city, "", cityIDs)
		assertSeededScope(t, city, "testrig", rigIDs)
	})

	t.Run("start-default-pack", func(t *testing.T) {
		city.StartWithSupervisor()
		if out, err := city.GC("status"); err != nil {
			t.Fatalf("gc status after migration: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")); err == nil {
			t.Error("gc start republished managed Dolt runtime state for a bd-owned city")
		} else if !os.IsNotExist(err) {
			t.Fatalf("probe managed dolt state: %v", err)
		}
		for _, proc := range doltProcessesUnder(t, cityRoot) {
			if strings.Contains(proc, filepath.Join(".gc", "runtime", "packs", "dolt")) {
				t.Errorf("a gc-managed Dolt server is running after migration:\n%s", proc)
			}
		}
		// One shared proxy root, one proxy, one Dolt child — the old topology's
		// process count, preserved.
		assertProxiedScope(t, cityRoot, "started city")
		assertProxiedScope(t, rigDir, "started rig")
	})

	t.Run("stop-and-restart", func(t *testing.T) {
		if out, err := city.GC("supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop: %v\n%s", err, out)
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop: %v\n%s", err, out)
		}
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Fatalf("gc stop left processes under %s:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop is not re-runnable: %v\n%s", err, out)
		}
		city.StartWithSupervisor()
		assertProxiedScope(t, cityRoot, "restarted city")
		assertSeededScope(t, city, "", cityIDs)
	})

	t.Run("rerun-is-idempotent", func(t *testing.T) {
		if out, err := city.GC("supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop: %v\n%s", err, out)
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop: %v\n%s", err, out)
		}
		out, err := city.GCStdout("beads", "city", "migrate-proxied", "--json")
		if err != nil {
			t.Fatalf("rerunning migrate-proxied: %v\n%s", err, out)
		}
		var rerun migrateProxiedCityReport
		lastJSONLine(t, out, &rerun)
		if rerun.Failed != 0 {
			t.Fatalf("rerun report = %+v", rerun)
		}
		for _, scope := range rerun.Scopes {
			if scope.Status != "already-migrated" {
				t.Errorf("%s rerun status = %q, want already-migrated", scope.Scope, scope.Status)
			}
		}
	})
}

// TestBeadsMigrateProxiedRefusesLiveLegacyServer is the hazard the spike
// proved and the reason migrate-proxied fences at all: bd's own running-server
// check consults only bd's pid file, which gc never writes, so bd happily
// commits the mode flip onto a data dir Dolt still holds an exclusive lock on
// and the workspace is unusable until the old server dies.
func TestBeadsMigrateProxiedRefusesLiveLegacyServer(t *testing.T) {
	bdPath, doltPath := requireProxiedTooling(t)
	legacyGC := requireLegacyGCBinary(t)

	newEnv := proxiedEnv(t, bdPath, doltPath)
	oldEnv := legacyGCEnv(t, newEnv, legacyGC)

	city := helpers.NewCity(t, oldEnv)
	cityRoot := city.Dir
	t.Cleanup(func() {
		_, _ = helpers.RunGC(oldEnv, cityRoot, "stop", cityRoot)
		_, _ = helpers.RunGC(newEnv, cityRoot, "stop", cityRoot)
		if leaked := waitForNoDoltProcesses(t, cityRoot, 20*time.Second); len(leaked) > 0 {
			t.Errorf("processes survived cleanup:\n%s", strings.Join(leaked, "\n"))
		}
	})

	city.InitNoStart("claude")
	assertLegacyManagedScope(t, cityRoot, "legacy city")
	// A bd write through the legacy front door guarantees the managed server is
	// up right now, not merely that init once started one.
	if out, err := city.GC("bd", "create", "hazard probe", "-p", "2"); err != nil {
		t.Fatalf("seeding through the legacy server: %v\n%s", err, out)
	}

	metadataPath := filepath.Join(cityRoot, ".beads", "metadata.json")
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	city.Env = newEnv
	out, err := city.GC("beads", "city", "migrate-proxied")
	if err == nil {
		t.Fatalf("migrate-proxied succeeded against a live legacy server:\n%s", out)
	}
	if !strings.Contains(out, "gc stop") {
		t.Errorf("refusal does not name the remedy:\n%s", out)
	}

	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused migration rewrote metadata.json:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(cityRoot, ".beads", "migrate-dolt-mode.json")); !os.IsNotExist(err) {
		t.Errorf("a refused migration left bd's migration journal behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityRoot, ".beads", "proxied_server_client_info.json")); !os.IsNotExist(err) {
		t.Errorf("a refused migration wrote bd's proxy sidecar: %v", err)
	}
}

// seedLegacyScope writes three issues and one dependency through the front
// door and returns the ids in creation order.
func seedLegacyScope(t *testing.T, city *helpers.City, rig string) []string {
	t.Helper()
	ids := make([]string, 0, 3)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		args := []string{"bd"}
		if rig != "" {
			args = append(args, "--rig", rig)
		}
		label := title
		if rig != "" {
			label = rig + " " + title
		}
		args = append(args, "create", label, "-p", "2", "--json")
		out, err := city.GCStdout(args...)
		if err != nil {
			t.Fatalf("gc %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		var created struct {
			ID string `json:"id"`
		}
		lastJSONLine(t, out, &created)
		if created.ID == "" {
			t.Fatalf("gc bd create returned no id:\n%s", out)
		}
		ids = append(ids, created.ID)
	}
	depArgs := []string{"bd"}
	if rig != "" {
		depArgs = append(depArgs, "--rig", rig)
	}
	depArgs = append(depArgs, "dep", "add", ids[1], ids[0])
	if out, err := city.GC(depArgs...); err != nil {
		t.Fatalf("gc %s: %v\n%s", strings.Join(depArgs, " "), err, out)
	}
	return ids
}

// assertSeededScope reads the seeded data back through the bd front door.
func assertSeededScope(t *testing.T, city *helpers.City, rig string, ids []string) {
	t.Helper()
	scope := "city"
	prefix := []string{"bd"}
	if rig != "" {
		scope = "rig " + rig
		prefix = append(prefix, "--rig", rig)
	}

	listOut, err := city.GCStdout(append(append([]string{}, prefix...), "list", "--json")...)
	if err != nil {
		t.Fatalf("gc bd list in the %s: %v\n%s", scope, err, listOut)
	}
	for _, id := range ids {
		if !strings.Contains(listOut, id) {
			t.Errorf("%s: bd list lost %s after the migration:\n%s", scope, id, listOut)
		}
	}

	showOut, err := city.GCStdout(append(append([]string{}, prefix...), "show", ids[0], "--json")...)
	if err != nil {
		t.Fatalf("gc bd show %s in the %s: %v\n%s", ids[0], scope, err, showOut)
	}
	if !strings.Contains(showOut, ids[0]) {
		t.Errorf("%s: bd show did not return %s:\n%s", scope, ids[0], showOut)
	}

	depOut, err := city.GCStdout(append(append([]string{}, prefix...), "dep", "list", ids[1], "--json")...)
	if err != nil {
		t.Fatalf("gc bd dep list %s in the %s: %v\n%s", ids[1], scope, err, depOut)
	}
	if !strings.Contains(depOut, ids[0]) {
		t.Errorf("%s: the %s -> %s dependency did not survive:\n%s", scope, ids[1], ids[0], depOut)
	}
}

func readScopeProjectID(t *testing.T, scopeRoot string) string {
	t.Helper()
	var metadata struct {
		ProjectID string `json:"project_id"`
	}
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	return metadata.ProjectID
}

func readScopeDoltDatabase(t *testing.T, scopeRoot string) string {
	t.Helper()
	var metadata proxiedBeadsMetadata
	readJSONFile(t, filepath.Join(scopeRoot, ".beads", "metadata.json"), &metadata)
	return metadata.DoltDatabase
}
