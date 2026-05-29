package gchome

import (
	"path/filepath"
	"testing"
)

func TestDefaultUsesGCHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	if got := Default(); got != dir {
		t.Fatalf("Default() = %q, want %q", got, dir)
	}
}

func TestRegistryPathsUseHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "gc")

	if got, want := RegistriesPath(home), filepath.Join(home, "registries.toml"); got != want {
		t.Fatalf("RegistriesPath() = %q, want %q", got, want)
	}
	if got, want := RegistryCacheRoot(home), filepath.Join(home, "registry-cache"); got != want {
		t.Fatalf("RegistryCacheRoot() = %q, want %q", got, want)
	}
}
