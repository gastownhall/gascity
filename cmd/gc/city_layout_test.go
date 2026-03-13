package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestMigrateLegacyContentPrunesEmptyLegacyDir(t *testing.T) {
	cityDir := t.TempDir()
	legacyDir := filepath.Join(cityDir, ".gc", "prompts")
	canonicalDir := filepath.Join(cityDir, "prompts")
	legacyFile := filepath.Join(legacyDir, "mayor.md")
	canonicalFile := filepath.Join(canonicalDir, "mayor.md")

	for _, path := range []string{legacyDir, canonicalDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
	}
	for _, path := range []string{legacyFile, canonicalFile} {
		if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	if err := migrateLegacyContent(legacyDir, canonicalDir); err != nil {
		t.Fatalf("migrateLegacyContent(%q, %q): %v", legacyDir, canonicalDir, err)
	}

	if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
		t.Fatalf("legacy directory still exists: %q", legacyDir)
	}
}

func TestEnsureCityScaffoldMigratesLegacySystemArtifacts(t *testing.T) {
	cityDir := t.TempDir()
	legacySystemDir := filepath.Join(cityDir, citylayout.LegacySystemFormulasRoot)
	legacyBin := filepath.Join(cityDir, citylayout.LegacyBeadsBdScript)
	if err := os.MkdirAll(legacySystemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySystemDir, "base.formula.toml"), []byte("name = \"base\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureCityScaffold(cityDir); err != nil {
		t.Fatalf("ensureCityScaffold(%q): %v", cityDir, err)
	}

	for _, rel := range []string{
		filepath.Join(citylayout.SystemFormulasRoot, "base.formula.toml"),
		filepath.Join(citylayout.SystemBinRoot, "gc-beads-bd"),
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("missing migrated artifact %q: %v", rel, err)
		}
	}
}

func TestEnsureCityScaffoldMigratesLegacyPackStorage(t *testing.T) {
	cityDir := t.TempDir()
	legacyEntries := map[string]string{
		filepath.Join(citylayout.LegacyPacksRoot, "bd", "pack.toml"):             "[pack]\nname = \"bd\"\n",
		filepath.Join(citylayout.LegacyPacksRoot, "dolt", "pack.toml"):           "[pack]\nname = \"dolt\"\n",
		filepath.Join(citylayout.LegacyPacksRoot, "remote", ".git", "HEAD"):      "ref: refs/heads/main\n",
		filepath.Join(citylayout.LegacyPacksRoot, "_inc", "foo", ".git", "HEAD"): "ref: refs/heads/main\n",
	}
	for rel, content := range legacyEntries {
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureCityScaffold(cityDir); err != nil {
		t.Fatalf("ensureCityScaffold(%q): %v", cityDir, err)
	}

	for _, rel := range []string{
		filepath.Join(citylayout.SystemPacksRoot, "bd", "pack.toml"),
		filepath.Join(citylayout.SystemPacksRoot, "dolt", "pack.toml"),
		filepath.Join(citylayout.CachePacksRoot, "remote", ".git", "HEAD"),
		filepath.Join(citylayout.CacheIncludesRoot, "foo", ".git", "HEAD"),
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("missing migrated pack artifact %q: %v", rel, err)
		}
	}
}
