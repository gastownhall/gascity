//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// bdTrimCounters is a snapshot of every counter the gc bd passthrough trims
// are judged by.
type bdTrimCounters struct {
	fullLoads   int64 // loadCityConfigFS calls
	passes      int64 // EnsureBuiltinRuntimeAssets calls, warm or cold
	importScans int64 // missingRequiredBuiltinImports walks
	composes    int64 // real compositions (memo hits excluded)
	fallbacks   int64 // passthrough loads that fell back to the readiness pass
}

func snapBdTrimCounters() bdTrimCounters {
	return bdTrimCounters{
		fullLoads:   loadCityConfigCalls.Load(),
		passes:      builtinReadinessPasses.Load(),
		importScans: requiredBuiltinImportScans.Load(),
		composes:    composedCityConfigLoads.Load(),
		fallbacks:   bdPassthroughReadinessFallbacks.Load(),
	}
}

func (c bdTrimCounters) since(before bdTrimCounters) bdTrimCounters {
	return bdTrimCounters{
		fullLoads:   c.fullLoads - before.fullLoads,
		passes:      c.passes - before.passes,
		importScans: c.importScans - before.importScans,
		composes:    c.composes - before.composes,
		fallbacks:   c.fallbacks - before.fallbacks,
	}
}

// newBdTrimsTestCity is newSingleExecTestCity in the shape gc init writes
// today, under a private GC_HOME: a v2 root pack with the canonical builtin
// imports (core, bd) and the rig bound in .gc/site.toml, so the readiness pass
// and the import scan both have real work to do. The caches are warm on disk,
// as on any running city, but this process has never readied the city: the
// state a fresh `gc bd` process starts in.
func newBdTrimsTestCity(t *testing.T) (cityDir string) {
	t.Helper()
	clearGCEnv(t)
	config.ResetRemoteCacheValidationCache()
	t.Cleanup(config.ResetRemoteCacheValidationCache)
	newSingleExecTestCity(t)
	cityDir = os.Getenv("GC_CITY_PATH")
	writeBdTrimsCityConfig(t, cityDir)
	materializeBuiltinPacksForTest(t, cityDir)
	resetBuiltinRuntimeReadyCacheFor(cityDir)
	resetHostedCredentialProbeCache()
	t.Cleanup(resetHostedCredentialProbeCache)
	return cityDir
}

// registerBdTrimsCity registers the city in the supervisor registry, so the
// registry-scan resolution steps can find it from its rig.
func registerBdTrimsCity(t *testing.T, cityDir string) {
	t.Helper()
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityDir, "demo"); err != nil {
		t.Fatalf("registering city: %v", err)
	}
}

// writeBdTrimsCityConfig rewrites a newSingleExecTestCity city.toml into the
// v2 shape: rig binding in .gc/site.toml, builtin imports in pack.toml.
func writeBdTrimsCityConfig(t *testing.T, cityDir string) {
	t.Helper()
	cityTOML := "[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	siteTOML := "workspace_name = \"demo\"\nworkspace_prefix = \"demo\"\n\n[[rig]]\nname = \"frontend\"\npath = " + strconv.Quote(filepath.Join(cityDir, "frontend")) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(siteTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core", "bd")
}

// TestGcBdPassthroughRunsNoFullConfigLoad pins what the passthrough pays before
// it execs bd: one composition and nothing else. No full loader call (the
// discarded rig-name load and doBd's own), no builtin readiness pass (cold or
// warm, at the load or at the write guard's store open), no required-import
// scan, and no fallback, for every way gc bd finds its city and for reads and
// writes alike.
func TestGcBdPassthroughRunsNoFullConfigLoad(t *testing.T) {
	// helpersLoad marks the argvs whose guards open a store past the scope
	// decision: the exact-id write guard (bdStoreForCityWithConfig) and the
	// rig-prefix probe, whose rig env builders (scopeUsesProxiedDoltMode,
	// scopePrefixForInit, desiredScopeDoltConfigStateForInit) call the full
	// loader. Inside the scope those calls are memo hits that run no readiness
	// pass and no import scan, which the other columns pin for every case.
	argvs := []struct {
		args        []string
		helpersLoad bool
	}{
		{[]string{"show", "demo-abc"}, false},
		{[]string{"show", "demo-abc", "--json"}, false},
		{[]string{"list", "--json"}, false},
		{[]string{"show", "fe-abc"}, true},
		{[]string{"update", "demo-abc", "--status", "in_progress"}, true},
		{[]string{"close", "demo-abc"}, true},
		{[]string{"heartbeat", "demo-abc"}, true},
		{[]string{"create", "--title", "x"}, false},
	}
	// noFullLoads marks the shapes that used to pay the discarded rig-name
	// load (walk-up is the same decoration as GC_DIR; see
	// TestResolveCityPathOnlyNamesTheSameCity). A rig target's env builders
	// load as above.
	shapes := []struct {
		name        string
		noFullLoads bool
		passes      int64 // readiness passes the city-deciding step itself runs
		setup       func(t *testing.T, cityDir string) (prefix []string)
	}{
		{"GC_CITY_PATH", true, 0, func(*testing.T, string) []string { return nil }},
		{"GC_CITY_PATH and a non-rig GC_DIR", true, 0, func(t *testing.T, _ string) []string {
			t.Setenv("GC_DIR", t.TempDir())
			return nil
		}},
		{"GC_DIR city", true, 0, func(t *testing.T, cityDir string) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_DIR", cityDir)
			return nil
		}},
		{"GC_CITY_PATH and GC_RIG", false, 0, func(t *testing.T, _ string) []string {
			t.Setenv("GC_RIG", "frontend")
			return nil
		}},
		{"--city", false, 0, func(t *testing.T, cityDir string) []string {
			t.Setenv("GC_CITY_PATH", "")
			return []string{"--city", cityDir}
		}},
		{"bd-level --rig", false, 0, func(*testing.T, string) []string { return []string{"--rig", "frontend"} }},
		// The registry scans decide the city, so their load stays (it runs
		// the one cold pass); it must run inside the invocation scope, so the
		// passthrough's own load is a memo hit and nothing composes twice.
		{"registered rig cwd", false, 1, func(t *testing.T, cityDir string) []string {
			registerBdTrimsCity(t, cityDir)
			t.Setenv("GC_CITY_PATH", "")
			setCwd(t, filepath.Join(cityDir, "frontend"))
			return nil
		}},
		{"GC_RIG only", false, 1, func(t *testing.T, cityDir string) []string {
			registerBdTrimsCity(t, cityDir)
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_RIG", "frontend")
			return nil
		}},
	}
	for _, shape := range shapes {
		for _, argv := range argvs {
			t.Run(shape.name+"/"+strings.Join(argv.args, " "), func(t *testing.T) {
				cityDir := newBdTrimsTestCity(t)
				args := append(shape.setup(t, cityDir), argv.args...)
				before := snapBdTrimCounters()
				var stdout, stderr bytes.Buffer
				code := doBd(args, &stdout, &stderr)
				got := snapBdTrimCounters().since(before)
				want := bdTrimCounters{fullLoads: got.fullLoads, passes: shape.passes, composes: 1}
				if shape.noFullLoads && !argv.helpersLoad {
					want.fullLoads = 0
				}
				if got != want {
					t.Fatalf("doBd(%q) exit %d cost %+v, want %+v; stderr=%q", args, code, got, want, stderr.String())
				}
				if currentInvocationConfigMemo() != nil {
					t.Fatal("doBd left its invocation config memo open")
				}
			})
		}
	}
}

// TestBdPassthroughConfigFallsBackToTheFullLoadOnACacheCompositionRejects pins
// the passthrough loader's one self-heal: a bundled cache whose marker
// composition rejects. The loader runs the readiness pass, the pass rewrites
// the cache, and the second composition gives the config a full load gives.
func TestBdPassthroughConfigFallsBackToTheFullLoadOnACacheCompositionRejects(t *testing.T) {
	cityDir := newBdTrimsTestCity(t)
	source, ok := builtinpacks.Source("bd")
	if !ok {
		t.Fatal("no bundled source for the bd pack")
	}
	commit := strings.TrimPrefix(config.BundledSourcePinnedVersion(source), "sha:")
	cacheDir := config.GlobalRepoCachePath(os.Getenv("GC_HOME"), source, commit)
	marker := filepath.Join(cacheDir, ".gc-bundled-pack-cache.toml")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading the bundled cache marker: %v", err)
	}
	corrupted := []byte(strings.Replace(string(data), "repository = ", "repository = \"github.com/wrong/repo\"\nunused = ", 1))
	if err := os.WriteFile(marker, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetRemoteCacheValidationCache()
	if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"), skipRevisionSnapshot); err == nil {
		t.Fatal("the corrupted marker does not fail composition; the fixture no longer reaches the fallback")
	}
	config.ResetRemoteCacheValidationCache()

	before := snapBdTrimCounters()
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(show) = %d with a cache the readiness pass heals; stderr=%q", code, stderr.String())
	}
	got := snapBdTrimCounters().since(before)
	if got.fallbacks != 1 || got.passes != 1 {
		t.Fatalf("fallbacks = %d, passes = %d; want 1 and 1", got.fallbacks, got.passes)
	}
	repository, _ := builtinpacks.RepositoryForSource(source)
	if err := builtinpacks.ValidateSyntheticRepo(cacheDir, repository, commit); err != nil {
		t.Fatalf("the fallback did not heal the cache: %v", err)
	}

	// A load that fell back ran the pass: the city is readied, and nothing is
	// deferred, so a pass that ends degraded is retried by the next caller
	// rather than skipped for the rest of the invocation.
	if err := os.WriteFile(marker, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetRemoteCacheValidationCache()
	resetBuiltinRuntimeReadyCacheFor(cityDir)
	endScope := beginInvocationConfigMemo()
	before = snapBdTrimCounters()
	if _, err := loadCityConfigForBdPassthrough(cityDir, io.Discard); err != nil {
		endScope()
		t.Fatalf("passthrough load over a corrupted marker: %v", err)
	}
	deferred := builtinReadinessDeferredInInvocation(cityDir)
	endScope()
	if got := snapBdTrimCounters().since(before); got.fallbacks != 1 || got.passes != 1 {
		t.Fatalf("second corruption: fallbacks = %d, passes = %d; want 1 and 1", got.fallbacks, got.passes)
	}
	if deferred {
		t.Fatal("a passthrough load that ran the readiness pass also recorded a deferral")
	}
	if !builtinRuntimeReadied(cityDir) {
		t.Fatal("the fallback pass did not leave the city readied")
	}

	endScope = beginInvocationConfigMemo()
	viaPassthrough, err := loadCityConfigForBdPassthrough(cityDir, io.Discard)
	endScope()
	if err != nil {
		t.Fatalf("passthrough load after the heal: %v", err)
	}
	full, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("full load: %v", err)
	}
	if !reflect.DeepEqual(viaPassthrough, full) {
		t.Fatal("the passthrough load and a full load disagree on the healed city's config")
	}
}

// TestBdPassthroughConfigNeedsNoFallbackForAnAbsentCache pins why the fallback
// is rare: an ABSENT bundled cache (a fresh binary's content hash names a new
// cache dir) is rebuilt by composition itself, offline.
func TestBdPassthroughConfigNeedsNoFallbackForAnAbsentCache(t *testing.T) {
	newBdTrimsTestCity(t)
	source, _ := builtinpacks.Source("bd")
	commit := strings.TrimPrefix(config.BundledSourcePinnedVersion(source), "sha:")
	cacheDir := config.GlobalRepoCachePath(os.Getenv("GC_HOME"), source, commit)
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}
	config.ResetRemoteCacheValidationCache()

	before := snapBdTrimCounters()
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(show) = %d with an absent cache; stderr=%q", code, stderr.String())
	}
	got := snapBdTrimCounters().since(before)
	if got.fallbacks != 0 || got.passes != 0 {
		t.Fatalf("fallbacks = %d, passes = %d; want 0 and 0", got.fallbacks, got.passes)
	}
	repository, _ := builtinpacks.RepositoryForSource(source)
	if err := builtinpacks.ValidateSyntheticRepo(cacheDir, repository, commit); err != nil {
		t.Fatalf("composition did not rematerialize the absent cache: %v", err)
	}
}

// TestFullLoadsRunTheReadinessPassOncePerInvocation pins the once-per-
// invocation rule and its boundary: inside a one-shot scope a readied city is
// not re-validated, and outside one (a long-lived process) every load still
// runs the warm revalidation.
func TestFullLoadsRunTheReadinessPassOncePerInvocation(t *testing.T) {
	cityDir := newBdTrimsTestCity(t)

	before := snapBdTrimCounters()
	endScope := beginInvocationConfigMemo()
	for i := 0; i < 2; i++ {
		if _, err := loadCityConfig(cityDir, io.Discard); err != nil {
			endScope()
			t.Fatal(err)
		}
	}
	endScope()
	if got := snapBdTrimCounters().since(before).passes; got != 1 {
		t.Fatalf("two full loads in one invocation ran %d readiness passes, want 1", got)
	}

	before = snapBdTrimCounters()
	for i := 0; i < 2; i++ {
		if _, err := loadCityConfig(cityDir, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if got := snapBdTrimCounters().since(before).passes; got != 2 {
		t.Fatalf("two full loads outside an invocation scope ran %d readiness passes, want 2", got)
	}
}

// TestBdPassthroughReadinessDeferralIsScoped pins that the deferral a
// passthrough load records covers the store opens of its own invocation, and
// nothing after it.
func TestBdPassthroughReadinessDeferralIsScoped(t *testing.T) {
	cityDir := newBdTrimsTestCity(t)
	// The deferral is keyed by the normalized path: a city reached through a
	// symlink is the same city.
	link := filepath.Join(t.TempDir(), "city-link")
	if err := os.Symlink(cityDir, link); err != nil {
		t.Fatal(err)
	}
	open := func(cfg *config.City, cityPath string) {
		_, _ = openStoreResultAtForCityWithConfig(
			filepath.Join(cityPath, ".beads"), cityPath, cfg, gate.ModeUnset, false, false, false, nil)
	}

	// Load through the link and open through the real path (and the link):
	// the deferral must name the city, not the spelling of its path.
	endScope := beginInvocationConfigMemo()
	cfg, err := loadCityConfigForBdPassthrough(link, io.Discard)
	if err != nil {
		endScope()
		t.Fatal(err)
	}
	before := snapBdTrimCounters()
	open(cfg, cityDir)
	open(cfg, link)
	inScope := snapBdTrimCounters().since(before).passes
	endScope()
	if inScope != 0 {
		t.Fatalf("store opens inside the passthrough's invocation ran %d readiness passes, want 0", inScope)
	}
	if builtinReadinessDeferredInInvocation(cityDir) {
		t.Fatal("the deferral outlived its invocation scope")
	}

	before = snapBdTrimCounters()
	open(cfg, cityDir)
	if got := snapBdTrimCounters().since(before).passes; got != 1 {
		t.Fatalf("a store open after the scope ended ran %d readiness passes, want 1", got)
	}
}

// TestGcBdDoesNotPrintTheMissingBuiltinImportHint pins where that hint lives:
// gc status, gc start and gc doctor print it (TestStatusWarnsOnMissingBuiltinImports),
// the gc bd data-plane passthrough does not.
func TestGcBdDoesNotPrintTheMissingBuiltinImportHint(t *testing.T) {
	clearGCEnv(t)
	newSingleExecTestCity(t)
	cityDir := os.Getenv("GC_CITY_PATH")
	// The control: a full load with a visible writer does print it for this
	// city, so the assertion below is about gc bd and not about the fixture.
	builtinImportWarningCache.Delete(normalizePathForCompare(cityDir))
	var full bytes.Buffer
	if _, err := loadCityConfig(cityDir, &full); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.String(), "does not import required builtin pack") {
		t.Fatalf("a full load printed no missing-import hint for the fixture; stderr=%q", full.String())
	}
	builtinImportWarningCache.Delete(normalizePathForCompare(cityDir))

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(show) = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "does not import required builtin pack") {
		t.Fatalf("gc bd printed the missing-import hint: %q", stderr.String())
	}
}

// TestGcBdStillPrintsConfigLoadWarnings pins that skipping the import scan did
// not take the composition's own warnings with it.
func TestGcBdStillPrintsConfigLoadWarnings(t *testing.T) {
	cityDir := newBdTrimsTestCity(t)
	f, err := os.OpenFile(filepath.Join(cityDir, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[agents]\nmodel = \"x\"\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(show) = %d; stderr=%q", code, stderr.String())
	}
	const warning = "[agents] is a deprecated compatibility alias"
	if n := strings.Count(stderr.String(), warning); n != 1 {
		t.Fatalf("gc bd printed the config warning %d times, want 1; stderr=%q", n, stderr.String())
	}
}

// TestGcBdValidationErrorsMatchTheFullLoad pins that a config which composes
// but fails validation is not mistaken for a cache problem: no fallback pass,
// and the same exit code and text the full load gives.
func TestGcBdValidationErrorsMatchTheFullLoad(t *testing.T) {
	run := func(legacy bool) (int, string, bdTrimCounters) {
		clearGCEnv(t)
		newSingleExecTestCity(t)
		cityDir := writeRuntimeCityFixture(t, "subprocess")
		t.Setenv("GC_CITY_PATH", cityDir)
		setCwd(t, cityDir)
		if legacy {
			defer useLegacyBdPassthroughSeams()()
		}
		before := snapBdTrimCounters()
		var stdout, stderr bytes.Buffer
		code := doBd([]string{"show", "demo-abc"}, &stdout, &stderr)
		return code, strings.ReplaceAll(stderr.String(), cityDir, "$CITY"), snapBdTrimCounters().since(before)
	}

	oldCode, oldStderr, _ := run(true)
	// The one intended difference: the legacy full load also printed the
	// missing-builtin-import hint.
	oldStderr = dropMissingBuiltinImportHint(oldStderr)
	newCode, newStderr, got := run(false)
	if oldCode != newCode || oldStderr != newStderr {
		t.Fatalf("validation failure differs:\n old: %d %q\n new: %d %q", oldCode, oldStderr, newCode, newStderr)
	}
	if newCode == 0 {
		t.Fatal("a pack runtime shadowing a builtin did not fail gc bd; the fixture no longer reaches validation")
	}
	if got.fallbacks != 0 || got.passes != 0 {
		t.Fatalf("a validation error ran fallbacks = %d, passes = %d; want 0 and 0", got.fallbacks, got.passes)
	}
}

// useLegacyBdPassthroughSeams puts doBd back on the loaders it used before the
// trims: the general resolver (with its rig decoration) and the full load. It
// returns the restore, which the caller runs as soon as its legacy run ends.
func useLegacyBdPassthroughSeams() (restore func()) {
	origResolve, origLoad := resolveBdCityPath, loadBdPassthroughConfig
	resolveBdCityPath = resolveCity
	loadBdPassthroughConfig = func(cityPath string, w io.Writer) (*config.City, error) {
		return loadCityConfig(cityPath, w)
	}
	return func() { resolveBdCityPath, loadBdPassthroughConfig = origResolve, origLoad }
}

// TestResolveCityPathOnlyNamesTheSameCity pins resolveCityPathOnly against
// resolveCity across the resolution chain: the same city or the same error,
// never a rig, and no config load wherever the rig decoration was the only
// load. The steps that load to decide the city (the registry scans) still do.
// The walk-up (step 10) is refused in test binaries; its decoration is the
// same rigFromCwdDir call step 8 makes, and testdata/bd-passthrough-walkup.txtar
// covers its routing end to end.
func TestResolveCityPathOnlyNamesTheSameCity(t *testing.T) {
	type fixture struct{ city, rig, other string }
	cases := []struct {
		name      string
		setup     func(t *testing.T, f fixture)
		wantLoads bool // whether the city-deciding step itself loads config
		wantErr   bool
	}{
		{name: "--city and --rig", setup: func(_ *testing.T, f fixture) {
			cityFlag, rigFlag = f.city, "hauler"
		}},
		{name: "--city", setup: func(t *testing.T, f fixture) {
			cityFlag = f.city
			setCwd(t, f.rig)
		}},
		{name: "--rig", wantLoads: true, setup: func(_ *testing.T, _ fixture) { rigFlag = "hauler" }},
		{name: "GC_CITY and GC_RIG", setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_CITY", f.city)
			t.Setenv("GC_RIG", "hauler")
		}},
		{name: "GC_CITY from a rig cwd", setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_CITY", f.city)
			setCwd(t, f.rig)
		}},
		{name: "GC_CITY and a rig GC_DIR", setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_CITY", f.city)
			t.Setenv("GC_DIR", f.rig)
		}},
		{name: "GC_CITY and a non-rig GC_DIR", setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_CITY", f.city)
			t.Setenv("GC_DIR", f.other)
		}},
		{name: "GC_RIG only", wantLoads: true, setup: func(t *testing.T, _ fixture) { t.Setenv("GC_RIG", "hauler") }},
		{name: "GC_DIR legacy misfire", wantLoads: true, setup: func(t *testing.T, f fixture) {
			if err := os.MkdirAll(filepath.Join(f.rig, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_DIR", f.rig)
		}},
		{name: "GC_DIR city", setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_DIR", f.city)
			setCwd(t, f.rig)
		}},
		{name: "registered rig cwd", wantLoads: true, setup: func(t *testing.T, f fixture) { setCwd(t, f.rig) }},
		{name: "no city", wantErr: true, setup: func(t *testing.T, f fixture) { setCwd(t, f.other) }},
		{name: "remote target", wantErr: true, setup: func(t *testing.T, f fixture) {
			t.Setenv("GC_CITY_URL", "https://box.internal:9443")
			t.Setenv("GC_CITY", f.city)
		}},
		{name: "unknown GC_RIG", wantErr: true, setup: func(t *testing.T, _ fixture) { t.Setenv("GC_RIG", "nope") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, _ := setupDiscoveryLockCity(t)
			rigDir := registerRigInSiteBinding(t, cityPath, "hauler")
			declareRigInCityTOML(t, cityPath, "hauler")
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_URL", "")
			t.Setenv("GC_CITY_CONTEXT", "")
			other := t.TempDir()
			setCwd(t, other)
			tc.setup(t, fixture{city: cityPath, rig: rigDir, other: other})

			wantCity, wantErr := resolveCity()
			fullCtx, _ := resolveContext()

			before := snapBdTrimCounters()
			ctx, err := resolveContextMode(cityOnlyResolution)
			loads := snapBdTrimCounters().since(before).fullLoads
			gotCity, gotErr := resolveCityPathOnly()

			if (wantErr != nil) != tc.wantErr {
				t.Fatalf("resolveCity() err = %v, want error %v (fixture drift)", wantErr, tc.wantErr)
			}
			if gotCity != wantCity || errString(gotErr) != errString(wantErr) {
				t.Fatalf("resolveCityPathOnly() = %q, %v; resolveCity() = %q, %v", gotCity, gotErr, wantCity, wantErr)
			}
			if errString(err) != errString(wantErr) || ctx.CityPath != wantCity {
				t.Fatalf("city-only context = %+v, %v; want city %q, %v", ctx, err, wantCity, wantErr)
			}
			// The registry steps answer with the rig they matched; that is the
			// city-deciding step's own output, not the decoration.
			if !tc.wantLoads && ctx.RigName != "" && ctx.RigName != fullCtx.RigName {
				t.Fatalf("city-only resolution decorated rig %q", ctx.RigName)
			}
			if !tc.wantLoads && !tc.wantErr && loads != 0 {
				t.Fatalf("city-only resolution ran %d full config loads, want 0", loads)
			}
		})
	}
}

// TestCityOnlyResolutionDecoratesNoRig pins the decoration skip directly for
// the steps where the general resolver derives the rig from a directory.
func TestCityOnlyResolutionDecoratesNoRig(t *testing.T) {
	cityPath, _ := setupDiscoveryLockCity(t)
	rigDir := registerRigInSiteBinding(t, cityPath, "hauler")
	declareRigInCityTOML(t, cityPath, "hauler")
	for _, shape := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"GC_CITY from a rig cwd", func(t *testing.T) {
			t.Setenv("GC_CITY", cityPath)
			setCwd(t, rigDir)
		}},
		{"GC_DIR city from a rig GC_DIR", func(t *testing.T) {
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_DIR", cityPath)
			setCwd(t, rigDir)
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			shape.setup(t)
			if got := rigFromCwdDir(cityPath, rigDir, authoritativeResolution); got != "hauler" {
				t.Fatalf("the decoration itself found rig %q, want hauler (fixture drift)", got)
			}
			if got := rigFromCwdDir(cityPath, rigDir, cityOnlyResolution); got != "" {
				t.Fatalf("city-only decoration = %q, want none", got)
			}
		})
	}
}

// TestCityOnlyResolutionWaitsOnABusyRepoCache pins that city-only is not
// advisory: a gc bd the user typed from a rig directory waits for another
// process's repo-cache write and then resolves, rather than failing with
// config.ErrRepoCacheBusy the way eager discovery does.
func TestCityOnlyResolutionWaitsOnABusyRepoCache(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	rigDir := registerRigInSiteBinding(t, cityPath, "drayman")
	declareRigInCityTOML(t, cityPath, "drayman")
	t.Setenv("GC_CITY", "")
	setCwd(t, rigDir)
	release := holdExclusiveRepoCacheLock(t, cacheRoot)

	type result struct {
		city string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		city, err := resolveCityPathOnly()
		done <- result{city, err}
	}()
	select {
	case got := <-done:
		release()
		t.Fatalf("city-only resolution returned (%q, %v) while the repo-cache lock was held; it must wait like any typed command", got.city, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	release()
	got := <-done
	if got.err != nil || got.city != cityPath {
		t.Fatalf("after the lock was released: city %q, err %v; want %q", got.city, got.err, cityPath)
	}
}

// TestBdPassthroughConfigAppliesFeatureFlags pins that the passthrough loader
// keeps the full load's tail: the process feature flags follow the city's
// [daemon] formula_v2, as they do after loadCityConfig.
func TestBdPassthroughConfigAppliesFeatureFlags(t *testing.T) {
	cityDir := newBdTrimsTestCity(t)
	appendFile(t, filepath.Join(cityDir, "city.toml"), "\n[daemon]\nformula_v2 = false\n")
	featureflags.WithScoped(featureflags.Flags{FormulaV2: true, GraphApply: true}, func() {
		endScope := beginInvocationConfigMemo()
		_, err := loadCityConfigForBdPassthrough(cityDir, io.Discard)
		endScope()
		if err != nil {
			t.Fatal(err)
		}
		if got := featureflags.Snapshot(); got.FormulaV2 || got.GraphApply {
			t.Fatalf("feature flags after the passthrough load = %+v, want both off per [daemon] formula_v2 = false", got)
		}
	})
}

// dropMissingBuiltinImportHint removes the missing-builtin-import hint, which
// the legacy full load prints and gc bd no longer does
// (TestGcBdDoesNotPrintTheMissingBuiltinImportHint), from captured stderr.
func dropMissingBuiltinImportHint(s string) string {
	var kept []string
	for _, line := range strings.SplitAfter(s, "\n") {
		if !strings.Contains(line, "does not import required builtin pack") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}
