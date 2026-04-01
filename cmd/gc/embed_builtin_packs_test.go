package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestMaterializeBuiltinPacks(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	// Verify bd pack.toml exists.
	bdToml := filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "pack.toml")
	if _, err := os.Stat(bdToml); err != nil {
		t.Errorf("bd pack.toml missing: %v", err)
	}

	// Verify dolt pack.toml exists.
	doltToml := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "pack.toml")
	if _, err := os.Stat(doltToml); err != nil {
		t.Errorf("dolt pack.toml missing: %v", err)
	}

	// Verify doctor scripts are executable.
	for _, script := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "doctor", "check-bd.sh"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt.sh"),
	} {
		info, err := os.Stat(script)
		if err != nil {
			t.Errorf("script missing: %v", err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("script %s not executable: mode %v", filepath.Base(script), info.Mode())
		}
	}

	// Verify dolt commands are executable.
	cmds := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "commands")
	entries, err := os.ReadDir(cmds)
	if err != nil {
		t.Fatalf("reading dolt commands dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("dolt commands dir is empty")
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Errorf("stat %s: %v", e.Name(), err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("dolt command %s not executable: mode %v", e.Name(), info.Mode())
		}
	}

	// Verify dolt scripts/runtime.sh exists and is executable.
	runtimeSh := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "scripts", "runtime.sh")
	if info, err := os.Stat(runtimeSh); err != nil {
		t.Errorf("dolt scripts/runtime.sh missing: %v", err)
	} else if info.Mode()&0o111 == 0 {
		t.Errorf("dolt scripts/runtime.sh not executable: mode %v", info.Mode())
	}

	// Verify formulas exist.
	formulasDir := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas")
	if _, err := os.Stat(formulasDir); err != nil {
		t.Errorf("dolt formulas dir missing: %v", err)
	}

	// Verify TOML files are not executable.
	info, err := os.Stat(bdToml)
	if err == nil && info.Mode()&0o111 != 0 {
		t.Errorf("pack.toml should not be executable: mode %v", info.Mode())
	}
}

func TestMaterializeBuiltinPacks_Idempotent(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}
	// Second call should succeed without error.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	// Files should still exist.
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "pack.toml")); err != nil {
		t.Error("bd pack.toml missing after second call")
	}
}

func TestBuiltinPackIncludes_DefaultProvider(t *testing.T) {
	dir := t.TempDir()

	// Materialize packs first.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Default provider (empty) → should include maintenance, bd, dolt.
	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries", includes)
	}

	systemRoot := filepath.Join(dir, citylayout.SystemPacksRoot)
	wantMaintenance := filepath.Join(systemRoot, "maintenance")
	wantBd := filepath.Join(systemRoot, "bd")
	wantDolt := filepath.Join(systemRoot, "dolt")

	if includes[0] != wantMaintenance {
		t.Errorf("includes[0] = %q, want %q", includes[0], wantMaintenance)
	}
	if includes[1] != wantBd {
		t.Errorf("includes[1] = %q, want %q", includes[1], wantBd)
	}
	if includes[2] != wantDolt {
		t.Errorf("includes[2] = %q, want %q", includes[2], wantDolt)
	}
}

func TestBuiltinPackIncludes_ExplicitBd(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Write a city.toml with provider = "bd".
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries (maintenance + bd + dolt)", includes)
	}

	if got := filepath.Base(includes[0]); got != "maintenance" {
		t.Errorf("includes[0] base = %q, want maintenance", got)
	}
	if got := filepath.Base(includes[1]); got != "bd" {
		t.Errorf("includes[1] base = %q, want bd", got)
	}
	if got := filepath.Base(includes[2]); got != "dolt" {
		t.Errorf("includes[2] base = %q, want dolt", got)
	}
}

func TestBuiltinPackIncludes_NonBdProvider(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Write a city.toml with a non-bd provider.
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	// Only maintenance, no bd/dolt.
	if len(includes) != 1 {
		t.Fatalf("builtinPackIncludes() = %v, want 1 entry (maintenance only)", includes)
	}

	if got := filepath.Base(includes[0]); got != "maintenance" {
		t.Errorf("includes[0] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_EnvOverride(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// GC_BEADS env var overrides city.toml provider.
	t.Setenv("GC_BEADS", "file")
	includes := builtinPackIncludes(dir)

	// Only maintenance, no bd/dolt.
	if len(includes) != 1 {
		t.Fatalf("builtinPackIncludes() = %v, want 1 entry when GC_BEADS=file", includes)
	}

	if got := filepath.Base(includes[0]); got != "maintenance" {
		t.Errorf("includes[0] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_NotMaterialized(t *testing.T) {
	dir := t.TempDir()

	// Don't materialize — should return empty.
	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 0 {
		t.Errorf("builtinPackIncludes() = %v, want empty when packs not materialized", includes)
	}
}

func TestBuiltinPackIncludes_PathsPointToSystemPacks(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	systemRoot := filepath.Join(dir, citylayout.SystemPacksRoot)
	for _, inc := range includes {
		// Every include path must be under .gc/system/packs/.
		rel, err := filepath.Rel(systemRoot, inc)
		if err != nil {
			t.Errorf("path %q not relative to system root: %v", inc, err)
			continue
		}
		if rel == ".." || len(rel) > 0 && rel[0] == '.' {
			t.Errorf("path %q escapes system packs root (rel=%q)", inc, rel)
		}
		// Each include path should be a directory with pack.toml inside.
		if _, err := os.Stat(filepath.Join(inc, "pack.toml")); err != nil {
			t.Errorf("pack.toml missing in %q: %v", inc, err)
		}
	}
}

func TestBuiltinPackIncludes_AlwaysIncludesMaintenance(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Even with non-bd provider, maintenance must be present.
	t.Setenv("GC_BEADS", "file")
	includes := builtinPackIncludes(dir)

	found := false
	for _, inc := range includes {
		if filepath.Base(inc) == "maintenance" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("maintenance pack not found in includes: %v", includes)
	}

	// Also with bd provider.
	t.Setenv("GC_BEADS", "bd")
	includes = builtinPackIncludes(dir)

	found = false
	for _, inc := range includes {
		if filepath.Base(inc) == "maintenance" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("maintenance pack not found in bd includes: %v", includes)
	}
}

func TestMaterializeGastownPacks(t *testing.T) {
	dir := t.TempDir()

	// MaterializeGastownPacks is a no-op shim — verify it returns nil.
	if err := MaterializeGastownPacks(dir); err != nil {
		t.Fatalf("MaterializeGastownPacks() error: %v", err)
	}
}

func TestMaterializeGastownPacks_Idempotent(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeGastownPacks(dir); err != nil {
		t.Fatal(err)
	}
	// Second call should succeed without error.
	if err := MaterializeGastownPacks(dir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
}
