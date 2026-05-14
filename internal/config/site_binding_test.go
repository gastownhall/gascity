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

func TestPersistRigSiteBindings_PreservesWorkspaceIdentity(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files[SiteBindingPath("/city")] = []byte(`
workspace_name = "site-city"
workspace_prefix = "sc"
`)
	cfg := []Rig{{Name: "frontend", Path: "/tmp/frontend"}}

	if err := PersistRigSiteBindings(fs, "/city", cfg); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}

	binding, err := LoadSiteBinding(fs, "/city")
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if binding.WorkspaceName != "site-city" {
		t.Fatalf("WorkspaceName = %q, want %q", binding.WorkspaceName, "site-city")
	}
	if binding.WorkspacePrefix != "sc" {
		t.Fatalf("WorkspacePrefix = %q, want %q", binding.WorkspacePrefix, "sc")
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" {
		t.Fatalf("binding.Rigs = %+v, want preserved workspace identity plus frontend rig", binding.Rigs)
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

func TestLoadWithIncludes_AppliesWorkspaceIdentitySiteBinding(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "declared-city"
prefix = "declared"
`)
	fs.Files[SiteBindingPath("/city")] = []byte(`
workspace_name = "site-city"
workspace_prefix = "sc"
`)

	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Workspace.Name != "declared-city" {
		t.Fatalf("Workspace.Name = %q, want raw declared value preserved", cfg.Workspace.Name)
	}
	if cfg.Workspace.Prefix != "declared" {
		t.Fatalf("Workspace.Prefix = %q, want raw declared value preserved", cfg.Workspace.Prefix)
	}
	if cfg.ResolvedWorkspaceName != "site-city" {
		t.Fatalf("ResolvedWorkspaceName = %q, want %q", cfg.ResolvedWorkspaceName, "site-city")
	}
	if cfg.ResolvedWorkspacePrefix != "sc" {
		t.Fatalf("ResolvedWorkspacePrefix = %q, want %q", cfg.ResolvedWorkspacePrefix, "sc")
	}
	if got := cfg.EffectiveCityName(); got != "site-city" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "site-city")
	}
	if got := EffectiveHQPrefix(cfg); got != "sc" {
		t.Fatalf("EffectiveHQPrefix() = %q, want %q", got, "sc")
	}
	if len(prov.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", prov.Warnings)
	}
}

func TestLegacySiteBindingSurfaceErrorAggregatesViolations(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Name: "legacy-city", Prefix: "lc"},
		Rigs: []Rig{{
			Name: "frontend",
			Path: "/legacy/frontend",
		}},
	}

	err := LegacySiteBindingSurfaceError(cfg, "city.toml")
	if err == nil {
		t.Fatal("LegacySiteBindingSurfaceError returned nil, want aggregated error")
	}
	for _, want := range []string{
		"pre-1.0 site-binding fields are no longer supported",
		"city.toml: unsupported pre-1.0 workspace identity fields (workspace.name, workspace.prefix)",
		`city.toml: unsupported pre-1.0 rig.path for rig "frontend"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
}

func TestLoadWithIncludes_RejectsLegacyRigPathInSchema2City(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "city"
schema = 2
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes succeeded, want hard error for legacy rig.path")
	}
	if !strings.Contains(err.Error(), `unsupported pre-1.0 rig.path for rig "frontend"`) {
		t.Fatalf("error = %v, want legacy rig.path guidance", err)
	}
}

func TestLoadWithIncludes_RejectsLegacyWorkspaceIdentityInSchema2City(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "legacy-city"
prefix = "lc"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "city"
schema = 2
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes succeeded, want hard error for legacy workspace identity")
	}
	if !strings.Contains(err.Error(), "unsupported pre-1.0 workspace identity fields (workspace.name, workspace.prefix)") {
		t.Fatalf("error = %v, want legacy workspace identity guidance", err)
	}
}

func TestLoadWithIncludes_WarnsOnLegacySiteBindingSurfacesInSchema2Fragments(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragments/legacy.toml"]
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "city"
schema = 2
`)
	fs.Files["/city/fragments/legacy.toml"] = []byte(`
[workspace]
name = "fragment-city"

[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	for _, want := range []string{
		"/city/fragments/legacy.toml: workspace identity fields are deprecated in v2; move them to .gc/site.toml (workspace.name)",
		`/city/fragments/legacy.toml: rig.path is deprecated in v2; move it to .gc/site.toml for rig "frontend"`,
	} {
		var found bool
		for _, warning := range prov.Warnings {
			if strings.Contains(warning, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("warnings = %v, want substring %q", prov.Warnings, want)
		}
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

func TestLoad_AppliesWorkspaceIdentitySiteBinding(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "declared-city"
`)
	fs.Files[SiteBindingPath("/city")] = []byte(`
workspace_name = "site-city"
workspace_prefix = "sc"
`)

	cfg, err := Load(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResolvedWorkspaceName != "site-city" {
		t.Fatalf("ResolvedWorkspaceName = %q, want %q", cfg.ResolvedWorkspaceName, "site-city")
	}
	if cfg.ResolvedWorkspacePrefix != "sc" {
		t.Fatalf("ResolvedWorkspacePrefix = %q, want %q", cfg.ResolvedWorkspacePrefix, "sc")
	}
	if got := cfg.EffectiveCityName(); got != "site-city" {
		t.Fatalf("EffectiveCityName() = %q, want %q", got, "site-city")
	}
}
