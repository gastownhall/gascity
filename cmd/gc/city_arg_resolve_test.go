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
	got, err := resolveCityRef("foo/bar", cityRefOpts{cmd: "gc test", allowNameFallback: true}, pathResolve)
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

	got, err := resolveCityRef("alpha", cityRefOpts{cmd: "gc test", allowNameFallback: true}, failingPathResolve(t))
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

	_, err := resolveCityRef("ghost", cityRefOpts{cmd: "gc test", allowNameFallback: true}, failingPathResolve(t))
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
	got, err := resolveCityRef("mycity", cityRefOpts{cmd: "gc test", allowNameFallback: true}, pathResolve)
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

	_, err := resolveCityRef("dup", cityRefOpts{cmd: "gc test", allowNameFallback: true}, failingPathResolve(t))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
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
	got, err := resolveCityRef("same", cityRefOpts{cmd: "gc test", allowNameFallback: true}, pathResolve)
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
	got, err := resolveCityRef("alpha", cityRefOpts{cmd: "gc register", allowNameFallback: false}, pathResolve)
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
	got, err := resolveCityRef("other", cityRefOpts{cmd: "gc stop", allowNameFallback: true}, walkUp)
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
