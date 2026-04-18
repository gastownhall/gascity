package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
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
		filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "doctor", "check-bd", "run.sh"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh"),
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

	// Verify dolt commands have executable run.sh entrypoints.
	cmds := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "commands")
	entries, err := os.ReadDir(cmds)
	if err != nil {
		t.Fatalf("reading dolt commands dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("dolt commands dir is empty")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run := filepath.Join(cmds, e.Name(), "run.sh")
		info, err := os.Stat(run)
		if err != nil {
			t.Errorf("dolt command %s/run.sh missing: %v", e.Name(), err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("dolt command %s/run.sh not executable: mode %v", e.Name(), info.Mode())
		}
	}

	// Verify dolt assets/scripts/runtime.sh exists and is executable.
	runtimeSh := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "assets", "scripts", "runtime.sh")
	if info, err := os.Stat(runtimeSh); err != nil {
		t.Errorf("dolt assets/scripts/runtime.sh missing: %v", err)
	} else if info.Mode()&0o111 == 0 {
		t.Errorf("dolt assets/scripts/runtime.sh not executable: mode %v", info.Mode())
	}

	// Verify formulas exist.
	formulasDir := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas")
	if _, err := os.Stat(formulasDir); err != nil {
		t.Errorf("dolt formulas dir missing: %v", err)
	}

	// Verify embedded order files are materialized alongside formulas.
	for _, order := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "gate-sweep.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "mol-dog-jsonl.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "mol-dog-reaper.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "orders", "dolt-health.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "orders", "digest-generate.toml"),
	} {
		if _, err := os.Stat(order); err != nil {
			t.Errorf("embedded order missing: %v", err)
		}
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

func TestMaterializedBuiltinPackOrdersScanWithoutWarnings(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{
				filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "formulas"),
				filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas"),
				filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "formulas"),
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(dir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if strings.Contains(stderr.String(), "deprecated order path") {
		t.Fatalf("unexpected deprecation warning while scanning materialized builtin packs:\n%s", stderr.String())
	}

	names := make(map[string]bool, len(aa))
	for _, a := range aa {
		names[a.Name] = true
	}
	for _, want := range []string{"gate-sweep", "dolt-health", "digest-generate"} {
		if !names[want] {
			t.Fatalf("missing bundled order %q; got %v", want, names)
		}
	}
}

func TestMaterializeBuiltinPacks_PrunesLegacyOrderDirs(t *testing.T) {
	dir := t.TempDir()

	legacyPaths := []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "formulas", "orders", "gate-sweep", "order.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas", "orders", "dolt-health", "order.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "formulas", "orders", "digest-generate", "order.toml"),
	}
	for _, path := range legacyPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir legacy path: %v", err)
		}
		if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
			t.Fatalf("write legacy path: %v", err)
		}
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	for _, path := range legacyPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy order path still exists: %s", path)
		}
	}

	for _, path := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "gate-sweep.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "orders", "dolt-health.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "orders", "digest-generate.toml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("flat order missing after materialization: %v", err)
		}
	}
}

func TestBuiltinPackIncludes_DefaultProvider(t *testing.T) {
	dir := t.TempDir()

	// Materialize packs first.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Default provider (empty) → should include core, maintenance, and bd.
	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries", includes)
	}

	systemRoot := filepath.Join(dir, citylayout.SystemPacksRoot)
	wantCore := filepath.Join(systemRoot, "core")
	wantMaintenance := filepath.Join(systemRoot, "maintenance")
	wantBd := filepath.Join(systemRoot, "bd")

	if includes[0] != wantCore {
		t.Errorf("includes[0] = %q, want %q", includes[0], wantCore)
	}
	if includes[1] != wantMaintenance {
		t.Errorf("includes[1] = %q, want %q", includes[1], wantMaintenance)
	}
	if includes[2] != wantBd {
		t.Errorf("includes[2] = %q, want %q", includes[2], wantBd)
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
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries (core + maintenance + bd)", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Errorf("includes[2] base = %q, want bd", got)
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

	// Core and maintenance are always auto-included; bd/dolt are gated
	// on a bd-compatible provider.
	if len(includes) != 2 {
		t.Fatalf("builtinPackIncludes() = %v, want 2 entries (core + maintenance)", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_ExecGcBeadsBdOverrideIncludesBdAndDolt(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	includes := builtinPackIncludes(dir)
	// core + maintenance + bd + dolt = 4 entries. Core and maintenance are
	// always auto-included; bd and dolt arrive via the exec-override path.
	if len(includes) != 4 {
		t.Fatalf("builtinPackIncludes() = %v, want 4 entries when GC_BEADS=exec:gc-beads-bd", includes)
	}
	if got := filepath.Base(includes[0]); got != "core" {
		t.Fatalf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Fatalf("includes[2] base = %q, want bd", got)
	}
	if got := filepath.Base(includes[3]); got != "dolt" {
		t.Fatalf("includes[3] base = %q, want dolt", got)
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

	// Core and maintenance are always auto-included; bd/dolt are gated on
	// a bd-compatible provider.
	if len(includes) != 2 {
		t.Fatalf("builtinPackIncludes() = %v, want 2 entries when GC_BEADS=file", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_ManagedExecEnvStillIncludesBd(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(dir))
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want core + maintenance + bd", includes)
	}
	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Errorf("includes[2] base = %q, want bd", got)
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
