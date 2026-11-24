package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeMemoTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// TestInvocationConfigMemoIsOffOutsideAScope pins that a process which never
// opens a scope (the supervisor, the API server) reloads on every call, as it
// always has: an edit to city.toml is seen by the next load.
func TestInvocationConfigMemoIsOffOutsideAScope(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	before := composedCityConfigLoads.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"renamed\"\nprefix = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got != 2 {
		t.Fatalf("loads outside a scope = %d, want 2", got)
	}
	if cfg.Workspace.Name != "renamed" {
		t.Fatalf("second load outside a scope read %q, want the edited %q", cfg.Workspace.Name, "renamed")
	}
}

// TestInvocationConfigMemoSharesOneLoadInAScope pins the scope: one composed
// load per city, other option sets untouched, errors never remembered, and the
// scope closed by its end function.
func TestInvocationConfigMemoSharesOneLoadInAScope(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	missing := filepath.Join(t.TempDir(), "city.toml")

	end := beginInvocationConfigMemo()
	// The scope is process-global: a t.Fatal before the explicit end() below
	// must not leave it open for the rest of the package's tests. end is
	// idempotent, so the explicit call stays the one under test.
	t.Cleanup(end)
	innerEnd := beginInvocationConfigMemo() // nested begin reuses the open scope
	innerEnd()
	if currentInvocationConfigMemo() == nil {
		t.Fatal("a nested scope's end closed the outer scope")
	}

	before := composedCityConfigLoads.Load()
	first, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace.Name != second.Workspace.Name {
		t.Fatal("a second load in the scope did not answer the first load's config")
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("loads in a scope = %d, want 1", got)
	}

	// Another option set is not the one-shot shape: it loads.
	before = composedCityConfigLoads.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, advisoryLoad); err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("advisory loads in a scope = %d, want 1 (not memoized)", got)
	}

	// A failed load is not remembered.
	before = composedCityConfigLoads.Load()
	for i := 0; i < 2; i++ {
		if _, _, err := loadComposedCityConfig(fsys.OSFS{}, missing, invocationMemoLoadOptions); err == nil {
			t.Fatal("loading a missing city.toml succeeded")
		}
	}
	if got := composedCityConfigLoads.Load() - before; got != 2 {
		t.Fatalf("failed loads in a scope = %d, want 2 (errors are not memoized)", got)
	}

	end()
	if currentInvocationConfigMemo() != nil {
		t.Fatal("end did not close the scope")
	}
}

// TestInvocationConfigMemoHandsEachCallerItsOwnRigs: helpers resolve rig paths
// in place (resolveRigPaths) on the config they load. A memo hit must not hand
// the next helper a config another helper already rewrote: each caller gets
// its own top-level City and its own Rigs and Agents slices.
func TestInvocationConfigMemoHandsEachCallerItsOwnRigs(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"rigs/r\"\nprefix = \"rr\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	defer beginInvocationConfigMemo()()

	first, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rigs) != 1 || first.Rigs[0].Path != "rigs/r" {
		t.Fatalf("rigs = %+v, want one rig at rigs/r", first.Rigs)
	}
	resolveRigPaths(cityDir, first.Rigs)
	first.Workspace.Name = "mutated"

	second, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if second.Rigs[0].Path != "rigs/r" {
		t.Fatalf("second caller saw rig path %q, want the loaded rigs/r (another caller's in-place resolve leaked)", second.Rigs[0].Path)
	}
	if second.Workspace.Name != "demo" {
		t.Fatalf("second caller saw workspace %q, want demo", second.Workspace.Name)
	}
}

// TestInvocationConfigMemoHotCommandsStayMemoized pins the #7864 behavior the
// freshness checks must keep: while city.toml, its sources, packs.lock and
// site.toml are unchanged, a second composition and a second beads peek do
// no extra work, and a readiness deferral taken from that composition stays.
func TestInvocationConfigMemoHotCommandsStayMemoized(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	end := beginInvocationConfigMemo()
	t.Cleanup(end)

	before := composedCityConfigLoads.Load()
	peeksBefore := cityTOMLBeadsPeekDecodes.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	deferBuiltinReadinessForInvocation(cityDir)
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	if got := peekBeadsProvider(tomlPath); got != "bd" {
		t.Fatalf("provider = %q, want bd", got)
	}
	if got := peekBeadsProvider(tomlPath); got != "bd" {
		t.Fatalf("second peek provider = %q, want bd", got)
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("compositions with unchanged sources = %d, want 1", got)
	}
	if got := cityTOMLBeadsPeekDecodes.Load() - peeksBefore; got != 1 {
		t.Fatalf("beads peeks with an unchanged city.toml = %d, want 1", got)
	}
	if !builtinReadinessDeferredInInvocation(cityDir) {
		t.Fatal("unchanged sources retired the readiness deferral")
	}
}

// TestInvocationConfigMemoInvalidatesOnCityTOMLWrite pins that a city.toml
// write is visible to the next composition, beads peek, and readiness check
// in the same scope.
func TestInvocationConfigMemoInvalidatesOnCityTOMLWrite(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	end := beginInvocationConfigMemo()
	t.Cleanup(end)

	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	deferBuiltinReadinessForInvocation(cityDir)
	if got := peekBeadsProvider(tomlPath); got != "" {
		t.Fatalf("provider before the write = %q, want empty", got)
	}
	before := composedCityConfigLoads.Load()
	peeksBefore := cityTOMLBeadsPeekDecodes.Load()
	body := "[workspace]\nname = \"renamed\"\nprefix = \"demo\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(tomlPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(tomlPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Name != "renamed" {
		t.Fatalf("composition after the city.toml write read %q, want renamed", cfg.Workspace.Name)
	}
	if got := peekBeadsProvider(tomlPath); got != "file" {
		t.Fatalf("peek after the city.toml write = %q, want file", got)
	}
	if builtinReadinessDeferredInInvocation(cityDir) {
		t.Fatal("readiness stayed deferred after city.toml changed")
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("compositions after the write = %d, want 1", got)
	}
	if got := cityTOMLBeadsPeekDecodes.Load() - peeksBefore; got != 1 {
		t.Fatalf("peeks after the write = %d, want 1 (the remembered decode was stale)", got)
	}
}

// TestInvocationConfigMemoInvalidatesOnExternalSources pins the sources a
// composition depends on beyond city.toml itself: an included rig or pack
// file the load reported, packs.lock, and the rig bindings in .gc/site.toml.
func TestInvocationConfigMemoInvalidatesOnExternalSources(t *testing.T) {
	t.Run("included rig config", func(t *testing.T) {
		cityDir := t.TempDir()
		tomlPath := filepath.Join(cityDir, "city.toml")
		rigPath := filepath.Join(cityDir, "rig.toml")
		if err := os.WriteFile(tomlPath, []byte("include = [\"rig.toml\"]\n[workspace]\nname = \"demo\"\nprefix = \"demo\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rigPath, []byte("[[rigs]]\nname = \"alpha\"\npath = \"rigs/alpha\"\nprefix = \"al\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		end := beginInvocationConfigMemo()
		t.Cleanup(end)
		first, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Rigs) != 1 || first.Rigs[0].Name != "alpha" {
			t.Fatalf("rigs = %+v, want alpha", first.Rigs)
		}
		deferBuiltinReadinessForInvocation(cityDir)
		before := composedCityConfigLoads.Load()
		if err := os.WriteFile(rigPath, []byte("[[rigs]]\nname = \"beta\"\npath = \"rigs/beta\"\nprefix = \"be\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(rigPath, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		second, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Rigs) != 1 || second.Rigs[0].Name != "beta" {
			t.Fatalf("rigs after the include changed = %+v, want beta", second.Rigs)
		}
		if got := composedCityConfigLoads.Load() - before; got != 1 {
			t.Fatalf("compositions after the included rig config changed = %d, want 1", got)
		}
		if builtinReadinessDeferredInInvocation(cityDir) {
			t.Fatal("readiness stayed deferred after an included source changed")
		}
	})

	t.Run("packs.lock and site.toml", func(t *testing.T) {
		for _, rel := range []string{"packs.lock", filepath.Join(".gc", "site.toml")} {
			t.Run(rel, func(t *testing.T) {
				cityDir := writeMemoTestCity(t)
				tomlPath := filepath.Join(cityDir, "city.toml")
				end := beginInvocationConfigMemo()
				t.Cleanup(end)
				if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
					t.Fatal(err)
				}
				before := composedCityConfigLoads.Load()
				path := filepath.Join(cityDir, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("created = true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
					t.Fatal(err)
				}
				if got := composedCityConfigLoads.Load() - before; got != 1 {
					t.Fatalf("compositions after creating %s = %d, want 1", rel, got)
				}
			})
		}
	})
}

// TestInvocationConfigMemoWriteHookIgnoresUnchangedStat pins that the city.toml
// edit writer drops the memo even when the net size and mtime match what was
// loaded. Stat freshness cannot see that write.
func TestInvocationConfigMemoWriteHookIgnoresUnchangedStat(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	info, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	end := beginInvocationConfigMemo()
	t.Cleanup(end)
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace.Name = "edited"
	if err := writeCityConfigForEditFS(fsys.OSFS{}, tomlPath, cfg); err != nil {
		t.Fatal(err)
	}
	// Put every seeded stat back, including removing a site binding the
	// writer created. A memo that survived the writer would answer the
	// restored files without composing; the hook must force a new load.
	if err := os.Remove(filepath.Join(cityDir, ".gc", "site.toml")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tomlPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	before := composedCityConfigLoads.Load()
	loaded, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Name != "demo" {
		t.Fatalf("load after restoring the seeded file read %q, want demo", loaded.Workspace.Name)
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("compositions after the edit writer = %d, want 1 (the hook dropped the memo)", got)
	}
}

// TestInvocationConfigMemoPackRefreshAndImportInstall pins the two events
// that change files a composition may not have reported. The first builtin
// refresh drops the city; a second one keeps the composition taken after it.
// Import-install validation drops the memo before it loads.
func TestInvocationConfigMemoPackRefreshAndImportInstall(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	end := beginInvocationConfigMemo()
	t.Cleanup(end)

	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	deferBuiltinReadinessForInvocation(cityDir)
	if got := peekBeadsProvider(tomlPath); got != "" {
		t.Fatalf("provider = %q, want empty", got)
	}
	noteBuiltinPackRefresh(tomlPath)
	if builtinReadinessDeferredInInvocation(cityDir) {
		t.Fatal("the first pack refresh left the readiness deferral in place")
	}
	before := composedCityConfigLoads.Load()
	peeksBefore := cityTOMLBeadsPeekDecodes.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	if got := peekBeadsProvider(tomlPath); got != "" {
		t.Fatal(got)
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("compositions after the first refresh = %d, want 1", got)
	}
	if got := cityTOMLBeadsPeekDecodes.Load() - peeksBefore; got != 1 {
		t.Fatalf("peeks after the first refresh = %d, want 1", got)
	}

	noteBuiltinPackRefresh(tomlPath)
	before = composedCityConfigLoads.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got != 0 {
		t.Fatalf("compositions after a later refresh = %d, want 0", got)
	}

	invalidateInvocationConfigMemo()
	before = composedCityConfigLoads.Load()
	if err := validateComposedConfigAfterInstall(cityDir); err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got < 1 {
		t.Fatalf("import-install validation composed %d times, want at least 1 fresh load", got)
	}
}
