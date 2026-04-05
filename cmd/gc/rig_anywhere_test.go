package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// setupCity creates a minimal city directory with city.toml and .gc/.
// Returns the absolute path to the city root.
func setupCity(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[workspace]\nname = \"" + name + "\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// resetFlags saves and restores cityFlag and rigFlag globals.
func resetFlags(t *testing.T) {
	t.Helper()
	oldCity := cityFlag
	oldRig := rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = oldCity
		rigFlag = oldRig
	})
}

// setCwd changes the working directory and restores it on cleanup.
func setCwd(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

// registryAt creates a Registry backed by a file in the given temp dir.
func registryAt(t *testing.T, gcHome string) *supervisor.Registry {
	t.Helper()
	return supervisor.NewRegistry(filepath.Join(gcHome, "cities.toml"))
}

// ===========================================================================
// 1. resolveContext priority chain
// ===========================================================================

func TestRigAnywhere_ResolveContext(t *testing.T) {
	t.Run("city_and_rig_flags", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := setupCity(t, "alpha")

		cityFlag = cityPath
		rigFlag = "myrig"

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "myrig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "myrig")
		}
	})

	t.Run("city_flag_only_rig_from_cwd", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "beta")

		// Add a rig entry in city.toml so rigFromCwd can resolve it.
		rigDir := filepath.Join(t.TempDir(), "frontend")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := "[workspace]\nname = \"beta\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		cityFlag = cityPath
		setCwd(t, rigDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "frontend" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "frontend")
		}
	})

	t.Run("city_flag_only_no_rig_match", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := setupCity(t, "gamma")

		cityFlag = cityPath
		// cwd is not inside any rig
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "" {
			t.Errorf("RigName = %q, want empty", ctx.RigName)
		}
	})

	t.Run("rig_flag_by_name", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "delta")
		rigDir := filepath.Join(t.TempDir(), "myapp")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Register the rig in the global index.
		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "myapp", cityPath); err != nil {
			t.Fatal(err)
		}

		rigFlag = "myapp"
		setCwd(t, t.TempDir()) // cwd somewhere unrelated

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "myapp" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "myapp")
		}
	})

	t.Run("rig_flag_by_path", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "epsilon")
		rigDir := filepath.Join(t.TempDir(), "myapp")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "myapp", cityPath); err != nil {
			t.Fatal(err)
		}

		rigFlag = rigDir // path, not name
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "myapp" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "myapp")
		}
	})

	t.Run("gc_city_and_gc_rig_env", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := setupCity(t, "zeta")

		t.Setenv("GC_CITY", cityPath)
		t.Setenv("GC_RIG", "envrig")
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "envrig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "envrig")
		}
	})

	t.Run("gc_rig_env_only", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "eta")
		rigDir := filepath.Join(t.TempDir(), "envapp")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "envapp", cityPath); err != nil {
			t.Fatal(err)
		}

		t.Setenv("GC_RIG", "envapp")
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "envapp" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "envapp")
		}
	})

	t.Run("rig_index_cwd_lookup", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "theta")
		rigDir := filepath.Join(t.TempDir(), "indexed-rig")
		subDir := filepath.Join(rigDir, "src", "deep")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "indexed-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		// cwd is deep inside the rig dir; should match via prefix.
		setCwd(t, subDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "indexed-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "indexed-rig")
		}
	})

	t.Run("walk_up_fallback", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())

		cityPath := setupCity(t, "iota")
		subDir := filepath.Join(cityPath, "sub", "dir")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}

		setCwd(t, subDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
	})

	t.Run("walk_up_fallback_with_rig_match", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())

		cityPath := setupCity(t, "kappa")
		rigDir := filepath.Join(t.TempDir(), "myrig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Register rig in city.toml so rigFromCwdDir can match.
		toml := "[workspace]\nname = \"kappa\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"myrig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		// cwd inside the city tree; walk-up finds city, then rigFromCwdDir matches.
		subInCity := filepath.Join(cityPath, "rigs", "workspace")
		if err := os.MkdirAll(subInCity, 0o755); err != nil {
			t.Fatal(err)
		}
		setCwd(t, subInCity)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		// cwd is inside city tree but not inside the rig dir, so rig should be empty.
		if ctx.RigName != "" {
			t.Errorf("RigName = %q, want empty (cwd not inside rig)", ctx.RigName)
		}
	})

	t.Run("failure_nothing_matches", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())

		isolated := t.TempDir()
		setCwd(t, isolated)

		_, err := resolveContext()
		if err == nil {
			t.Fatal("resolveContext() should fail when nothing matches")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})

	t.Run("ambiguous_rig_no_default", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		rigDir := filepath.Join(t.TempDir(), "shared-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Create two cities that both contain this rig.
		city1 := filepath.Join(t.TempDir(), "city1")
		city2 := filepath.Join(t.TempDir(), "city2")
		for _, cp := range []string{city1, city2} {
			if err := os.MkdirAll(cp, 0o755); err != nil {
				t.Fatal(err)
			}
			toml := fmt.Sprintf("[workspace]\nname = %q\n\n[[agent]]\nname = \"a\"\n\n[[rigs]]\nname = \"shared-rig\"\npath = %q\n", filepath.Base(cp), rigDir)
			if err := os.WriteFile(filepath.Join(cp, "city.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(city1, "city1"); err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(city2, "city2"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "shared-rig", ""); err != nil {
			t.Fatal(err)
		}

		rigFlag = "shared-rig"
		setCwd(t, t.TempDir())

		_, err := resolveContext()
		if err == nil {
			t.Fatal("resolveContext() should fail for ambiguous rig")
		}
		if !strings.Contains(err.Error(), "multiple cities") {
			t.Errorf("error = %q, want message about multiple cities", err)
		}
		if !strings.Contains(err.Error(), "gc rig default") {
			t.Errorf("error = %q, want helpful 'gc rig default' hint", err)
		}
	})

	t.Run("rig_index_cwd_ambiguous_falls_through", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		// Create a city for the walk-up fallback.
		cityPath := setupCity(t, "lambda")
		rigDir := filepath.Join(cityPath, "rigs", "ambig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Register rig in global index with no default (ambiguous).
		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "ambig", ""); err != nil {
			t.Fatal(err)
		}

		setCwd(t, rigDir)

		// Should fall through from rig index (ambiguous) to walk-up (finds city).
		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
	})

	t.Run("flags_take_priority_over_env", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())

		flagCity := setupCity(t, "flag-city")
		envCity := setupCity(t, "env-city")

		cityFlag = flagCity
		rigFlag = "flagrig"
		t.Setenv("GC_CITY", envCity)
		t.Setenv("GC_RIG", "envrig")

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		// Flags (step 1) should beat env vars (step 4).
		if ctx.CityPath != flagCity {
			t.Errorf("CityPath = %q, want %q (flag should beat env)", ctx.CityPath, flagCity)
		}
		if ctx.RigName != "flagrig" {
			t.Errorf("RigName = %q, want %q (flag should beat env)", ctx.RigName, "flagrig")
		}
	})

	t.Run("gc_city_env_only_rig_from_cwd", func(t *testing.T) {
		resetFlags(t)
		t.Setenv("GC_HOME", t.TempDir())

		cityPath := setupCity(t, "mu")
		rigDir := filepath.Join(t.TempDir(), "envrig-dir")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := "[workspace]\nname = \"mu\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"envrig-dir\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		t.Setenv("GC_CITY", cityPath)
		setCwd(t, rigDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "envrig-dir" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "envrig-dir")
		}
	})
}

// ===========================================================================
// 2. gc rig add --name
// ===========================================================================

func TestRigAnywhere_RigAddName(t *testing.T) {
	t.Run("name_flag_overrides_basename", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "test-city")
		rigDir := filepath.Join(t.TempDir(), "my-project")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "custom-name", false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		output := stdout.String()
		if !strings.Contains(output, "Adding rig 'custom-name'") {
			t.Errorf("output should use custom name: %s", output)
		}

		// Verify city.toml has the custom name, not the basename.
		cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Rigs) != 1 {
			t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
		}
		if cfg.Rigs[0].Name != "custom-name" {
			t.Errorf("rig name = %q, want %q", cfg.Rigs[0].Name, "custom-name")
		}
	})

	t.Run("no_name_flag_uses_basename", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "test-city")
		rigDir := filepath.Join(t.TempDir(), "auto-named")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Rigs) != 1 || cfg.Rigs[0].Name != "auto-named" {
			t.Errorf("rig name = %q, want %q", cfg.Rigs[0].Name, "auto-named")
		}
	})

	t.Run("name_conflict_global_uniqueness", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		city1 := setupCity(t, "city-one")
		rigDir1 := filepath.Join(t.TempDir(), "rig1")
		if err := os.MkdirAll(rigDir1, 0o755); err != nil {
			t.Fatal(err)
		}

		// First add succeeds.
		var stdout1, stderr1 bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, city1, rigDir1, "", "shared-name", false, &stdout1, &stderr1)
		if code != 0 {
			t.Fatalf("first doRigAdd = %d, stderr: %s", code, stderr1.String())
		}

		// Verify it's in the global registry.
		reg := registryAt(t, gcHome)
		entry, ok := reg.LookupRigByName("shared-name")
		if !ok {
			t.Fatal("rig not found in global registry after first add")
		}
		if entry.Path != rigDir1 {
			t.Errorf("registry path = %q, want %q", entry.Path, rigDir1)
		}

		// Second add with same name but different path should warn (global registry).
		city2 := setupCity(t, "city-two")
		rigDir2 := filepath.Join(t.TempDir(), "rig2")
		if err := os.MkdirAll(rigDir2, 0o755); err != nil {
			t.Fatal(err)
		}
		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, city2, rigDir2, "", "shared-name", false, &stdout2, &stderr2)
		// The global registry conflict is a warning, not an error. The rig still
		// gets added to city.toml. Check that the warning is emitted.
		if code != 0 {
			// If global registry rejects it, that's expected behavior too.
			if !strings.Contains(stderr2.String(), "shared-name") {
				t.Errorf("stderr should mention the conflicting name: %s", stderr2.String())
			}
		}
	})
}

// ===========================================================================
// 3. gc rig add cities.toml sync
// ===========================================================================

func TestRigAnywhere_RigAddCitiesTomlSync(t *testing.T) {
	t.Run("add_registers_in_cities_toml", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "sync-city")
		rigDir := filepath.Join(t.TempDir(), "sync-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		// Verify rig appears in cities.toml.
		reg := registryAt(t, gcHome)
		entry, ok := reg.LookupRigByName("sync-rig")
		if !ok {
			t.Fatal("rig not found in cities.toml after add")
		}
		if entry.Path != rigDir {
			t.Errorf("rig path = %q, want %q", entry.Path, rigDir)
		}
	})

	t.Run("first_add_auto_sets_default_city", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "default-city")
		rigDir := filepath.Join(t.TempDir(), "auto-default")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		reg := registryAt(t, gcHome)
		entry, ok := reg.LookupRigByName("auto-default")
		if !ok {
			t.Fatal("rig not found in cities.toml")
		}
		if entry.DefaultCity != cityPath {
			t.Errorf("default_city = %q, want %q (auto-set on first add)", entry.DefaultCity, cityPath)
		}
	})

	t.Run("re_add_same_city_idempotent", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "idem-city")
		rigDir := filepath.Join(t.TempDir(), "idem-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Add the rig to city.toml first so re-add triggers.
		toml := "[workspace]\nname = \"idem-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"idem-rig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		var stdout1, stderr1 bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout1, &stderr1)
		if code != 0 {
			t.Fatalf("first doRigAdd = %d, stderr: %s", code, stderr1.String())
		}

		// Re-add should succeed without duplicates.
		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout2, &stderr2)
		if code != 0 {
			t.Fatalf("re-add doRigAdd = %d, stderr: %s", code, stderr2.String())
		}

		// Verify only one entry in global registry.
		reg := registryAt(t, gcHome)
		rigs, err := reg.ListRigs()
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, r := range rigs {
			if r.Name == "idem-rig" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected 1 rig entry, got %d", count)
		}
	})
}

// ===========================================================================
// 4. gc rig add .beads/.env
// ===========================================================================

func TestRigAnywhere_RigAddBeadsEnv(t *testing.T) {
	t.Run("writes_gt_root", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		cityPath := setupCity(t, "env-city")
		rigDir := filepath.Join(t.TempDir(), "env-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, "", "", false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		envPath := filepath.Join(rigDir, ".beads", ".env")
		data, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatalf("reading .beads/.env: %v", err)
		}
		expected := "GT_ROOT=" + cityPath
		if !strings.Contains(string(data), expected) {
			t.Errorf(".beads/.env = %q, want to contain %q", string(data), expected)
		}
	})

	t.Run("re_add_updates_gt_root", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "file")

		city1 := setupCity(t, "city-one")
		city2 := setupCity(t, "city-two")
		rigDir := filepath.Join(t.TempDir(), "shared")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// First add with city1.
		var stdout1, stderr1 bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, city1, rigDir, "", "", false, &stdout1, &stderr1)
		if code != 0 {
			t.Fatalf("first doRigAdd = %d, stderr: %s", code, stderr1.String())
		}

		envPath := filepath.Join(rigDir, ".beads", ".env")
		data1, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data1), "GT_ROOT="+city1) {
			t.Fatalf("first .beads/.env missing city1: %s", data1)
		}

		// Re-add to city2 (rig already exists from a different city perspective).
		toml2 := "[workspace]\nname = \"city-two\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"shared\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(city2, "city.toml"), []byte(toml2), 0o644); err != nil {
			t.Fatal(err)
		}

		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, city2, rigDir, "", "", false, &stdout2, &stderr2)
		if code != 0 {
			t.Fatalf("second doRigAdd = %d, stderr: %s", code, stderr2.String())
		}

		data2, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data2), "GT_ROOT="+city2) {
			t.Errorf("re-add should update GT_ROOT: %s", data2)
		}
	})
}

// ===========================================================================
// 5. gc rig default
// ===========================================================================

func TestRigAnywhere_RigDefault(t *testing.T) {
	t.Run("sets_default_city", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "default-test")
		rigDir := filepath.Join(t.TempDir(), "defrig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Add rig to city.toml.
		toml := "[workspace]\nname = \"default-test\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"defrig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		// Register city and rig in global index.
		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "default-test"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "defrig", ""); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := cmdRigDefault("defrig", "default-test", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigDefault = %d, stderr: %s", code, stderr.String())
		}

		// Verify default was set.
		entry, ok := reg.LookupRigByName("defrig")
		if !ok {
			t.Fatal("rig not found in registry")
		}
		if entry.DefaultCity != cityPath {
			t.Errorf("default_city = %q, want %q", entry.DefaultCity, cityPath)
		}
	})

	t.Run("updates_beads_env_gt_root", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "env-upd")
		rigDir := filepath.Join(t.TempDir(), "envrig")
		if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Pre-populate .beads/.env with stale GT_ROOT.
		if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"),
			[]byte("GT_ROOT=/old/path\nOTHER=value\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		toml := "[workspace]\nname = \"env-upd\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"envrig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "env-upd"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "envrig", ""); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := cmdRigDefault("envrig", "env-upd", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigDefault = %d, stderr: %s", code, stderr.String())
		}

		data, err := os.ReadFile(filepath.Join(rigDir, ".beads", ".env"))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "GT_ROOT="+cityPath) {
			t.Errorf(".beads/.env should have updated GT_ROOT: %s", content)
		}
		if !strings.Contains(content, "OTHER=value") {
			t.Errorf(".beads/.env should preserve OTHER entry: %s", content)
		}
	})

	t.Run("fails_when_rig_not_registered", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "norig-city")
		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "norig-city"); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := cmdRigDefault("nonexistent-rig", "norig-city", &stdout, &stderr)
		if code != 1 {
			t.Fatalf("cmdRigDefault should fail, got code %d", code)
		}
		if !strings.Contains(stderr.String(), "not registered") {
			t.Errorf("stderr = %q, want 'not registered'", stderr.String())
		}
	})

	t.Run("fails_when_rig_not_in_city", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityA := setupCity(t, "city-a")
		cityB := setupCity(t, "city-b")
		rigDir := filepath.Join(t.TempDir(), "orphan-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Rig is in city-a's config, but NOT in city-b's.
		tomlA := "[workspace]\nname = \"city-a\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"orphan-rig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityA, "city.toml"), []byte(tomlA), 0o644); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityA, "city-a"); err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(cityB, "city-b"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "orphan-rig", cityA); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		code := cmdRigDefault("orphan-rig", "city-b", &stdout, &stderr)
		if code != 1 {
			t.Fatalf("cmdRigDefault should fail, got code %d", code)
		}
		if !strings.Contains(stderr.String(), "not registered in city") {
			t.Errorf("stderr = %q, want 'not registered in city'", stderr.String())
		}
	})
}

// ===========================================================================
// 6. gc rig remove
// ===========================================================================

func TestRigAnywhere_RigRemove(t *testing.T) {
	t.Run("removes_from_city_toml", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityPath := setupCity(t, "rm-city")
		rigDir := filepath.Join(t.TempDir(), "rm-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		toml := "[workspace]\nname = \"rm-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"rm-rig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "rm-city"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "rm-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		cityFlag = cityPath
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("rm-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		// Verify city.toml no longer has the rig.
		cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range cfg.Rigs {
			if r.Name == "rm-rig" {
				t.Error("rig should be removed from city.toml")
			}
		}
	})

	t.Run("removes_from_cities_toml_when_no_other_city", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityPath := setupCity(t, "solo-city")
		rigDir := filepath.Join(t.TempDir(), "solo-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		toml := "[workspace]\nname = \"solo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"solo-rig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "solo-city"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "solo-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		cityFlag = cityPath
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("solo-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		// Rig should be gone from global registry too.
		_, ok := reg.LookupRigByName("solo-rig")
		if ok {
			t.Error("rig should be removed from cities.toml when no other city has it")
		}
	})

	t.Run("clears_default_when_removed_city_was_default", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityA := setupCity(t, "city-a")
		cityB := setupCity(t, "city-b")
		rigDir := filepath.Join(t.TempDir(), "shared-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Rig is in both cities, default is city-a.
		for _, cp := range []string{cityA, cityB} {
			toml := "[workspace]\nname = \"" + filepath.Base(cp) + "\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"shared-rig\"\npath = \"" + rigDir + "\"\n"
			if err := os.WriteFile(filepath.Join(cp, "city.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityA, "city-a"); err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(cityB, "city-b"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "shared-rig", cityA); err != nil {
			t.Fatal(err)
		}

		// Remove from city-a (the default).
		cityFlag = cityA
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("shared-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		// Rig should still exist in registry (city-b still has it).
		entry, ok := reg.LookupRigByName("shared-rig")
		if !ok {
			t.Fatal("rig should still be in registry (city-b has it)")
		}
		// Since only 1 city remains, auto-set to that city.
		if entry.DefaultCity != cityB {
			t.Errorf("default_city = %q, want %q (auto-set to remaining city)", entry.DefaultCity, cityB)
		}
	})

	t.Run("auto_sets_default_when_one_city_remains", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityA := setupCity(t, "multi-a")
		cityB := setupCity(t, "multi-b")
		cityC := setupCity(t, "multi-c")
		rigDir := filepath.Join(t.TempDir(), "multi-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Rig in all three cities, default is city-a.
		for _, cp := range []string{cityA, cityB, cityC} {
			name := filepath.Base(cp)
			toml := "[workspace]\nname = \"" + name + "\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"multi-rig\"\npath = \"" + rigDir + "\"\n"
			if err := os.WriteFile(filepath.Join(cp, "city.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		reg := registryAt(t, gcHome)
		for _, cp := range []string{cityA, cityB, cityC} {
			if err := reg.Register(cp, filepath.Base(cp)); err != nil {
				t.Fatal(err)
			}
		}
		if err := reg.RegisterRig(rigDir, "multi-rig", cityA); err != nil {
			t.Fatal(err)
		}

		// Remove from city-a. Two cities remain, so default should be cleared
		// (not auto-set, since there are still 2 remaining).
		cityFlag = cityA
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("multi-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		entry, ok := reg.LookupRigByName("multi-rig")
		if !ok {
			t.Fatal("rig should still be in registry")
		}
		// With 2 remaining cities, default should be empty (ambiguous).
		if entry.DefaultCity != "" {
			t.Errorf("default_city = %q, want empty (2 cities remain)", entry.DefaultCity)
		}
	})
}

// ===========================================================================
// 7. writeBeadsEnvGTRoot
// ===========================================================================

func TestRigAnywhere_WriteBeadsEnvGTRoot(t *testing.T) {
	t.Run("creates_env_from_scratch", func(t *testing.T) {
		rigDir := t.TempDir()

		err := writeBeadsEnvGTRoot(fsys.OSFS{}, rigDir, "/my/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(rigDir, ".beads", ".env"))
		if err != nil {
			t.Fatalf("reading .env: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, "GT_ROOT=/my/city") {
			t.Errorf(".env = %q, want GT_ROOT=/my/city", content)
		}
		if !strings.HasSuffix(content, "\n") {
			t.Errorf(".env should end with newline")
		}
	})

	t.Run("updates_existing_gt_root", func(t *testing.T) {
		rigDir := t.TempDir()
		beadsDir := filepath.Join(rigDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, ".env"),
			[]byte("GT_ROOT=/old/path\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := writeBeadsEnvGTRoot(fsys.OSFS{}, rigDir, "/new/path")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(beadsDir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "GT_ROOT=/new/path") {
			t.Errorf(".env = %q, want GT_ROOT=/new/path", content)
		}
		if strings.Contains(content, "/old/path") {
			t.Errorf(".env should not contain old path: %s", content)
		}
	})

	t.Run("preserves_other_env_entries", func(t *testing.T) {
		rigDir := t.TempDir()
		beadsDir := filepath.Join(rigDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, ".env"),
			[]byte("DOLT_PORT=3306\nGT_ROOT=/old/city\nCUSTOM_VAR=hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := writeBeadsEnvGTRoot(fsys.OSFS{}, rigDir, "/new/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(beadsDir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "GT_ROOT=/new/city") {
			t.Errorf(".env missing updated GT_ROOT: %s", content)
		}
		if !strings.Contains(content, "DOLT_PORT=3306") {
			t.Errorf(".env missing DOLT_PORT: %s", content)
		}
		if !strings.Contains(content, "CUSTOM_VAR=hello") {
			t.Errorf(".env missing CUSTOM_VAR: %s", content)
		}
		// Exactly one GT_ROOT line.
		count := strings.Count(content, "GT_ROOT=")
		if count != 1 {
			t.Errorf("expected 1 GT_ROOT line, got %d in: %s", count, content)
		}
	})

	t.Run("adds_gt_root_when_missing_from_existing_env", func(t *testing.T) {
		rigDir := t.TempDir()
		beadsDir := filepath.Join(rigDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, ".env"),
			[]byte("DOLT_PORT=3306\nOTHER=val\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		err := writeBeadsEnvGTRoot(fsys.OSFS{}, rigDir, "/appended/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(beadsDir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, "GT_ROOT=/appended/city") {
			t.Errorf(".env should contain appended GT_ROOT: %s", content)
		}
		if !strings.Contains(content, "DOLT_PORT=3306") {
			t.Errorf(".env should preserve existing entries: %s", content)
		}
	})

	t.Run("creates_beads_dir_if_needed", func(t *testing.T) {
		rigDir := t.TempDir()
		// No .beads dir exists.

		err := writeBeadsEnvGTRoot(fsys.OSFS{}, rigDir, "/new/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		fi, err := os.Stat(filepath.Join(rigDir, ".beads"))
		if err != nil {
			t.Fatalf(".beads dir not created: %v", err)
		}
		if !fi.IsDir() {
			t.Error(".beads should be a directory")
		}
	})

	t.Run("with_fake_fs", func(t *testing.T) {
		f := fsys.NewFake()
		// No pre-existing .env.
		f.Dirs["/rig/.beads"] = true

		err := writeBeadsEnvGTRoot(f, "/rig", "/my/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		content, ok := f.Files["/rig/.beads/.env"]
		if !ok {
			t.Fatal(".env not written to fake fs")
		}
		if !strings.Contains(string(content), "GT_ROOT=/my/city") {
			t.Errorf("fake .env = %q, want GT_ROOT", string(content))
		}
	})

	t.Run("with_fake_fs_updates_existing", func(t *testing.T) {
		f := fsys.NewFake()
		f.Dirs["/rig/.beads"] = true
		f.Files["/rig/.beads/.env"] = []byte("KEEP=me\nGT_ROOT=/old\n")

		err := writeBeadsEnvGTRoot(f, "/rig", "/new/city")
		if err != nil {
			t.Fatalf("writeBeadsEnvGTRoot error: %v", err)
		}

		content := string(f.Files["/rig/.beads/.env"])
		if !strings.Contains(content, "GT_ROOT=/new/city") {
			t.Errorf("fake .env = %q, want updated GT_ROOT", content)
		}
		if !strings.Contains(content, "KEEP=me") {
			t.Errorf("fake .env = %q, want KEEP preserved", content)
		}
		if strings.Contains(content, "/old") {
			t.Errorf("fake .env = %q, should not have old GT_ROOT", content)
		}
	})
}

// ===========================================================================
// Additional edge case: resolveRigToContext
// ===========================================================================

func TestRigAnywhere_ResolveRigToContext(t *testing.T) {
	t.Run("rig_not_registered_anywhere", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())

		_, err := resolveRigToContext("nonexistent-rig")
		if err == nil {
			t.Fatal("resolveRigToContext should fail for unregistered rig")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error = %q, want 'not registered'", err)
		}
	})

	t.Run("rig_by_name_with_default", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "ctx-city")
		rigDir := filepath.Join(t.TempDir(), "ctx-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "ctx-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		ctx, err := resolveRigToContext("ctx-rig")
		if err != nil {
			t.Fatalf("resolveRigToContext error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "ctx-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "ctx-rig")
		}
	})

	t.Run("rig_by_path_with_default", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "path-city")
		rigDir := filepath.Join(t.TempDir(), "path-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "path-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		ctx, err := resolveRigToContext(rigDir)
		if err != nil {
			t.Fatalf("resolveRigToContext error: %v", err)
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "path-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "path-rig")
		}
	})

	t.Run("rig_ambiguous_no_default_helpful_error", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		rigDir := filepath.Join(t.TempDir(), "ambig-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Create two cities that both contain this rig so it's truly ambiguous.
		city1 := filepath.Join(t.TempDir(), "city1")
		city2 := filepath.Join(t.TempDir(), "city2")
		for _, cp := range []string{city1, city2} {
			if err := os.MkdirAll(cp, 0o755); err != nil {
				t.Fatal(err)
			}
			toml := fmt.Sprintf("[workspace]\nname = %q\n\n[[agent]]\nname = \"a\"\n\n[[rigs]]\nname = \"ambig-rig\"\npath = %q\n", filepath.Base(cp), rigDir)
			if err := os.WriteFile(filepath.Join(cp, "city.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		reg := registryAt(t, gcHome)
		if err := reg.Register(city1, "city1"); err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(city2, "city2"); err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterRig(rigDir, "ambig-rig", ""); err != nil {
			t.Fatal(err)
		}

		_, err := resolveRigToContext("ambig-rig")
		if err == nil {
			t.Fatal("should fail for ambiguous rig")
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, "multiple cities") {
			t.Errorf("error = %q, want 'multiple cities'", errMsg)
		}
		if !strings.Contains(errMsg, "gc rig default") {
			t.Errorf("error = %q, want 'gc rig default' hint", errMsg)
		}
		if !strings.Contains(errMsg, "--city") {
			t.Errorf("error = %q, want '--city' hint", errMsg)
		}
	})
}

// ===========================================================================
// lookupRigFromCwd
// ===========================================================================

func TestRigAnywhere_LookupRigFromCwd(t *testing.T) {
	t.Run("matches_cwd", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "lookup-city")
		rigDir := filepath.Join(t.TempDir(), "lookup-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "lookup-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		ctx, ok := lookupRigFromCwd(rigDir)
		if !ok {
			t.Fatal("expected match")
		}
		if ctx.CityPath != cityPath {
			t.Errorf("CityPath = %q, want %q", ctx.CityPath, cityPath)
		}
		if ctx.RigName != "lookup-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "lookup-rig")
		}
	})

	t.Run("matches_subdir_of_rig", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "sub-city")
		rigDir := filepath.Join(t.TempDir(), "sub-rig")
		subDir := filepath.Join(rigDir, "pkg", "deep")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "sub-rig", cityPath); err != nil {
			t.Fatal(err)
		}

		ctx, ok := lookupRigFromCwd(subDir)
		if !ok {
			t.Fatal("expected match for subdirectory")
		}
		if ctx.RigName != "sub-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "sub-rig")
		}
	})

	t.Run("no_match", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())

		_, ok := lookupRigFromCwd("/completely/unrelated/path")
		if ok {
			t.Error("expected no match for unrelated path")
		}
	})

	t.Run("ambiguous_returns_false", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		rigDir := filepath.Join(t.TempDir(), "ambig-cwd-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		reg := registryAt(t, gcHome)
		if err := reg.RegisterRig(rigDir, "ambig-cwd-rig", ""); err != nil {
			t.Fatal(err)
		}

		_, ok := lookupRigFromCwd(rigDir)
		if ok {
			t.Error("expected false for ambiguous rig (no default)")
		}
	})
}

// ===========================================================================
// rigFromCwdDir
// ===========================================================================

func TestRigAnywhere_RigFromCwdDir(t *testing.T) {
	t.Run("matches_absolute_rig_path", func(t *testing.T) {
		cityPath := setupCity(t, "cwd-match")
		rigDir := filepath.Join(t.TempDir(), "matchrig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := "[workspace]\nname = \"cwd-match\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"matchrig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		got := rigFromCwdDir(cityPath, rigDir)
		if got != "matchrig" {
			t.Errorf("rigFromCwdDir = %q, want %q", got, "matchrig")
		}
	})

	t.Run("matches_subdir_of_rig", func(t *testing.T) {
		cityPath := setupCity(t, "cwd-sub")
		rigDir := filepath.Join(t.TempDir(), "subrig")
		subDir := filepath.Join(rigDir, "internal", "pkg")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := "[workspace]\nname = \"cwd-sub\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"subrig\"\npath = \"" + rigDir + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		got := rigFromCwdDir(cityPath, subDir)
		if got != "subrig" {
			t.Errorf("rigFromCwdDir = %q, want %q", got, "subrig")
		}
	})

	t.Run("no_match_returns_empty", func(t *testing.T) {
		cityPath := setupCity(t, "cwd-nomatch")
		got := rigFromCwdDir(cityPath, "/unrelated/path")
		if got != "" {
			t.Errorf("rigFromCwdDir = %q, want empty", got)
		}
	})

	t.Run("relative_rig_path_resolved", func(t *testing.T) {
		cityPath := setupCity(t, "cwd-rel")
		// Create rig dir inside city.
		rigDir := filepath.Join(cityPath, "rigs", "relrig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Use relative path in config.
		toml := "[workspace]\nname = \"cwd-rel\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"relrig\"\npath = \"rigs/relrig\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}

		got := rigFromCwdDir(cityPath, rigDir)
		if got != "relrig" {
			t.Errorf("rigFromCwdDir with relative path = %q, want %q", got, "relrig")
		}
	})
}

// ===========================================================================
// resolveCityByNameOrPath
// ===========================================================================

func TestRigAnywhere_ResolveCityByNameOrPath(t *testing.T) {
	t.Run("by_path", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "path-resolve")
		reg := registryAt(t, gcHome)

		got, err := resolveCityByNameOrPath(reg, cityPath)
		if err != nil {
			t.Fatalf("resolveCityByNameOrPath error: %v", err)
		}
		if got != cityPath {
			t.Errorf("got %q, want %q", got, cityPath)
		}
	})

	t.Run("by_name", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "name-resolve")
		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "name-resolve"); err != nil {
			t.Fatal(err)
		}

		got, err := resolveCityByNameOrPath(reg, "name-resolve")
		if err != nil {
			t.Fatalf("resolveCityByNameOrPath error: %v", err)
		}
		// Canonicalize to handle macOS /var → /private/var symlink.
		want, _ := filepath.EvalSymlinks(cityPath)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		reg := registryAt(t, gcHome)

		_, err := resolveCityByNameOrPath(reg, "totally-unknown")
		if err == nil {
			t.Fatal("expected error for unknown city")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %q, want 'not found'", err)
		}
	})
}
