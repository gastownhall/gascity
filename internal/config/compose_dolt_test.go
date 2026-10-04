package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestLoadWithIncludesPreservesDoltModeAcrossDoltFragment is the load-bearing
// regression: an included fragment that defines ONLY an unrelated [dolt]
// sibling key must NOT reset the root's explicit mode. Without the per-field
// IsDefined preservation branch the root's mode = "server" escape hatch
// silently becomes "", which resolves to the proxied provider default.
func TestLoadWithIncludesPreservesDoltModeAcrossDoltFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[dolt]
mode = "server"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[dolt]
archive_level = 1
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Dolt.Mode != "server" {
		t.Fatalf("Dolt.Mode = %q, want root server to survive a [dolt] fragment", cfg.Dolt.Mode)
	}
	if cfg.Dolt.ArchiveLevel == nil || *cfg.Dolt.ArchiveLevel != 1 {
		t.Fatalf("Dolt.ArchiveLevel = %v, want the fragment's 1", cfg.Dolt.ArchiveLevel)
	}
}

// TestLoadWithIncludesFragmentOverridesDoltMode is the companion to the
// preservation test: a fragment that DOES set mode must win (LWW), so the
// preservation branch can't drift into "base value always wins."
func TestLoadWithIncludesFragmentOverridesDoltMode(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[dolt]
mode = "server"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[dolt]
mode = "proxied-server"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Dolt.Mode != "proxied-server" {
		t.Fatalf("Dolt.Mode = %q, want the fragment's proxied-server to win", cfg.Dolt.Mode)
	}
}
