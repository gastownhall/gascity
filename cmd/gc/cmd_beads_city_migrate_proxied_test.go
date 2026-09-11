package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNewBeadsCmdIncludesCityMigrateProxied(t *testing.T) {
	cmd := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{})
	migrate, _, err := cmd.Find([]string{"city", "migrate-proxied"})
	if err != nil {
		t.Fatalf("Find(city migrate-proxied): %v", err)
	}
	if migrate == nil || migrate.Name() != "migrate-proxied" {
		t.Fatalf("migrate-proxied command = %#v", migrate)
	}
	for _, flag := range []string{"json", "dry-run", "rig"} {
		if migrate.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s flag", flag)
		}
	}
}

// The hazard the spike proved: bd's own running-server check consults only its
// own pid file, so a gc-started server is invisible to it and the migration
// commits onto a locked data dir. gc must refuse before bd is ever invoked, and
// without touching a byte of the scope.
func TestMigrateProxiedRefusesWhileManagedDoltStateIsPublished(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		t.Fatalf("bd must not run while the managed server is up: %s %v", scopeRoot, args)
		return nil, nil
	})

	writeManagedDoltStateFile(t, city)
	before := mustReadFile(t, filepath.Join(city, ".beads", "metadata.json"))

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "run gc stop first") {
		t.Fatalf("stderr = %q, want a gc stop instruction", stderr.String())
	}
	if got := mustReadFile(t, filepath.Join(city, ".beads", "metadata.json")); !bytes.Equal(got, before) {
		t.Fatalf("metadata.json was rewritten by a refused migration:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(city, ".beads", "migrate-dolt-mode.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused migration left a bd journal behind: %v", err)
	}
}

func TestMigrateProxiedDryRunWritesNothing(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		t.Fatalf("dry run must not run bd: %s %v", scopeRoot, args)
		return nil, nil
	})
	cityBefore := mustReadFile(t, filepath.Join(city, ".beads", "config.yaml"))
	rigBefore := mustReadFile(t, filepath.Join(rig, ".beads", "metadata.json"))

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{DryRun: true, JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstderr=%s", code, stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if !report.DryRun || len(report.Scopes) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, scope := range report.Scopes {
		if scope.Status != migrateProxiedStatusPlanned {
			t.Errorf("%s status = %q, want %q", scope.Scope, scope.Status, migrateProxiedStatusPlanned)
		}
	}
	if got := report.Scopes[1].DoltDataDir; got != "../../.beads/dolt" {
		t.Errorf("rig dolt_data_dir = %q, want ../../.beads/dolt", got)
	}
	if got := mustReadFile(t, filepath.Join(city, ".beads", "config.yaml")); !bytes.Equal(got, cityBefore) {
		t.Error("dry run rewrote the city config")
	}
	if got := mustReadFile(t, filepath.Join(rig, ".beads", "metadata.json")); !bytes.Equal(got, rigBefore) {
		t.Error("dry run rewrote the rig metadata")
	}
}

// End to end against a stub bd that behaves the way rc.2 does: it flips
// metadata.json and leaves .beads/config.yaml alone.
func TestMigrateProxiedMigratesCityAndSharedRootRig(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	calls := stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if report.Failed != 0 || len(report.Scopes) != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, scope := range report.Scopes {
		if scope.Status != migrateProxiedStatusMigrated {
			t.Errorf("%s status = %q (%s)", scope.Scope, scope.Status, scope.Error)
		}
	}

	// The city is migrated first: a rig sharing its data dir cannot be proxied
	// against a still-direct city.
	if len(*calls) == 0 || (*calls)[0].scope != normalizePathForCompare(city) {
		t.Fatalf("first bd call = %+v, want the city", *calls)
	}

	for _, scope := range []string{city, rig} {
		mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(scope, ".beads", "metadata.json"))
		if err != nil || !ok || mode != "proxied-server" {
			t.Errorf("%s dolt_mode = %q (ok=%v, err=%v)", scope, mode, ok, err)
		}
		cfg := string(mustReadFile(t, filepath.Join(scope, ".beads", "config.yaml")))
		if strings.Contains(cfg, "dolt.mode") {
			t.Errorf("%s config.yaml still carries dolt.mode:\n%s", scope, cfg)
		}
		if strings.Contains(cfg, "dolt.host") || strings.Contains(cfg, "dolt.port") {
			t.Errorf("%s config.yaml still carries a managed endpoint:\n%s", scope, cfg)
		}
	}

	dataDir, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, filepath.Join(rig, ".beads", "metadata.json"))
	if err != nil || !ok {
		t.Fatalf("rig dolt_data_dir = %q (ok=%v, err=%v)", dataDir, ok, err)
	}
	if dataDir != "../../.beads/dolt" {
		t.Errorf("rig dolt_data_dir = %q, want ../../.beads/dolt", dataDir)
	}
	if filepath.IsAbs(dataDir) {
		t.Error("rig dolt_data_dir must be relative; beads drops absolute values on save")
	}
	if db, _, _ := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(rig, ".beads", "metadata.json")); db != "sp" {
		t.Errorf("rig dolt_database = %q, want sp (identity must survive the migration)", db)
	}

	// R1: a proxied metadata binding is provider-owned on its own. Nothing new
	// belongs in .gc.
	if _, err := os.Stat(providerScopeOwnershipPath(city)); !os.IsNotExist(err) {
		t.Fatalf("migrate-proxied journaled scope ownership in .gc: %v", err)
	}
	for _, scope := range []string{city, rig} {
		owned, err := scopeProviderOwned(city, scope)
		if err != nil {
			t.Fatalf("scopeProviderOwned(%s): %v", scope, err)
		}
		if !owned {
			t.Errorf("%s is not classified provider-owned after migration", scope)
		}
	}
}

// migrate-proxied changes a city's topology inside a live process, and the
// same process then pings the migrated scope and migrates the next one through
// the bd env builders. A projection cached before the flip describes a
// managed-direct city that no longer exists, so the command drops the city's
// entries outright rather than relying on every classification input being
// stamped. Asserted on the raw cache: a stale stamp only makes an entry miss,
// it does not remove it, so an absent key is proof the forget call ran.
func TestMigrateProxiedDropsTheProjectionCache(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	for _, scope := range []string{city, rig} {
		rememberProxiedScopeRuntimeEnv(city, scope, "pre-migration-stamp",
			map[string]string{"GC_DOLT_PORT": "3306"})
		if _, cached := proxiedScopeRuntimeEnvCache.Load(proxiedScopeRuntimeEnvCacheKey(city, scope)); !cached {
			t.Fatalf("fixture did not seed a cache entry for %s", scope)
		}
	}
	t.Cleanup(func() { forgetProxiedScopeRuntimeEnv(city) })

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}

	for _, scope := range []string{city, rig} {
		if _, cached := proxiedScopeRuntimeEnvCache.Load(proxiedScopeRuntimeEnvCacheKey(city, scope)); cached {
			t.Errorf("%s kept a projection cached across the migration", scope)
		}
	}
}

func TestMigrateProxiedIsIdempotent(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("first run = %d, want 0\nstderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("second run = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if len(report.Scopes) != 1 || report.Scopes[0].Status != migrateProxiedStatusAlready {
		t.Fatalf("second run report = %+v, want already-migrated", report)
	}
}

func TestMigrateProxiedRefusesTypedScopes(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		backend string
		want    string
	}{
		{name: "embedded", mode: "embedded", backend: "dolt", want: "embedded Dolt scope"},
		{name: "doltlite", mode: "", backend: "doltlite", want: "only a dolt backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city, _ := newLegacyManagedCityFixture(t)
			writeScopeMetadataRaw(t, city, map[string]any{
				"database": "dolt", "backend": tc.backend, "dolt_mode": tc.mode, "dolt_database": "hq",
			})
			stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
				t.Fatalf("a refused scope must not reach bd: %s %v", scopeRoot, args)
				return nil, nil
			})
			var stdout, stderr bytes.Buffer
			if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 1 {
				t.Fatalf("doBeadsCityMigrateProxied() = %d, want 1\nstdout=%s", code, stdout.String())
			}
			report := decodeMigrateProxiedReport(t, stdout.String())
			if report.Failed != 1 || !strings.Contains(report.Scopes[0].Error, tc.want) {
				t.Fatalf("report = %+v, want a refusal mentioning %q", report, tc.want)
			}
		})
	}
}

func TestMigrateProxiedRefusesExternalEndpointScope(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	if err := ensureCanonicalScopeConfig(fsys.OSFS{}, city, contract.ConfigState{
		IssuePrefix:    "ci",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4406",
		DoltMode:       "server",
	}); err != nil {
		t.Fatal(err)
	}
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		t.Fatalf("an external scope must not reach bd: %s %v", scopeRoot, args)
		return nil, nil
	})

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 1", code)
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if report.Failed != 1 || !strings.Contains(report.Scopes[0].Error, "external Dolt endpoint") {
		t.Fatalf("report = %+v, want an external-endpoint refusal", report)
	}
}

// The silent-data-loss trap from the spike: a bare `dolt init` in a rig clears
// bd's refusal and then migrates the rig onto a brand-new empty database while
// its real data stays in the city's dir. Refuse instead.
func TestMigrateProxiedRefusesRigWithNoStoreAnywhere(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t, "spike")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 1\nstdout=%s", code, stdout.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if len(report.Scopes) != 2 || report.Scopes[1].Status != migrateProxiedStatusFailed {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Scopes[1].Error, "refusing to migrate it onto an empty store") {
		t.Fatalf("rig error = %q", report.Scopes[1].Error)
	}
	// The city still completed, so a rerun finishes the rest.
	if report.Scopes[0].Status != migrateProxiedStatusMigrated {
		t.Fatalf("city status = %q, want the completed scope to stay migrated", report.Scopes[0].Status)
	}
}

func TestMigrateProxiedRefusesRigWithItsOwnStoreAndACityDatabase(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	seedCityDatabaseDir(t, city, "sp")
	if err := os.MkdirAll(filepath.Join(rig, ".beads", "dolt", "sp", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := planMigrateProxiedScopes(city, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve the duplicate before migrating") {
		t.Fatalf("planMigrateProxiedScopes() error = %v, want a duplicate-store refusal", err)
	}
}

func TestMigrateProxiedRejectsUnknownRigSelector(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{Rigs: []string{"nope"}}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such rig in city.toml: nope") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMigrateProxiedRigSelectorStillMigratesTheCityFirst(t *testing.T) {
	city, rigs := newLegacyManagedCityFixture(t, "one", "two")
	two := rigs["two"]
	seedCityDatabaseDir(t, city, "on")
	seedCityDatabaseDir(t, city, "tw")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true, Rigs: []string{"one"}}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	if len(report.Scopes) != 2 || report.Scopes[0].Scope != "city" || report.Scopes[1].Scope != "rig:one" {
		t.Fatalf("report = %+v, want city + rig:one only", report)
	}
	if mode, ok, _ := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(two, ".beads", "metadata.json")); !ok || mode != "server" {
		t.Fatalf("unselected rig dolt_mode = %q, want an untouched server", mode)
	}
}

func TestSetMetadataDoltDataDirRefusesAbsolutePath(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	path := filepath.Join(city, ".beads", "metadata.json")
	err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, path, filepath.Join(city, ".beads", "dolt"))
	if err == nil || !strings.Contains(err.Error(), "must be relative") {
		t.Fatalf("SetMetadataDoltDataDir(absolute) error = %v", err)
	}
}

// Once the city is migrated, bd's own proxy holds the city data dir's Dolt
// lock and serves every database in it — including the rigs still queued
// behind it. A fence that only asked "is the lock held?" would refuse the
// command's own second step.
func TestRequireNoManagedDoltServerAcceptsABdOwnedProxyRoot(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	dataDir := filepath.Join(city, ".beads", "dolt")
	configPath := writeBdProxyRootAt(t, dataDir, 4242)
	stubBdProxyProcesses(t, map[int][]string{4242: bdProxyChildArgv(configPath)})

	if err := requireNoManagedDoltServer(city); err != nil {
		t.Fatalf("requireNoManagedDoltServer() = %v, want nil for a bd-owned proxy root", err)
	}
}

// A published gc runtime state is never excused, proxy record or not: gc and bd
// both believing they own the server is the conflict, not the evidence.
func TestRequireNoManagedDoltServerStillRefusesPublishedGCState(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	dataDir := filepath.Join(city, ".beads", "dolt")
	configPath := writeBdProxyRootAt(t, dataDir, 4243)
	stubBdProxyProcesses(t, map[int][]string{4243: bdProxyChildArgv(configPath)})
	writeManagedDoltStateFile(t, city)

	err := requireNoManagedDoltServer(city)
	if err == nil || !strings.Contains(err.Error(), "run gc stop first") {
		t.Fatalf("requireNoManagedDoltServer() = %v, want a gc stop refusal", err)
	}
}

func TestRequireNoManagedDoltServerAcceptsAStoppedCity(t *testing.T) {
	city, _ := newLegacyManagedCityFixture(t)
	if err := requireNoManagedDoltServer(city); err != nil {
		t.Fatalf("requireNoManagedDoltServer() = %v, want nil for a stopped city", err)
	}
}

// --- fixtures -------------------------------------------------------------

type migrateProxiedBdCall struct {
	scope string
	args  []string
}

// newLegacyManagedCityFixture builds the OLD-WAY layout: rigs live inside the
// city, each with its own database name but no Dolt root of its own, and every
// scope carries gc's `dolt.mode: server` in config.yaml.
func newLegacyManagedCityFixture(t *testing.T, rigNames ...string) (string, map[string]string) {
	t.Helper()
	city := normalizePathForCompare(t.TempDir())
	rigs := make([]config.Rig, 0, len(rigNames))
	paths := make(map[string]string, len(rigNames))
	for _, name := range rigNames {
		path := filepath.Join(city, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		rigs = append(rigs, config.Rig{Name: name, Path: path, Prefix: name[:2]})
	}
	writeCityEndpointCityConfig(t, city, rigs)
	writeLegacyManagedScope(t, city, "ci", "hq", contract.EndpointOriginManagedCity)
	for _, rig := range rigs {
		writeLegacyManagedScope(t, rig.Path, rig.Prefix, rig.Prefix, contract.EndpointOriginInheritedCity)
	}
	t.Setenv("GC_BEADS", "bd")
	return city, paths
}

func writeLegacyManagedScope(t *testing.T, scopeRoot, prefix, database string, origin contract.EndpointOrigin) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureCanonicalScopeConfig(fsys.OSFS{}, scopeRoot, contract.ConfigState{
		IssuePrefix:    prefix,
		EndpointOrigin: origin,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltMode:       "server",
	}); err != nil {
		t.Fatal(err)
	}
	// ensureCanonicalScopeConfig now strips proxied-server but keeps "server",
	// which is exactly what a legacy scope on disk looks like.
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: database,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeScopeMetadataRaw(t *testing.T, scopeRoot string, meta map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedCityDatabaseDir reproduces the legacy layout: every rig's database lives
// inside the city's multi-database Dolt data dir.
func seedCityDatabaseDir(t *testing.T, city, database string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// initDoltRootMarker stands in for `dolt init`: it is the repo_state.json bd's
// root validator demands and gc's data dir never had.
func initDoltRootMarker(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ".dolt", "repo_state.json"), []byte(`{"head":"refs/heads/main"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManagedDoltStateFile(t *testing.T, city string) {
	t.Helper()
	path := managedDoltStatePath(city)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"running":true,"pid":%d,"port":35402,"data_dir":%q}`, os.Getpid(), filepath.Join(city, ".beads", "dolt"))
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stubMigrateProxiedBd(t *testing.T, fn func(cityPath, scopeRoot string, args ...string) ([]byte, error)) {
	t.Helper()
	previousBd := runBdScopeCommand
	previousDolt := runDoltInitDataDir
	runBdScopeCommand = fn
	runDoltInitDataDir = func(dataDir string) ([]byte, error) {
		initDoltRootMarker(t, dataDir)
		return nil, nil
	}
	t.Cleanup(func() {
		runBdScopeCommand = previousBd
		runDoltInitDataDir = previousDolt
	})
}

// stubMigrateProxiedBdFlippingMetadata mimics rc.2: migrate rewrites
// metadata.json's dolt_mode and nothing else — config.yaml is left exactly as
// gc wrote it, which is the whole reason step (f) exists.
func stubMigrateProxiedBdFlippingMetadata(t *testing.T) *[]migrateProxiedBdCall {
	t.Helper()
	calls := &[]migrateProxiedBdCall{}
	stubMigrateProxiedBd(t, func(_, scopeRoot string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "migrate" && args[len(args)-1] == "--help" {
			return []byte("--json  emit JSON"), nil
		}
		*calls = append(*calls, migrateProxiedBdCall{scope: scopeRoot, args: args})
		switch args[0] {
		case "migrate":
			path := filepath.Join(scopeRoot, ".beads", "metadata.json")
			database, _, err := contract.ReadDoltDatabase(fsys.OSFS{}, path)
			if err != nil {
				return nil, err
			}
			if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, path, contract.MetadataState{
				Database:     "dolt",
				Backend:      "dolt",
				DoltMode:     "proxied-server",
				DoltDatabase: database,
			}); err != nil {
				return nil, err
			}
			return []byte(`{"target_mode":"proxied-server"}`), nil
		case "ping":
			return []byte(`{"status":"ok"}`), nil
		}
		return nil, fmt.Errorf("unexpected bd command %v", args)
	})
	return calls
}

func decodeMigrateProxiedReport(t *testing.T, out string) migrateProxiedReport {
	t.Helper()
	var report migrateProxiedReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return report
}
