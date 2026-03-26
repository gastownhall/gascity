package main

import (
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

func TestInjectBuiltinPacks_BdProvider(t *testing.T) {
	dir := t.TempDir()

	// Materialize packs first.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	// Default provider (empty) → should inject.
	injectBuiltinPacks(cfg, dir)

	if len(cfg.PackDirs) != 2 {
		t.Fatalf("PackDirs = %v, want 2 entries", cfg.PackDirs)
	}
	if got := filepath.Base(cfg.PackDirs[0]); got != "bd" {
		t.Errorf("PackDirs[0] = %q, want bd", got)
	}
	if got := filepath.Base(cfg.PackDirs[1]); got != "dolt" {
		t.Errorf("PackDirs[1] = %q, want dolt", got)
	}
}

func TestInjectBuiltinPacks_ExplicitBd(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.Beads.Provider = "bd"
	injectBuiltinPacks(cfg, dir)

	if len(cfg.PackDirs) != 2 {
		t.Fatalf("PackDirs = %v, want 2 entries", cfg.PackDirs)
	}
}

func TestInjectBuiltinPacks_FileProvider(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.Beads.Provider = "file"
	injectBuiltinPacks(cfg, dir)

	if len(cfg.PackDirs) != 0 {
		t.Errorf("PackDirs = %v, want empty for file provider", cfg.PackDirs)
	}
}

func TestInjectBuiltinPacks_EnvOverride(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{}
	injectBuiltinPacks(cfg, dir)

	if len(cfg.PackDirs) != 0 {
		t.Errorf("PackDirs = %v, want empty when GC_BEADS=file", cfg.PackDirs)
	}
}

func TestInjectBuiltinPacks_AlreadyLoaded(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Create a user-supplied bd pack in a separate directory.
	userBd := filepath.Join(dir, "user-packs", "my-bd")
	if err := os.MkdirAll(userBd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userBd, "pack.toml"), []byte("[pack]\nname = \"bd\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	cfg.PackDirs = []string{userBd}
	injectBuiltinPacks(cfg, dir)

	// Should not inject — user bd pack takes precedence.
	if len(cfg.PackDirs) != 1 {
		t.Errorf("PackDirs = %v, want only user bd pack", cfg.PackDirs)
	}
}

func TestInjectBuiltinPacks_NotMaterialized(t *testing.T) {
	dir := t.TempDir()

	// Don't materialize — injection should be a no-op.
	cfg := &config.City{}
	injectBuiltinPacks(cfg, dir)

	if len(cfg.PackDirs) != 0 {
		t.Errorf("PackDirs = %v, want empty when packs not materialized", cfg.PackDirs)
	}
}

func TestReadPackName(t *testing.T) {
	dir := t.TempDir()

	// Write a pack.toml with a name.
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test-pack\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := readPackName(dir); got != "test-pack" {
		t.Errorf("readPackName() = %q, want %q", got, "test-pack")
	}

	// Non-existent dir → empty.
	if got := readPackName(filepath.Join(dir, "nope")); got != "" {
		t.Errorf("readPackName(nonexistent) = %q, want empty", got)
	}
}

func TestMaterializeGastownPacks(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeGastownPacks(dir); err != nil {
		t.Fatalf("MaterializeGastownPacks() error: %v", err)
	}

	// Verify gastown pack.toml exists.
	gastownToml := filepath.Join(dir, "packs", "gastown", "pack.toml")
	if _, err := os.Stat(gastownToml); err != nil {
		t.Errorf("gastown pack.toml missing: %v", err)
	}

	// Verify maintenance pack.toml exists.
	maintenanceToml := filepath.Join(dir, "packs", "maintenance", "pack.toml")
	if _, err := os.Stat(maintenanceToml); err != nil {
		t.Errorf("maintenance pack.toml missing: %v", err)
	}

	// Verify gastown scripts are executable.
	scriptsDir := filepath.Join(dir, "packs", "gastown", "scripts")
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		t.Fatalf("reading gastown scripts dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("gastown scripts dir is empty")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Errorf("stat %s: %v", e.Name(), err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("gastown script %s not executable: mode %v", e.Name(), info.Mode())
		}
	}

	// Verify gastown prompts exist.
	promptsDir := filepath.Join(dir, "packs", "gastown", "prompts")
	if _, err := os.Stat(promptsDir); err != nil {
		t.Errorf("gastown prompts dir missing: %v", err)
	}

	// Verify default overlay payloads are materialized for both packs.
	for _, overlayPath := range []string{
		filepath.Join(dir, "packs", "gastown", "overlays", "default", ".claude", "settings.json"),
		filepath.Join(dir, "packs", "maintenance", "overlays", "default", ".claude", "settings.json"),
	} {
		data, err := os.ReadFile(overlayPath)
		if err != nil {
			t.Errorf("default overlay settings missing at %s: %v", overlayPath, err)
			continue
		}
		if !strings.Contains(string(data), `gc handoff \"context cycle\"`) {
			t.Errorf("default overlay %s missing context-cycle hook", overlayPath)
		}
	}

	// Verify TOML files are not executable.
	info, err := os.Stat(gastownToml)
	if err == nil && info.Mode()&0o111 != 0 {
		t.Errorf("pack.toml should not be executable: mode %v", info.Mode())
	}

	// Verify pack name is correct.
	if got := readPackName(filepath.Join(dir, "packs", "gastown")); got != "gastown" {
		t.Errorf("readPackName(gastown) = %q, want %q", got, "gastown")
	}
	if got := readPackName(filepath.Join(dir, "packs", "maintenance")); got != "maintenance" {
		t.Errorf("readPackName(maintenance) = %q, want %q", got, "maintenance")
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

	// Files should still exist.
	if _, err := os.Stat(filepath.Join(dir, "packs", "gastown", "pack.toml")); err != nil {
		t.Error("gastown pack.toml missing after second call")
	}
}
