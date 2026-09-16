package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stubRemovedGCExecutable makes the invoking executable an absolute path that no
// longer exists: the residual real trigger for the fail-closed resolver. A
// Homebrew or Nix upgrade that removes the old Cellar/store directory
// /proc/self/exe resolved through leaves a still-running supervisor exactly here.
func stubRemovedGCExecutable(t *testing.T) string {
	t.Helper()
	removed := filepath.Join(t.TempDir(), "bin", "gc")
	previous := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return removed, nil }
	t.Cleanup(func() { resolveInvokingExecutable = previous })
	return removed
}

// TestLegacyBdRunnersSurviveARemovedGCExecutable pins the legacy half of the
// split. main never set GC_BIN for gc's own bd invocations, so an upgrade that
// unlinked the running binary's path left GC_BIN stale and everything else
// working. Refusing instead takes down the running supervisor's reconciler,
// store opens, health, recover and its own SIGTERM shutdown — leaving the
// managed Dolt server unstopped — until the process restarts.
func TestLegacyBdRunnersSurviveARemovedGCExecutable(t *testing.T) {
	removed := stubRemovedGCExecutable(t)
	if _, err := resolveBdInvokingGCBinary(); err == nil {
		t.Fatal("fixture did not make the strict resolver fail")
	}

	env := map[string]string{"BEADS_DIR": "/tmp/x/.beads"}
	pinBdGCEnvironmentBestEffort(env)
	if env["GC_BIN"] != removed {
		t.Fatalf("GC_BIN = %q, want the un-canonicalized absolute path %q", env["GC_BIN"], removed)
	}

	city := t.TempDir()
	runner := bdCommandRunnerWithManagedRetryErr(city, func(dir string) (map[string]string, error) {
		return map[string]string{"BEADS_DIR": filepath.Join(dir, ".beads")}, nil
	})
	if runner == nil {
		t.Fatal("bdCommandRunnerWithManagedRetryErr returned no runner")
	}
	// The runner builds its env lazily, so drive it and require that the refusal
	// is not the reason it fails. A missing `true` binary would be a fixture
	// problem, not the behavior under test.
	if _, err := runner(city, "true"); err != nil && strings.Contains(err.Error(), "canonicalize invoking gc executable") {
		t.Fatalf("legacy bd runner refused over an uncanonicalizable gc: %v", err)
	}
}

// TestLoadBearingGCBinPinsStayStrict is the other half: where bd re-invokes gc
// through a hook, an unverifiable GC_BIN is a refusal, not a warning.
func TestLoadBearingGCBinPinsStayStrict(t *testing.T) {
	stubRemovedGCExecutable(t)

	t.Run("bd store bridge", func(t *testing.T) {
		env := map[string]string{}
		if err := pinBdGCEnvironment(env); err == nil {
			t.Fatal("pinBdGCEnvironment accepted an uncanonicalizable gc executable")
		}
		if _, ok := env["GC_BIN"]; ok {
			t.Fatalf("a refused pin still wrote GC_BIN = %q", env["GC_BIN"])
		}
	})

	t.Run("provider-owned lifecycle env", func(t *testing.T) {
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"owned\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
			Database:     "dolt",
			Backend:      "dolt",
			DoltMode:     "proxied-server",
			DoltDatabase: "hq",
		}); err != nil {
			t.Fatal(err)
		}
		owned, err := cityScopeProviderOwned(city)
		if err != nil || !owned {
			t.Fatalf("fixture is not provider-owned: (%t, %v)", owned, err)
		}
		previous := resolveProviderLifecycleGCBinary
		resolveProviderLifecycleGCBinary = resolveBdInvokingGCBinary
		t.Cleanup(func() { resolveProviderLifecycleGCBinary = previous })

		if _, err := providerLifecycleProcessEnvFromBase(city, "exec:"+gcBeadsBdScriptPath(city), nil); err == nil {
			t.Fatal("provider-owned lifecycle env accepted an uncanonicalizable gc executable")
		}
	})
}
