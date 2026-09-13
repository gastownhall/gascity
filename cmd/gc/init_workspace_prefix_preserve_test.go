package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeSiteBindingForTest(t *testing.T, cityPath, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SiteBindingPath(cityPath), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A city.toml with no declared prefix must not clear the bound one; the city
// would fall through to a derived prefix and mint beads under a new namespace.
func TestPersistInitWorkspaceIdentityKeepsBoundPrefixWhenCityTomlHasNone(t *testing.T) {
	cityPath := t.TempDir()
	writeSiteBindingForTest(t, cityPath, "workspace_name = \"site-city\"\nworkspace_prefix = \"sc\"\n")
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	cfg := &config.City{}

	if err := persistInitWorkspaceIdentity(fsys.OSFS{}, cityPath, cityTomlPath, cfg, "site-city", ""); err != nil {
		t.Fatalf("persistInitWorkspaceIdentity: %v", err)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if binding.WorkspacePrefix != "sc" {
		t.Fatalf("WorkspacePrefix = %q, want preserved %q", binding.WorkspacePrefix, "sc")
	}
}

func TestPersistInitWorkspaceIdentityWritesDeclaredPrefix(t *testing.T) {
	cityPath := t.TempDir()
	writeSiteBindingForTest(t, cityPath, "workspace_name = \"site-city\"\nworkspace_prefix = \"sc\"\n")
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	cfg := &config.City{}

	if err := persistInitWorkspaceIdentity(fsys.OSFS{}, cityPath, cityTomlPath, cfg, "site-city", "nu"); err != nil {
		t.Fatalf("persistInitWorkspaceIdentity: %v", err)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if binding.WorkspacePrefix != "nu" {
		t.Fatalf("WorkspacePrefix = %q, want %q", binding.WorkspacePrefix, "nu")
	}
}

func TestPersistInitWorkspaceIdentityBindsFreshCity(t *testing.T) {
	cityPath := t.TempDir()
	cityTomlPath := filepath.Join(cityPath, "city.toml")
	cfg := &config.City{}

	if err := persistInitWorkspaceIdentity(fsys.OSFS{}, cityPath, cityTomlPath, cfg, "fresh-city", "fc"); err != nil {
		t.Fatalf("persistInitWorkspaceIdentity: %v", err)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if binding.WorkspaceName != "fresh-city" || binding.WorkspacePrefix != "fc" {
		t.Fatalf("binding = %+v, want fresh-city/fc", binding)
	}
}
