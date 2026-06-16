package gchome

import (
	"os"
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

// TestDefaultAvoidsSharedTempFallback guards #3506: when GC_HOME is unset and
// the user home cannot be resolved, Default() must not hand back the shared
// os.TempDir()/.gc path. That path is world-writable and shared across every
// process and user on the host, so concurrent processes clobber each other's
// state and unrelated city scans pick it up as a real city.
func TestDefaultAvoidsSharedTempFallback(t *testing.T) {
	t.Setenv("GC_HOME", "")
	t.Setenv("HOME", "") // forces os.UserHomeDir() to fail on unix

	got := Default()

	if shared := filepath.Join(os.TempDir(), ".gc"); got == shared {
		t.Fatalf("Default() = %q, want a process-isolated path, not the shared %q", got, shared)
	}
	// Must never be empty: callers join the result into a path (e.g.
	// filepath.Join(home, "registries.toml")), so "" silently becomes a
	// CWD-relative path and writes state to the wrong place instead of failing.
	if got == "" {
		t.Fatal("Default() returned an empty path; callers would write state to a CWD-relative path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Default() = %q, want an absolute process-isolated path", got)
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
