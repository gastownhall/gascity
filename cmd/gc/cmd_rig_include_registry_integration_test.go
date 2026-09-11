//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packregistry"
)

// scopedRegistryCatalog publishes one community pack under a SCOPED
// owner/pack name, the form the pack registry serves community packs under.
const scopedRegistryCatalog = `schema = 1

[[pack]]
name = "wespd/cacc-twin-team"
description = "Community twin-team pack."
source = "https://packages.example/cacc-twin-team.git"
source_kind = "git"

  [[pack.release]]
  version = "1.0.0"
  ref = "v1.0.0"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "First release."
`

// TestRigAddIncludeResolvesScopedRegistryPackName is the regression guard for
// registry-sfn: `gc rig add --include <owner>/<pack>` must resolve the scoped
// name against the cached registry catalog. Include resolution used to reject
// any slash-bearing token before looking it up — a safe proxy for "this is a
// path" only while every pack name was a single segment — so a scoped
// community name was persisted as the literal local import
// ./<owner>/<pack>, which resolves to a directory that does not exist.
func TestRigAddIncludeResolvesScopedRegistryPackName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, scopedRegistryCatalog)
	if err := packregistry.SaveConfig(home, packregistry.Config{
		Registries: []packregistry.Registry{{Name: "local", Source: catalogDir}},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := packregistry.WriteCatalogCache(home, "local", []byte(scopedRegistryCatalog)); err != nil {
		t.Fatalf("WriteCatalogCache: %v", err)
	}

	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"wespd/cacc-twin-team"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d; city.toml:\n%s", len(cfg.Rigs), data)
	}
	const wantSource = "https://packages.example/cacc-twin-team.git"
	var got []string
	for _, imp := range cfg.Rigs[0].Imports {
		got = append(got, imp.Source)
	}
	if !slices.Contains(got, wantSource) {
		t.Fatalf("rig import sources = %q, want the registry source %q; city.toml:\n%s", got, wantSource, data)
	}
	if slices.Contains(got, "./wespd/cacc-twin-team") {
		t.Errorf("scoped registry name persisted as a local import; pack expansion will fail citywide:\n%s", data)
	}
}
