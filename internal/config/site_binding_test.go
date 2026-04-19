package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestMarshalForWrite_StripsRigPaths(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs: []Rig{{
			Name: "frontend",
			Path: "/tmp/frontend",
		}},
	}

	data, err := cfg.MarshalForWrite()
	if err != nil {
		t.Fatalf("MarshalForWrite: %v", err)
	}
	if strings.Contains(string(data), "path = ") {
		t.Fatalf("MarshalForWrite should omit rig.path:\n%s", data)
	}
}

func TestPersistRigSiteBindings(t *testing.T) {
	fs := fsys.NewFake()
	cfg := []Rig{
		{Name: "beta", Path: "/tmp/beta"},
		{Name: "alpha", Path: "/tmp/alpha"},
		{Name: "unbound"},
	}

	if err := PersistRigSiteBindings(fs, "/city", cfg); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}

	binding, err := LoadSiteBinding(fs, "/city")
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 2 {
		t.Fatalf("len(binding.Rigs) = %d, want 2", len(binding.Rigs))
	}
	if binding.Rigs[0].Name != "alpha" || binding.Rigs[0].Path != "/tmp/alpha" {
		t.Fatalf("binding[0] = %+v, want alpha=/tmp/alpha", binding.Rigs[0])
	}
	if binding.Rigs[1].Name != "beta" || binding.Rigs[1].Path != "/tmp/beta" {
		t.Fatalf("binding[1] = %+v, want beta=/tmp/beta", binding.Rigs[1])
	}
}

func TestApplySiteBindingsForEdit_KeepsLegacyPath(t *testing.T) {
	fs := fsys.NewFake()
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{{Name: "frontend", Path: "/legacy/frontend"}},
	}

	warnings, err := ApplySiteBindingsForEdit(fs, "/city", cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindingsForEdit: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if cfg.Rigs[0].Path != "/legacy/frontend" {
		t.Fatalf("Path = %q, want legacy path preserved", cfg.Rigs[0].Path)
	}
}

func TestLoadWithIncludes_AppliesSiteBindings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)
	fs.Files[SiteBindingPath("/city")] = []byte(`
[[rig]]
name = "frontend"
path = "/site/frontend"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Rigs[0].Path != "/site/frontend" {
		t.Fatalf("Path = %q, want site binding path", cfg.Rigs[0].Path)
	}
	if len(prov.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", prov.Warnings)
	}
}

func TestLoadWithIncludes_WarnsOnUnboundRig(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Rigs[0].Path != "" {
		t.Fatalf("Path = %q, want empty for unbound rig", cfg.Rigs[0].Path)
	}
	if len(prov.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one unbound-rig warning", prov.Warnings)
	}
	if !strings.Contains(prov.Warnings[0], "frontend") || !strings.Contains(prov.Warnings[0], "no path binding") {
		t.Fatalf("warnings[0] = %q, want mention of rig name and unbound state", prov.Warnings[0])
	}
	// The remediation must be a valid CLI form: `gc rig add <dir> --name <rig>`,
	// not the nonexistent `--path` flag form.
	if !strings.Contains(prov.Warnings[0], "gc rig add <dir> --name frontend") {
		t.Fatalf("warnings[0] = %q, want real CLI form `gc rig add <dir> --name <rig>`", prov.Warnings[0])
	}
}

func TestApplySiteBindingsForEdit_NoWarnForUnboundRig(t *testing.T) {
	fs := fsys.NewFake()
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{{Name: "frontend"}},
	}

	warnings, err := ApplySiteBindingsForEdit(fs, "/city", cfg)
	if err != nil {
		t.Fatalf("ApplySiteBindingsForEdit: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want no warnings in edit mode (edit flow is migrating)", warnings)
	}
}

func TestLoadWithIncludes_FallsBackToLegacyRigPathWithoutSiteBinding(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Rigs[0].Path != "/legacy/frontend" {
		t.Fatalf("Path = %q, want legacy path fallback without site binding", cfg.Rigs[0].Path)
	}
	if len(prov.Warnings) != 1 || !strings.Contains(prov.Warnings[0], ".gc/site.toml") {
		t.Fatalf("warnings = %v, want legacy site binding guidance", prov.Warnings)
	}
}
