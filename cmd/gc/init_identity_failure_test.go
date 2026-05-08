package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

type failSiteBindingRenameFS struct {
	fsys.OSFS
	target string
	failed bool
}

func (f *failSiteBindingRenameFS) Rename(oldpath, newpath string) error {
	if !f.failed && filepath.Clean(newpath) == filepath.Clean(f.target) {
		f.failed = true
		return errors.New("injected site binding failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

func TestDoInitRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := doInit(fs, cityPath, defaultWizardConfig(), "machine-alias", &stdout, &stderr, false)
	if code == 0 {
		t.Fatalf("doInit = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "ma" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "ma")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "site.toml")); !os.IsNotExist(err) {
		t.Fatalf("site binding stat err = %v, want not exist", err)
	}
}

func TestDoInitFromFileRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	srcDir := t.TempDir()
	srcCfg := config.DefaultCity("declared-city")
	srcCfg.Workspace.Prefix = "dc"
	srcData, err := srcCfg.Marshal()
	if err != nil {
		t.Fatalf("marshal source config: %v", err)
	}
	srcToml := filepath.Join(srcDir, "source.toml")
	if err := os.WriteFile(srcToml, srcData, 0o644); err != nil {
		t.Fatalf("write source.toml: %v", err)
	}

	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fs, srcToml, cityPath, "machine-alias", &stdout, &stderr, true, false)
	if code == 0 {
		t.Fatalf("cmdInitFromTOMLFileWithOptions = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "dc" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "dc")
	}
}

func TestDoInitFromDirRestoresLegacyIdentityWhenSiteBindingWriteFails(t *testing.T) {
	srcDir := t.TempDir()
	srcCfg := config.DefaultCity("declared-city")
	srcCfg.Workspace.Prefix = "dc"
	srcData, err := srcCfg.Marshal()
	if err != nil {
		t.Fatalf("marshal source config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"), srcData, 0o644); err != nil {
		t.Fatalf("write source city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "pack.toml"), []byte("[pack]\nname = \"declared-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("write source pack.toml: %v", err)
	}

	cityPath := filepath.Join(t.TempDir(), "target-city")
	fs := &failSiteBindingRenameFS{target: filepath.Join(cityPath, ".gc", "site.toml")}

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsFS(fs, srcDir, cityPath, "machine-alias", &stdout, &stderr, true)
	if code == 0 {
		t.Fatalf("doInitFromDirWithOptionsFS = %d, want failure", code)
	}
	if !strings.Contains(stderr.String(), "injected site binding failure") {
		t.Fatalf("stderr = %q, want injected site binding failure", stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load config after rollback: %v", err)
	}
	if got := config.EffectiveCityName(cfg, filepath.Base(cityPath)); got != "machine-alias" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "machine-alias")
	}
	if got := config.EffectiveHQPrefix(cfg); got != "dc" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "dc")
	}
}
