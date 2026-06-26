package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// failingPathResolve returns a pathResolve closure that fails the test if it is
// ever invoked — used to prove a bare name resolves via the registry and is
// never fed to the (walk-up) path resolver.
func failingPathResolve(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(ref string) (string, error) {
		t.Fatalf("pathResolve must not be called for ref %q", ref)
		return "", nil
	}
}

func mkTestCity(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkRuntimeRootOnlyCity creates a legacy runtime-root-only city: a directory
// with a .gc/ runtime root but no city.toml. validateCityPath accepts this
// shape via HasRuntimeRoot, so bare-name resolution must recognize it as a
// local city too.
func mkRuntimeRootOnlyCity(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyCityRef(t *testing.T) {
	tests := []struct {
		in   string
		want cityRefKind
	}{
		{"", cityRefEmpty},
		{"   ", cityRefEmpty},
		{"chris-city", cityRefName},
		{"a.b_c-1", cityRefName},
		{"a/b", cityRefPath},
		{"./x", cityRefPath},
		{"../x", cityRefPath},
		{"/abs/path", cityRefPath},
		{"~/x", cityRefPath},
	}
	for _, tt := range tests {
		if got := classifyCityRef(tt.in); got != tt.want {
			t.Errorf("classifyCityRef(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestResolveCityRefPathShapedSkipsRegistry(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir()) // empty registry; must not be consulted
	called := false
	pathResolve := func(ref string) (string, error) {
		called = true
		return "/resolved/" + ref, nil
	}
	got, err := resolveCityRef("foo/bar", cityRefOpts{allowNameFallback: true}, pathResolve)
	if err != nil {
		t.Fatal(err)
	}
	if !called || got != "/resolved/foo/bar" {
		t.Fatalf("path-shaped ref must go straight to pathResolve; called=%v got=%q", called, got)
	}
}

func TestResolveCityRefNameNoLocalDirHitsRegistry(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir()) // cwd has no ./alpha

	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCityRef("alpha", cityRefOpts{allowNameFallback: true}, failingPathResolve(t))
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, cityPath) {
		t.Fatalf("got %q, want registered path %q", got, cityPath)
	}
}

func TestResolveCityRefNameNoMatchLoudError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	_, err := resolveCityRef("ghost", cityRefOpts{allowNameFallback: true}, failingPathResolve(t))
	if err == nil {
		t.Fatal("expected a loud error for an unknown name with no local city")
	}
	for _, want := range []string{"not a registered city name", "not a city directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestResolveCityRefLocalCityWins(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	local := filepath.Join(cwd, "mycity")
	mkTestCity(t, local) // ./mycity is a city, not registered

	called := ""
	pathResolve := func(ref string) (string, error) { called = ref; return local, nil }
	got, err := resolveCityRef("mycity", cityRefOpts{allowNameFallback: true}, pathResolve)
	if err != nil {
		t.Fatal(err)
	}
	if called != "mycity" {
		t.Fatalf("a local city must route through pathResolve; called=%q", called)
	}
	if !samePath(got, local) {
		t.Fatalf("got %q, want local city %q", got, local)
	}
}

func TestResolveCityRefAmbiguousLoudError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	mkTestCity(t, filepath.Join(cwd, "dup")) // local ./dup city

	elsewhere := filepath.Join(t.TempDir(), "dup")
	mkTestCity(t, elsewhere)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(elsewhere, "dup"); err != nil {
		t.Fatal(err)
	}

	_, err := resolveCityRef("dup", cityRefOpts{allowNameFallback: true}, failingPathResolve(t))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

// A legacy runtime-root-only (.gc/-only, no city.toml) local city must win for a
// bare name, matching validateCityPath's HasRuntimeRoot acceptance, instead of
// falling through to the registry and failing as "not registered".
func TestResolveCityRefRuntimeRootOnlyLocalCityWins(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	local := filepath.Join(cwd, "rtcity")
	mkRuntimeRootOnlyCity(t, local) // ./rtcity has only .gc/, not registered

	called := ""
	pathResolve := func(ref string) (string, error) { called = ref; return local, nil }
	got, err := resolveCityRef("rtcity", cityRefOpts{allowNameFallback: true}, pathResolve)
	if err != nil {
		t.Fatal(err)
	}
	if called != "rtcity" {
		t.Fatalf("a .gc/-only local city must route through pathResolve; called=%q", called)
	}
	if !samePath(got, local) {
		t.Fatalf("got %q, want local city %q", got, local)
	}
}

// A runtime-root-only local city whose name conflicts with a different
// registered city is ambiguous, exactly like the city.toml case.
func TestResolveCityRefRuntimeRootOnlyLocalCityAmbiguousWithRegistered(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	mkRuntimeRootOnlyCity(t, filepath.Join(cwd, "dup")) // local ./dup with only .gc/

	elsewhere := filepath.Join(t.TempDir(), "dup")
	mkTestCity(t, elsewhere)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(elsewhere, "dup"); err != nil {
		t.Fatal(err)
	}

	_, err := resolveCityRef("dup", cityRefOpts{allowNameFallback: true}, failingPathResolve(t))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error for a .gc/-only local city conflicting with a registration, got %v", err)
	}
}

func TestResolveCityRefLocalAndRegisteredSamePathNotAmbiguous(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	local := filepath.Join(cwd, "same")
	mkTestCity(t, local)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(local, "same"); err != nil {
		t.Fatal(err)
	}

	called := false
	pathResolve := func(string) (string, error) { called = true; return local, nil }
	got, err := resolveCityRef("same", cityRefOpts{allowNameFallback: true}, pathResolve)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !samePath(got, local) {
		t.Fatalf("same-path local+registered must resolve to the local city via pathResolve; called=%v got=%q", called, got)
	}
}

func TestResolveCityRefRegisterNoNameFallback(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}

	called := false
	pathResolve := func(ref string) (string, error) { called = true; return "/p/" + ref, nil }
	got, err := resolveCityRef("alpha", cityRefOpts{allowNameFallback: false}, pathResolve)
	if err != nil {
		t.Fatal(err)
	}
	if !called || got != "/p/alpha" {
		t.Fatalf("register (no name fallback) must treat a name-shaped ref as a path; called=%v got=%q", called, got)
	}
}

// Core regression guard: a bare name run from INSIDE another city must resolve
// via the registry, never via the path resolver's upward walk to the ambient
// city (the footgun the design exists to prevent).
func TestResolveCityRefFromInsideCityDoesNotWalkUp(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	ambient := t.TempDir()
	mkTestCity(t, ambient)
	t.Chdir(ambient) // cwd is itself a city

	otherPath := filepath.Join(t.TempDir(), "other")
	mkTestCity(t, otherPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(otherPath, "other"); err != nil {
		t.Fatal(err)
	}

	// walkUp simulates findCity: it returns the ambient city. If resolveCityRef
	// ever feeds the bare name to it, the assertion below catches the mis-target.
	walkUp := func(string) (string, error) { return ambient, nil }
	got, err := resolveCityRef("other", cityRefOpts{allowNameFallback: true}, walkUp)
	if err != nil {
		t.Fatal(err)
	}
	if samePath(got, ambient) {
		t.Fatalf("bare name resolved to the AMBIENT city %q (walk-up footgun)", ambient)
	}
	if !samePath(got, otherPath) {
		t.Fatalf("got %q, want registered other-city %q", got, otherPath)
	}
}

// resolveCommandCity is the central seam reload/suspend/resume/status use; a
// name passed to it must resolve via the registry.
func TestResolveCommandCityByName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCommandCity([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, cityPath) {
		t.Fatalf("resolveCommandCity(alpha) = %q, want %q", got, cityPath)
	}
}

// The central seam must also resist the walk-up footgun: a bare name from
// inside city A targets the registered city, not A.
func TestResolveCommandCityByNameFromInsideAnotherCity(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	ambient := t.TempDir()
	mkTestCity(t, ambient)
	t.Chdir(ambient)
	otherPath := filepath.Join(t.TempDir(), "other")
	mkTestCity(t, otherPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(otherPath, "other"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCommandCity([]string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if samePath(got, ambient) {
		t.Fatalf("resolveCommandCity targeted the ambient city %q (walk-up footgun)", ambient)
	}
	if !samePath(got, otherPath) {
		t.Fatalf("resolveCommandCity(other) = %q, want %q", got, otherPath)
	}
}

func TestResolveCommandCityUnknownNameFailsLoudly(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	_, err := resolveCommandCity([]string{"ghost-name"})
	if err == nil || !strings.Contains(err.Error(), "not a registered city name") {
		t.Fatalf("err = %v, want a name-aware not-found error", err)
	}
}

func TestResolveCityFlagValueByName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCityFlagValue("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, cityPath) {
		t.Fatalf("resolveCityFlagValue(alpha) = %q, want %q", got, cityPath)
	}
}

func TestResolveExplicitCityPathEnvByName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}

	// GC_CITY accepts a registered name.
	t.Setenv("GC_CITY", "alpha")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	got, ok := resolveExplicitCityPathEnv()
	if !ok || !samePath(got, cityPath) {
		t.Fatalf("resolveExplicitCityPathEnv() = (%q,%v), want (%q,true)", got, ok, cityPath)
	}

	// GC_CITY_PATH is path-only: a bare name must NOT resolve via the registry.
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "alpha")
	if _, ok := resolveExplicitCityPathEnv(); ok {
		t.Fatal("GC_CITY_PATH must not resolve a bare registered name")
	}
}

func TestRestartTargetResolvesByNameOnce(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil { // bootstrapped runtime root
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}
	gotPath, gotName, err := restartTarget([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(gotPath, cityPath) {
		t.Fatalf("restartTarget path = %q, want %q", gotPath, cityPath)
	}
	if gotName != "alpha" {
		t.Fatalf("restartTarget name = %q, want alpha", gotName)
	}
}

// The bespoke per-command resolvers (resolveStopCityPath, resolveStartDir)
// must also accept a name and resist the walk-up footgun; the central
// resolveCommandCity tests cover reload/suspend/resume/status, which share
// that seam.
func TestResolveStopCityPathByNameFromInsideAnotherCity(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	ambient := t.TempDir()
	mkTestCity(t, ambient)
	t.Chdir(ambient)
	otherPath := filepath.Join(t.TempDir(), "other")
	mkTestCity(t, otherPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(otherPath, "other"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveStopCityPath([]string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if samePath(got, ambient) {
		t.Fatalf("resolveStopCityPath targeted the ambient city %q (walk-up footgun)", ambient)
	}
	if !samePath(got, otherPath) {
		t.Fatalf("resolveStopCityPath(other) = %q, want %q", got, otherPath)
	}
}

// Regression (PR #3625 review, Contract & Interface Fidelity): a slashless
// local rig directory with no city.toml and no .gc/ must still resolve to its
// owning city+rig through the positional-arg seam. The name-first branch must
// probe the rig-path resolver before failing, instead of rejecting "frontend"
// as an unknown city name.
func TestResolveCommandContextSlashlessRigDirResolvesToCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	cityPath := setupCity(t, "demo-city")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend") // plain dir: no city.toml, no .gc/
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityPath, toml)
	registerCityForRigResolution(t, os.Getenv("GC_HOME"), cityPath, "demo-city")

	setCwd(t, parent) // cwd/frontend == rigDir, but "frontend" classifies as a name

	ctx, err := resolveCommandContext([]string{"frontend"})
	if err != nil {
		t.Fatalf("resolveCommandContext(frontend) error: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
	if ctx.RigName != "frontend" {
		t.Fatalf("RigName = %q, want frontend", ctx.RigName)
	}
}

// Regression companion for the stop seam: resolveStopCityPath must resolve a
// slashless local rig directory to its owning city, not reject it as an unknown
// city name.
func TestResolveStopCityPathSlashlessRigDirResolvesToCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	cityPath := setupCity(t, "stop-demo-city")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend") // plain dir: no city.toml, no .gc/
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"stop-demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityPath, toml)
	registerCityForRigResolution(t, os.Getenv("GC_HOME"), cityPath, "stop-demo-city")

	setCwd(t, parent)

	got, err := resolveStopCityPath([]string{"frontend"})
	if err != nil {
		t.Fatalf("resolveStopCityPath(frontend) error: %v", err)
	}
	assertSameTestPath(t, got, cityPath)
}

// Regression companion for the start seam (PR #3625 review F1): resolveStartDir
// must resolve a slashless local rig directory to its owning city, not reject it
// as an unknown city name. Without the rig-path probe, gc start <rig-name>
// regressed to a hard error where the sibling stop/command-context seams resolve.
func TestResolveStartDirSlashlessRigDirResolvesToCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	cityPath := setupCity(t, "start-demo-city")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend") // plain dir: no city.toml, no .gc/
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"start-demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityPath, toml)
	registerCityForRigResolution(t, os.Getenv("GC_HOME"), cityPath, "start-demo-city")

	setCwd(t, parent)

	got, err := resolveStartDir([]string{"frontend"})
	if err != nil {
		t.Fatalf("resolveStartDir(frontend) error: %v", err)
	}
	assertSameTestPath(t, got, cityPath)
}

// Regression companion for the restart seam (PR #3625 review F1): restartTarget
// delegates to resolveStartDir, so gc restart <rig-name> must resolve a slashless
// local rig directory to its owning city just like gc stop <rig-name>. The
// documented "stop then start" contract breaks if restart rejects a rig name the
// stop leg accepts.
func TestRestartTargetSlashlessRigDirResolvesToCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	cityPath := setupCity(t, "restart-demo-city")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend") // plain dir: no city.toml, no .gc/
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"restart-demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityPath, toml)
	registerCityForRigResolution(t, os.Getenv("GC_HOME"), cityPath, "restart-demo-city")

	setCwd(t, parent)

	gotPath, _, err := restartTarget([]string{"frontend"})
	if err != nil {
		t.Fatalf("restartTarget(frontend) error: %v", err)
	}
	assertSameTestPath(t, gotPath, cityPath)
}

// Guard: the rig-path probe must NOT reopen the walk-up footgun. A slashless
// name that is neither a registered city, a local city, nor a local rig must
// still fail loudly from inside an ambient city instead of silently targeting
// it.
func TestResolveCommandContextSlashlessUnknownFromInsideCityFailsLoud(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	ambient := setupCity(t, "ambient-city")
	setCwd(t, ambient) // inside a city; the old footgun would walk up to here

	_, err := resolveCommandContext([]string{"ghost-rig"})
	if err == nil {
		t.Fatal("expected a loud error for an unknown slashless name from inside a city")
	}
	if !strings.Contains(err.Error(), "not a registered city name") {
		t.Fatalf("err = %v, want a name-aware not-found error", err)
	}
}

func TestResolveStartDirByName(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, cityPath)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveStartDir([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, cityPath) {
		t.Fatalf("resolveStartDir(alpha) = %q, want %q", got, cityPath)
	}
}

// GC_CITY uses path-first / local-wins precedence (a documented deviation from
// the loud-ambiguity policy of the positional arg and --city flag): a local
// city dir of the same name wins over a different registration, silently.
func TestResolveExplicitCityPathEnvLocalWinsOverRegistration(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	local := filepath.Join(cwd, "alpha")
	mkTestCity(t, local) // local ./alpha city
	registeredElsewhere := filepath.Join(t.TempDir(), "alpha")
	mkTestCity(t, registeredElsewhere)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(registeredElsewhere, "alpha"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "alpha")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	got, ok := resolveExplicitCityPathEnv()
	if !ok {
		t.Fatal("GC_CITY=alpha should resolve")
	}
	if !samePath(got, local) {
		t.Fatalf("GC_CITY=alpha resolved to %q, want the local city %q (path-first/local-wins)", got, local)
	}
}

func TestCityNameCandidatesFiltersByPrefix(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	for _, n := range []string{"alpha", "alpine", "beta"} {
		p := filepath.Join(t.TempDir(), n)
		mkTestCity(t, p)
		if err := reg.Register(p, n); err != nil {
			t.Fatal(err)
		}
	}
	got := cityNameCandidates("alp")
	if len(got) != 2 {
		t.Fatalf("cityNameCandidates(\"alp\") = %v, want 2 (alpha, alpine)", got)
	}
	for _, c := range got {
		if !strings.HasPrefix(c, "alp") {
			t.Fatalf("candidate %q does not match prefix", c)
		}
		if !strings.Contains(c, "\t") {
			t.Fatalf("candidate %q missing tab-separated path description", c)
		}
	}
}

// setupSlashlessRigDirCollision builds the "cwd/<name> is a local rig dir AND
// <name> is a registered city" scenario shared by the ambiguity regressions:
// city "demo-city" owns a slashless rig directory cwd/frontend, and a DIFFERENT
// city is registered under the bare name "frontend". Returns the parent dir the
// caller should cd into so that cwd/frontend is the rig directory.
func setupSlashlessRigDirCollision(t *testing.T) string {
	t.Helper()
	gcHome := os.Getenv("GC_HOME")

	cityA := setupCity(t, "demo-city")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend") // plain dir: no city.toml, no .gc/
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityA, toml)
	registerCityForRigResolution(t, gcHome, cityA, "demo-city")

	cityB := setupCity(t, "frontend") // a different city, registered under the same bare name
	registerCityForRigResolution(t, gcHome, cityB, "frontend")
	return parent
}

// Regression (PR #3625 review, Behavioral Correctness, major): when cwd/<name>
// is a real local rig directory AND <name> is ALSO a registered city pointing
// to a different city, the positional seam must reject the collision loudly
// instead of silently preferring the registered city over the local rig dir.
// The old path behavior targeted the rig's owning city; the new name resolution
// must not silently shadow it. Mirrors the local-city-vs-registration guard.
func TestResolveCommandContextSlashlessRigDirVsRegisteredCityIsAmbiguous(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	parent := setupSlashlessRigDirCollision(t)
	setCwd(t, parent) // cwd/frontend == rig dir, and "frontend" is also registered

	_, err := resolveCommandContext([]string{"frontend"})
	if err == nil {
		t.Fatal("expected a loud ambiguity error: cwd/frontend is a local rig dir AND frontend is a registered city")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "rig") {
		t.Fatalf("err = %v, want an ambiguity error naming the local rig directory", err)
	}
}

// Regression companion for the stop seam: resolveStopCityPath must reject the
// same rig-dir/registered-city collision loudly, not silently stop a different
// city than the local rig the operator pointed at.
func TestResolveStopCityPathSlashlessRigDirVsRegisteredCityIsAmbiguous(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	parent := setupSlashlessRigDirCollision(t)
	setCwd(t, parent)

	_, err := resolveStopCityPath([]string{"frontend"})
	if err == nil {
		t.Fatal("expected a loud ambiguity error from the stop seam for a rig-dir/registered-city collision")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "rig") {
		t.Fatalf("err = %v, want an ambiguity error naming the local rig directory", err)
	}
}

// Guard: when cwd/<name> is a local rig dir whose OWNING city is the very same
// city registered under <name>, the two interpretations agree, so resolution
// must succeed instead of raising a false-positive ambiguity error.
func TestResolveCommandContextSlashlessRigDirSameRegisteredCityResolves(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	gcHome := os.Getenv("GC_HOME")
	city := setupCity(t, "frontend")
	parent := t.TempDir()
	rigDir := filepath.Join(parent, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"frontend\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, city, toml)
	registerCityForRigResolution(t, gcHome, city, "frontend")

	setCwd(t, parent)

	ctx, err := resolveCommandContext([]string{"frontend"})
	if err != nil {
		t.Fatalf("resolveCommandContext(frontend) where the rig's owning city IS the registered city: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, city)
}

// Regression (PR #3625 review, Test Evidence Quality): an unknown slashless name
// run from INSIDE a registered rig directory resolves to that rig's owning city
// — parity with the historical path-arg behavior — rather than failing loud.
// Pins the documented outcome for the ancestor-rig-scope match; the fail-loud
// guard above only covers a city root with no rigs in scope.
func TestResolveCommandContextSlashlessUnknownFromInsideRigResolvesToOwningCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	gcHome := os.Getenv("GC_HOME")
	cityPath := setupCity(t, "demo-city")
	rigDir := t.TempDir()
	toml := "[workspace]\nname = \"demo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
	writeRigAnywhereCityToml(t, cityPath, toml)
	registerCityForRigResolution(t, gcHome, cityPath, "demo-city")

	setCwd(t, rigDir) // cwd lies INSIDE the registered rig's scope

	ctx, err := resolveCommandContext([]string{"ghost"}) // unknown name; rigDir/ghost does not exist
	if err != nil {
		t.Fatalf("resolveCommandContext(ghost) from inside a registered rig: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
	if ctx.RigName != "frontend" {
		t.Fatalf("RigName = %q, want frontend", ctx.RigName)
	}
}

// Regression (PR #3625 review, Error Handling & Resilience): the GC_CITY=<name>
// env lookup is intentionally best-effort. A corrupt/unreadable registry must
// NOT hard-error the ambient env path; it falls through (ok=false) so later
// GC_DIR/cwd discovery still runs. This deliberately differs from the loud
// corrupt-registry behavior of the positional arg and --city flag.
func TestResolveExplicitCityPathEnvNameBestEffortOnCorruptRegistry(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	// Corrupt registry: malformed TOML so the registry load fails to parse.
	if err := os.WriteFile(supervisor.RegistryPath(), []byte("[[city]\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", "broken")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")

	if got, ok := resolveExplicitCityPathEnv(); ok {
		t.Fatalf("resolveExplicitCityPathEnv() = (%q, true) on a corrupt registry; want (\"\", false) best-effort fall-through", got)
	}
}
