package main

import (
	"bytes"
	"fmt"
	"log"
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
	writeRigAnywhereCityToml(t, dir, toml)
	return canonicalTestPath(dir)
}

func writeRigAnywhereCityToml(t *testing.T, cityPath, toml string) {
	t.Helper()
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse(city.toml fixture): %v", err)
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, cityPath, cfg.Rigs); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	data, err := cfg.MarshalForWrite()
	if err != nil {
		t.Fatalf("MarshalForWrite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRigAnywhereLegacyOrderPack(t *testing.T, cityPath string) {
	t.Helper()
	legacyOrderDir := filepath.Join(cityPath, ".gc", "system", "packs", "maintenance", "formulas", "orders", "legacy-health")
	if err := os.MkdirAll(legacyOrderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "system", "packs", "maintenance", "pack.toml"), []byte(`[pack]
name = "maintenance"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyOrderDir, "order.toml"), []byte(`[order]
formula = "mol-legacy-health"
gate = "manual"
`), 0o644); err != nil {
		t.Fatal(err)
	}
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

func registerCityForRigResolution(t *testing.T, gcHome, cityPath, cityName string) {
	t.Helper()
	if err := registryAt(t, gcHome).Register(cityPath, cityName); err != nil {
		t.Fatal(err)
	}
}

func bindRigForRigResolution(t *testing.T, cityPath, cityName, rigName, rigDir string) {
	t.Helper()
	toml := fmt.Sprintf("[workspace]\nname = %q\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = %q\npath = %q\n", cityName, rigName, rigDir)
	writeRigAnywhereCityToml(t, cityPath, toml)
}

func registerRigBindingForResolution(t *testing.T, gcHome, cityPath, cityName, rigName, rigDir string) {
	t.Helper()
	bindRigForRigResolution(t, cityPath, cityName, rigName, rigDir)
	registerCityForRigResolution(t, gcHome, cityPath, cityName)
}

func assertNoGlobalRigEntries(t *testing.T, gcHome string) {
	t.Helper()
	rigs, err := registryAt(t, gcHome).ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 0 {
		t.Fatalf("global cities.toml rig entries = %#v, want none", rigs)
	}
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
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		writeRigAnywhereCityToml(t, cityPath, toml)

		cityFlag = cityPath
		setCwd(t, rigDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		assertSameTestPath(t, ctx.CityPath, cityPath)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "delta", "myapp", rigDir)

		rigFlag = "myapp"
		setCwd(t, t.TempDir()) // cwd somewhere unrelated

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "epsilon", "myapp", rigDir)

		rigFlag = rigDir // path, not name
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		assertSameTestPath(t, ctx.CityPath, cityPath)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "eta", "envapp", rigDir)

		t.Setenv("GC_RIG", "envapp")
		setCwd(t, t.TempDir())

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
		if ctx.RigName != "envapp" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "envapp")
		}
	})

	t.Run("gc_rig_env_only_fails_closed_on_binding_load_error", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "env-good")
		rigDir := filepath.Join(t.TempDir(), "envapp")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, cityPath, "env-good", "envapp", rigDir)

		badCity := setupCity(t, "env-bad")
		if err := os.WriteFile(config.SiteBindingPath(badCity), []byte("[[rig]\nname = \"broken\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, badCity, "env-bad")

		t.Setenv("GC_RIG", "envapp")
		setCwd(t, t.TempDir())

		_, err := resolveContext()
		if err == nil {
			t.Fatal("resolveContext should fail closed for GC_RIG when registered bindings cannot load")
		}
		if !strings.Contains(err.Error(), "loading registered city rig bindings") {
			t.Fatalf("error = %q, want registered binding load error", err)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "theta", "indexed-rig", rigDir)

		// cwd is deep inside the rig dir; should match via prefix.
		setCwd(t, subDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		writeRigAnywhereCityToml(t, cityPath, toml)

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
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
			bindRigForRigResolution(t, cp, filepath.Base(cp), "shared-rig", rigDir)
		}

		registerCityForRigResolution(t, gcHome, city1, "city1")
		registerCityForRigResolution(t, gcHome, city2, "city2")

		rigFlag = "shared-rig"
		setCwd(t, t.TempDir())

		_, err := resolveContext()
		if err == nil {
			t.Fatal("resolveContext() should fail for ambiguous rig")
		}
		if !strings.Contains(err.Error(), "multiple cities") {
			t.Errorf("error = %q, want message about multiple cities", err)
		}
	})

	t.Run("registered_rig_cwd_ambiguous_falls_through", func(t *testing.T) {
		resetFlags(t)
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		// Create a city for the walk-up fallback.
		cityPath := setupCity(t, "lambda")
		rigDir := filepath.Join(cityPath, "rigs", "ambig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		registerRigBindingForResolution(t, gcHome, cityPath, "lambda", "ambig", rigDir)
		otherCity := setupCity(t, "lambda-other")
		bindRigForRigResolution(t, otherCity, "lambda-other", "ambig", rigDir)
		registerCityForRigResolution(t, gcHome, otherCity, "lambda-other")

		setCwd(t, rigDir)

		// Should fall through from ambiguous registered rig bindings to walk-up.
		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		assertSameTestPath(t, ctx.CityPath, flagCity)
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
		writeRigAnywhereCityToml(t, cityPath, toml)

		t.Setenv("GC_CITY", cityPath)
		setCwd(t, rigDir)

		ctx, err := resolveContext()
		if err != nil {
			t.Fatalf("resolveContext() error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "custom-name", "", false, false, &stdout, &stderr)
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
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", false, false, &stdout, &stderr)
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

	t.Run("same_name_allowed_in_different_cities", func(t *testing.T) {
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
		code := doRigAdd(fsys.OSFS{}, city1, rigDir1, nil, "shared-name", "", false, false, &stdout1, &stderr1)
		if code != 0 {
			t.Fatalf("first doRigAdd = %d, stderr: %s", code, stderr1.String())
		}

		assertNoGlobalRigEntries(t, gcHome)

		// Rig names are city-local in Phase A, so a second city can use the
		// same name without creating a machine-global rig conflict.
		city2 := setupCity(t, "city-two")
		rigDir2 := filepath.Join(t.TempDir(), "rig2")
		if err := os.MkdirAll(rigDir2, 0o755); err != nil {
			t.Fatal(err)
		}
		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, city2, rigDir2, nil, "shared-name", "", false, false, &stdout2, &stderr2)
		if code != 0 {
			t.Fatalf("second doRigAdd = %d, stderr: %s", code, stderr2.String())
		}
		if strings.Contains(stderr2.String(), "global registry") {
			t.Fatalf("doRigAdd should not warn about machine-wide rig registration: %s", stderr2.String())
		}
		assertNoGlobalRigEntries(t, gcHome)
	})
}

// ===========================================================================
// 3. gc rig add site binding sync
// ===========================================================================

func TestRigAnywhere_RigAddSiteBindingSync(t *testing.T) {
	t.Run("add_writes_site_binding_not_cities_toml_rigs", func(t *testing.T) {
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
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", false, false, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("doRigAdd = %d, stderr: %s", code, stderr.String())
		}

		assertNoGlobalRigEntries(t, gcHome)
		binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(binding.Rigs) != 1 {
			t.Fatalf("site binding rig count = %d, want 1", len(binding.Rigs))
		}
		if binding.Rigs[0].Name != "sync-rig" {
			t.Fatalf("site binding rig name = %q, want sync-rig", binding.Rigs[0].Name)
		}
		assertSameTestPath(t, binding.Rigs[0].Path, rigDir)
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
		writeRigAnywhereCityToml(t, cityPath, toml)

		var stdout1, stderr1 bytes.Buffer
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", false, false, &stdout1, &stderr1)
		if code != 0 {
			t.Fatalf("first doRigAdd = %d, stderr: %s", code, stderr1.String())
		}

		// Re-add should succeed without duplicates.
		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", false, false, &stdout2, &stderr2)
		if code != 0 {
			t.Fatalf("re-add doRigAdd = %d, stderr: %s", code, stderr2.String())
		}

		assertNoGlobalRigEntries(t, gcHome)
		binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, r := range binding.Rigs {
			if r.Name == "idem-rig" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected 1 site binding entry, got %d", count)
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
		code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", false, false, &stdout, &stderr)
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
		code := doRigAdd(fsys.OSFS{}, city1, rigDir, nil, "", "", false, false, &stdout1, &stderr1)
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
		writeRigAnywhereCityToml(t, city2, toml2)

		var stdout2, stderr2 bytes.Buffer
		code = doRigAdd(fsys.OSFS{}, city2, rigDir, nil, "", "", false, false, &stdout2, &stderr2)
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
// 5. gc rig remove
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
		writeRigAnywhereCityToml(t, cityPath, toml)

		registerCityForRigResolution(t, gcHome, cityPath, "rm-city")

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

	t.Run("removes_from_site_binding_without_cities_toml_rig_entries", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityPath := setupCity(t, "solo-city")
		rigDir := filepath.Join(t.TempDir(), "solo-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		toml := "[workspace]\nname = \"solo-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"solo-rig\"\npath = \"" + rigDir + "\"\n"
		writeRigAnywhereCityToml(t, cityPath, toml)

		cityFlag = cityPath
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("solo-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(binding.Rigs) != 0 {
			t.Fatalf("site binding rigs = %#v, want none", binding.Rigs)
		}
		assertNoGlobalRigEntries(t, gcHome)
	})

	t.Run("does_not_scan_unrelated_registered_cities_on_remove", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)
		resetFlags(t)

		cityPath := setupCity(t, "quiet-remove")
		rigDir := filepath.Join(t.TempDir(), "quiet-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml := "[workspace]\nname = \"quiet-remove\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"quiet-rig\"\npath = \"" + rigDir + "\"\n"
		writeRigAnywhereCityToml(t, cityPath, toml)

		unrelatedCity := setupCity(t, "unrelated-noisy")
		unrelatedTOML := `[workspace]
name = "unrelated-noisy"
includes = ["missing-pack"]

[[agent]]
name = "mayor"

[packs.missing-pack]
source = "https://example.com/missing.git"
ref = "main"
path = "packs/missing"
`
		writeRigAnywhereCityToml(t, unrelatedCity, unrelatedTOML)
		writeRigAnywhereLegacyOrderPack(t, unrelatedCity)

		reg := registryAt(t, gcHome)
		if err := reg.Register(cityPath, "quiet-remove"); err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(unrelatedCity, "unrelated-noisy"); err != nil {
			t.Fatal(err)
		}

		var logs bytes.Buffer
		oldWriter := log.Writer()
		oldFlags := log.Flags()
		oldPrefix := log.Prefix()
		log.SetOutput(&logs)
		log.SetFlags(0)
		log.SetPrefix("")
		t.Cleanup(func() {
			log.SetOutput(oldWriter)
			log.SetFlags(oldFlags)
			log.SetPrefix(oldPrefix)
		})

		cityFlag = cityPath
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("quiet-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}
		if strings.Contains(logs.String(), "deprecated order path") {
			t.Fatalf("cmdRigRemove emitted unrelated order migration warning:\n%s", logs.String())
		}
		if strings.Contains(logs.String(), "missing-pack") {
			t.Fatalf("cmdRigRemove scanned unrelated registered city:\n%s", logs.String())
		}
		if !strings.Contains(stdout.String(), "Removed rig 'quiet-rig'") {
			t.Fatalf("stdout = %q, want removal confirmation", stdout.String())
		}
		assertNoGlobalRigEntries(t, gcHome)
	})

	t.Run("shared_rig_remove_only_updates_current_city_site_binding", func(t *testing.T) {
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
			writeRigAnywhereCityToml(t, cp, toml)
		}

		cityFlag = cityA
		var stdout, stderr bytes.Buffer
		code := cmdRigRemove("shared-rig", &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdRigRemove = %d, stderr: %s", code, stderr.String())
		}

		bindingA, err := config.LoadSiteBinding(fsys.OSFS{}, cityA)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindingA.Rigs) != 0 {
			t.Fatalf("city A site binding rigs = %#v, want none", bindingA.Rigs)
		}
		bindingB, err := config.LoadSiteBinding(fsys.OSFS{}, cityB)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindingB.Rigs) != 1 {
			t.Fatalf("city B site binding rigs = %#v, want one", bindingB.Rigs)
		}
		assertSameTestPath(t, bindingB.Rigs[0].Path, rigDir)
		assertNoGlobalRigEntries(t, gcHome)
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
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
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
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
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
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
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

	t.Run("rig_by_name_with_single_binding", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "ctx-city")
		rigDir := filepath.Join(t.TempDir(), "ctx-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		registerRigBindingForResolution(t, gcHome, cityPath, "ctx-city", "ctx-rig", rigDir)

		ctx, err := resolveRigToContext("ctx-rig")
		if err != nil {
			t.Fatalf("resolveRigToContext error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
		if ctx.RigName != "ctx-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "ctx-rig")
		}
	})

	t.Run("rig_by_name_fails_closed_when_registered_city_binding_errors", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		goodCity := setupCity(t, "good-city")
		rigDir := filepath.Join(t.TempDir(), "ctx-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, goodCity, "good-city", "ctx-rig", rigDir)

		badCity := setupCity(t, "bad-city")
		if err := os.WriteFile(config.SiteBindingPath(badCity), []byte("[[rig]\nname = \"broken\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, badCity, "bad-city")

		_, err := resolveRigToContext("ctx-rig")
		if err == nil {
			t.Fatal("resolveRigToContext should fail when any registered city binding cannot load")
		}
		if !strings.Contains(err.Error(), "loading registered city rig bindings") {
			t.Fatalf("error = %q, want registered city binding load error", err)
		}
		if !strings.Contains(err.Error(), "bad-city") {
			t.Fatalf("error = %q, want bad city name", err)
		}
	})

	t.Run("rig_by_path_with_single_binding", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "path-city")
		rigDir := filepath.Join(t.TempDir(), "path-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		registerRigBindingForResolution(t, gcHome, cityPath, "path-city", "path-rig", rigDir)

		ctx, err := resolveRigToContext(rigDir)
		if err != nil {
			t.Fatalf("resolveRigToContext error: %v", err)
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
		if ctx.RigName != "path-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "path-rig")
		}
	})

	t.Run("legacy_city_toml_path_is_not_registered_binding", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "legacy-city")
		rigDir := filepath.Join(t.TempDir(), "legacy-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		legacy := fmt.Sprintf("[workspace]\nname = \"legacy-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"legacy-rig\"\npath = %q\n", rigDir)
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(config.SiteBindingPath(cityPath)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, cityPath, "legacy-city")

		_, err := resolveRigToContext("legacy-rig")
		if err == nil {
			t.Fatal("resolveRigToContext should not treat legacy city.toml paths as registered bindings")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Fatalf("error = %q, want not registered", err)
		}
	})

	t.Run("path_argument_fails_closed_on_binding_load_error", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "path-good")
		rigDir := filepath.Join(t.TempDir(), "path-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, cityPath, "path-good", "path-rig", rigDir)

		badCity := setupCity(t, "path-bad")
		if err := os.WriteFile(config.SiteBindingPath(badCity), []byte("[[rig]\nname = \"broken\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, badCity, "path-bad")

		_, err := resolveContextFromPath(rigDir)
		if err == nil {
			t.Fatal("resolveContextFromPath should fail closed when registered bindings cannot load")
		}
		if !strings.Contains(err.Error(), "loading registered city rig bindings") {
			t.Fatalf("error = %q, want registered binding load error", err)
		}
	})

	// Regression: gc stop (and other commands that scan registered rig
	// bindings) must not abort when a sibling city's directory has been
	// deleted out from under the registry. Resolution still succeeds on
	// the healthy target and registeredRigBindingsByPath reports the
	// stale entry as structured data so only explicit-rig-resolution
	// callers (not opportunistic probes) need to warn about it.
	t.Run("stale_sibling_directory_is_skipped_with_warning", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		goodCity := setupCity(t, "stale-sibling-good")
		rigDir := filepath.Join(t.TempDir(), "stale-sibling-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, goodCity, "stale-sibling-good", "stale-sibling-rig", rigDir)

		// Register a second city, then delete its directory to simulate
		// "gc stop ~/my-city" after the sibling city was rm -rf'd.
		staleDir := filepath.Join(t.TempDir(), "vanished-city")
		if err := os.MkdirAll(filepath.Join(staleDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "city.toml"),
			[]byte("[workspace]\nname = \"stale-sibling-bad\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, staleDir, "stale-sibling-bad")
		if err := os.RemoveAll(staleDir); err != nil {
			t.Fatal(err)
		}

		ctx, err := resolveContextFromPath(rigDir)
		if err != nil {
			t.Fatalf("resolveContextFromPath error: %v (want success with stale sibling skipped)", err)
		}
		assertSameTestPath(t, ctx.CityPath, goodCity)
		if ctx.RigName != "stale-sibling-rig" {
			t.Errorf("RigName = %q, want %q", ctx.RigName, "stale-sibling-rig")
		}

		// registeredRigBindingsByPath returns stale entries as structured
		// data; callers decide whether to emit a user-facing warning. This
		// asserts the diagnostic is available without coupling the test to
		// a particular stderr routing scheme.
		_, stale, err := registeredRigBindingsByPath(rigDir, true)
		if err != nil {
			t.Fatalf("registeredRigBindingsByPath error: %v", err)
		}
		if len(stale) == 0 {
			t.Fatal("expected a stale-registered-city entry, got none")
		}
		var found bool
		for _, s := range stale {
			if strings.Contains(s.Label, "stale-sibling-bad") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("stale = %+v, want an entry mentioning stale-sibling-bad", stale)
		}

		// The helper renders the structured list to a command's stderr.
		var warnings bytes.Buffer
		emitStaleRegisteredCityWarnings(&warnings, stale)
		warn := warnings.String()
		if !strings.Contains(warn, "stale-sibling-bad") {
			t.Errorf("warning = %q, want it to mention the stale city name", warn)
		}
		if !strings.Contains(warn, "city.toml missing") {
			t.Errorf("warning = %q, want it to explain city.toml is missing", warn)
		}
		if !strings.Contains(warn, filepath.Join(staleDir, "city.toml")) {
			t.Errorf("warning = %q, want it to mention the missing city.toml path", warn)
		}
	})

	// Regression: the stale-entry check handles ENOENT from the config-load
	// path itself. A registered city whose directory exists but whose city.toml
	// is missing must still be skipped rather than abort the resolver.
	t.Run("stale_sibling_city_toml_missing_hits_load_path", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		goodCity := setupCity(t, "load-path-good")
		rigDir := filepath.Join(t.TempDir(), "load-path-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, goodCity, "load-path-good", "load-path-rig", rigDir)

		// Register a second city whose directory exists but whose
		// city.toml was never created. The load path (not a Stat
		// pre-check) has to handle ENOENT here.
		emptyDir := filepath.Join(t.TempDir(), "empty-city")
		if err := os.MkdirAll(filepath.Join(emptyDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, emptyDir, "empty-city")

		ctx, err := resolveContextFromPath(rigDir)
		if err != nil {
			t.Fatalf("resolveContextFromPath error: %v (want success with ENOENT on load path)", err)
		}
		assertSameTestPath(t, ctx.CityPath, goodCity)

		_, stale, err := registeredRigBindingsByPath(rigDir, true)
		if err != nil {
			t.Fatalf("registeredRigBindingsByPath error: %v", err)
		}
		var found bool
		for _, s := range stale {
			if strings.Contains(s.Label, "empty-city") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("stale = %+v, want an entry mentioning empty-city", stale)
		}
	})

	t.Run("registered_city_with_missing_include_fails_closed_not_stale", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		goodCity := setupCity(t, "missing-include-good")
		rigDir := filepath.Join(t.TempDir(), "missing-include-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, goodCity, "missing-include-good", "missing-include-rig", rigDir)

		brokenCity := setupCity(t, "missing-include-broken")
		if err := os.WriteFile(filepath.Join(brokenCity, "city.toml"), []byte(`
include = ["missing.toml"]

[workspace]
name = "missing-include-broken"

[[agent]]
name = "missing-include-agent"
`), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, brokenCity, "missing-include-broken")

		_, stale, err := registeredRigBindingsByPath(rigDir, true)
		if err == nil {
			t.Fatal("registeredRigBindingsByPath should fail closed on missing include")
		}
		if !strings.Contains(err.Error(), "loading registered city rig bindings") {
			t.Fatalf("error = %q, want registered binding load error", err)
		}
		if !strings.Contains(err.Error(), "missing.toml") {
			t.Fatalf("error = %q, want missing include path", err)
		}
		for _, s := range stale {
			if strings.Contains(s.Label, "missing-include-broken") {
				t.Fatalf("stale = %+v, missing include must not be reported as stale", stale)
			}
		}
	})

	t.Run("path_lookup_error_preserves_stale_entries", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		goodCity := setupCity(t, "path-stale-error-good")
		rigDir := filepath.Join(t.TempDir(), "path-stale-error-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, goodCity, "path-stale-error-good", "path-stale-error-rig", rigDir)

		staleDir := filepath.Join(t.TempDir(), "path-stale-error-vanished")
		if err := os.MkdirAll(filepath.Join(staleDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "city.toml"), []byte("[workspace]\nname = \"path-stale-error-vanished\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, staleDir, "path-stale-error-vanished")
		if err := os.RemoveAll(staleDir); err != nil {
			t.Fatal(err)
		}

		badCity := setupCity(t, "path-stale-error-bad")
		if err := os.WriteFile(config.SiteBindingPath(badCity), []byte("[[rig]\nname = \"broken\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, badCity, "path-stale-error-bad")

		_, stale, err := registeredRigBindingsByPath(rigDir, true)
		if err == nil {
			t.Fatal("registeredRigBindingsByPath should fail closed on the malformed site binding")
		}
		var found bool
		for _, s := range stale {
			if strings.Contains(s.Label, "path-stale-error-vanished") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("stale = %+v, want vanished city preserved on error", stale)
		}
	})

	// Regression: emitStaleRegisteredCityWarnings dedupes by Label so a
	// command that invokes registeredRigBindings twice (e.g.
	// resolveRigToContext tries both name and path lookups) emits each
	// stale entry at most once.
	t.Run("emit_stale_warnings_deduplicates_by_label", func(t *testing.T) {
		stale := []staleRegisteredCity{
			{Label: "city-a", Path: "/tmp/a"},
			{Label: "city-b", Path: "/tmp/b"},
			{Label: "city-a", Path: "/tmp/a"}, // duplicate from a second scan
		}
		var out bytes.Buffer
		emitStaleRegisteredCityWarnings(&out, stale)
		got := out.String()
		if strings.Count(got, "city-a") != 1 {
			t.Errorf("city-a should appear once, got %d in %q", strings.Count(got, "city-a"), got)
		}
		if strings.Count(got, "city-b") != 1 {
			t.Errorf("city-b should appear once, got %d in %q", strings.Count(got, "city-b"), got)
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
			bindRigForRigResolution(t, cp, filepath.Base(cp), "ambig-rig", rigDir)
		}

		registerCityForRigResolution(t, gcHome, city1, "city1")
		registerCityForRigResolution(t, gcHome, city2, "city2")

		_, err := resolveRigToContext("ambig-rig")
		if err == nil {
			t.Fatal("should fail for ambiguous rig")
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, "multiple cities") {
			t.Errorf("error = %q, want 'multiple cities'", errMsg)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "lookup-city", "lookup-rig", rigDir)

		ctx, ok := lookupRigFromCwd(rigDir)
		if !ok {
			t.Fatal("expected match")
		}
		assertSameTestPath(t, ctx.CityPath, cityPath)
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

		registerRigBindingForResolution(t, gcHome, cityPath, "sub-city", "sub-rig", rigDir)

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

		cityA := setupCity(t, "ambig-cwd-a")
		cityB := setupCity(t, "ambig-cwd-b")
		bindRigForRigResolution(t, cityA, "ambig-cwd-a", "ambig-cwd-rig", rigDir)
		bindRigForRigResolution(t, cityB, "ambig-cwd-b", "ambig-cwd-rig", rigDir)
		registerCityForRigResolution(t, gcHome, cityA, "ambig-cwd-a")
		registerCityForRigResolution(t, gcHome, cityB, "ambig-cwd-b")

		_, ok := lookupRigFromCwd(rigDir)
		if ok {
			t.Error("expected false for ambiguous rig binding")
		}
	})

	t.Run("load_error_with_match_returns_false", func(t *testing.T) {
		gcHome := t.TempDir()
		t.Setenv("GC_HOME", gcHome)

		cityPath := setupCity(t, "lookup-good")
		rigDir := filepath.Join(t.TempDir(), "lookup-rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registerRigBindingForResolution(t, gcHome, cityPath, "lookup-good", "lookup-rig", rigDir)

		badCity := setupCity(t, "lookup-bad")
		if err := os.WriteFile(config.SiteBindingPath(badCity), []byte("[[rig]\nname = \"broken\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		registerCityForRigResolution(t, gcHome, badCity, "lookup-bad")

		_, ok := lookupRigFromCwd(rigDir)
		if ok {
			t.Fatal("lookupRigFromCwd should not choose a match when another registered city cannot load")
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
		writeRigAnywhereCityToml(t, cityPath, toml)

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
		writeRigAnywhereCityToml(t, cityPath, toml)

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
		writeRigAnywhereCityToml(t, cityPath, toml)

		got := rigFromCwdDir(cityPath, rigDir)
		if got != "relrig" {
			t.Errorf("rigFromCwdDir with relative path = %q, want %q", got, "relrig")
		}
	})

	t.Run("matches_symlink_alias_of_rig_path", func(t *testing.T) {
		cityPath := setupCity(t, "cwd-symlink")
		rigDir, aliasRigDir := makeRigSymlinkAliasFixture(t)
		toml := "[workspace]\nname = \"cwd-symlink\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"aliasrig\"\npath = \"" + rigDir + "\"\n"
		writeRigAnywhereCityToml(t, cityPath, toml)

		got := rigFromCwdDir(cityPath, filepath.Join(aliasRigDir, "src"))
		if got != "aliasrig" {
			t.Errorf("rigFromCwdDir via symlink alias = %q, want %q", got, "aliasrig")
		}
	})
}
