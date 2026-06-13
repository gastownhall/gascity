package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

type failSiteRenameFS struct {
	fsys.OSFS
	target string
	failed bool
}

func (f *failSiteRenameFS) Rename(oldpath, newpath string) error {
	if !f.failed && filepath.Clean(newpath) == filepath.Clean(f.target) {
		f.failed = true
		return errors.New("injected site binding failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

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

func TestWriteCityAndRigSiteBindingsForEditRemovingRigsDropsDeletedBinding(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityPath, []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = "/srv/frontend"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SiteBindingPath(dir), []byte(`[[rig]]
name = "frontend"
path = "/site/frontend"

[[rig]]
name = "archived"
path = "/site/archived"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{{Name: "frontend", Path: "/site/frontend"}},
	}
	if err := WriteCityAndRigSiteBindingsForEditRemovingRigs(fsys.OSFS{}, cityPath, cfg, "archived"); err != nil {
		t.Fatalf("WriteCityAndRigSiteBindingsForEditRemovingRigs: %v", err)
	}

	binding, err := LoadSiteBinding(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 {
		t.Fatalf("binding.Rigs = %+v, want only frontend", binding.Rigs)
	}
	if binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != "/site/frontend" {
		t.Fatalf("binding.Rigs[0] = %+v, want frontend binding", binding.Rigs[0])
	}
}

func TestWriteCityAndRigSiteBindingsForEditRestoresCityWhenSiteBindingFails(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = "/srv/frontend"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{{Name: "frontend", Path: "/srv/frontend"}},
	}
	fs := &failSiteRenameFS{target: SiteBindingPath(dir)}

	err := WriteCityAndRigSiteBindingsForEdit(fs, cityPath, cfg)
	if err == nil {
		t.Fatal("WriteCityAndRigSiteBindingsForEdit succeeded, want injected site binding failure")
	}
	if !strings.Contains(err.Error(), "restored city.toml") {
		t.Fatalf("error = %v, want rollback guidance", err)
	}
	restored, readErr := os.ReadFile(cityPath)
	if readErr != nil {
		t.Fatalf("read city.toml: %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml = %q, want restored original %q", restored, original)
	}
	if _, statErr := os.Stat(SiteBindingPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("site.toml stat err = %v, want not exist", statErr)
	}
}

func TestAppendRigAndWriteSiteBindingsForEditPreservesCityTomlSymlink(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}

	repoCityPath := filepath.Join(repoDir, "city.toml")
	liveCityPath := filepath.Join(cityDir, "city.toml")
	original := []byte(`[workspace]
name = "test-city"
`)
	if err := os.WriteFile(repoCityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "repo", "city.toml"), liveCityPath); err != nil {
		t.Fatal(err)
	}

	newRig := Rig{Name: "frontend", Path: "/srv/frontend"}
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{newRig},
	}
	if err := AppendRigAndWriteSiteBindingsForEdit(fsys.OSFS{}, liveCityPath, cfg, newRig); err != nil {
		t.Fatalf("AppendRigAndWriteSiteBindingsForEdit: %v", err)
	}

	info, err := os.Lstat(liveCityPath)
	if err != nil {
		t.Fatalf("Lstat(live city.toml): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("live city.toml was replaced with a regular file")
	}

	rewritten, err := os.ReadFile(repoCityPath)
	if err != nil {
		t.Fatalf("ReadFile(repo city.toml): %v", err)
	}
	if !strings.Contains(string(rewritten), `name = "frontend"`) {
		t.Fatalf("repo city.toml did not receive appended rig:\n%s", rewritten)
	}
	if strings.Contains(string(rewritten), `path = "/srv/frontend"`) {
		t.Fatalf("repo city.toml should not contain machine-local rig path:\n%s", rewritten)
	}

	binding, err := LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != "/srv/frontend" {
		t.Fatalf("site binding rigs = %+v, want frontend=/srv/frontend", binding.Rigs)
	}
}

// Regression for PR #3428 review: when city.toml is a symlink and the site
// binding write fails after the append, the rollback must restore the resolved
// target's original bytes and leave the live symlink intact (it must not
// replace the link with a regular file).
func TestAppendRigAndWriteSiteBindingsForEditRestoresSymlinkedCityWhenSiteBindingFails(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}

	repoCityPath := filepath.Join(repoDir, "city.toml")
	liveCityPath := filepath.Join(cityDir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n")
	if err := os.WriteFile(repoCityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "repo", "city.toml"), liveCityPath); err != nil {
		t.Fatal(err)
	}

	newRig := Rig{Name: "frontend", Path: "/srv/frontend"}
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs:      []Rig{newRig},
	}
	fs := &failSiteRenameFS{target: SiteBindingPath(cityDir)}

	err := AppendRigAndWriteSiteBindingsForEdit(fs, liveCityPath, cfg, newRig)
	if err == nil {
		t.Fatal("AppendRigAndWriteSiteBindingsForEdit succeeded, want injected site binding failure")
	}
	if !strings.Contains(err.Error(), "restored city.toml") {
		t.Fatalf("error = %v, want rollback guidance", err)
	}

	info, err := os.Lstat(liveCityPath)
	if err != nil {
		t.Fatalf("Lstat(live city.toml): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("live city.toml was replaced with a regular file during rollback")
	}
	restored, readErr := os.ReadFile(repoCityPath)
	if readErr != nil {
		t.Fatalf("read repo city.toml: %v", readErr)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("repo city.toml = %q, want restored original %q", restored, original)
	}
	if _, statErr := os.Stat(SiteBindingPath(cityDir)); !os.IsNotExist(statErr) {
		t.Fatalf("site.toml stat err = %v, want not exist", statErr)
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
	data := []byte(`[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)
	cfg := &City{
		Workspace: Workspace{Name: "legacy-city", Prefix: "lc"},
		Rigs: []Rig{{
			Name: "frontend",
			Path: "/legacy/frontend",
		}},
	}

	err := LegacySiteBindingSurfaceError(cfg, "city.toml", data)
	if err == nil {
		t.Fatal("LegacySiteBindingSurfaceError returned nil, want aggregated error")
	}
	for _, want := range []string{
		"pre-1.0 site-binding fields are no longer supported",
		`city.toml:3: unsupported pre-1.0 rig.path for rig "frontend"`,
		packV1MigrationDocsURL,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
	if got := strings.Count(err.Error(), packV1MigrationDocsURL); got != 1 {
		t.Fatalf("error = %v, want one docs pointer, got %d", err, got)
	}

	warnings := legacyWorkspaceIdentitySurfaceWarnings(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one workspace identity warning", warnings)
	}
	if !strings.Contains(warnings[0], "workspace identity fields are deprecated in v2; move them to .gc/site.toml (workspace.name, workspace.prefix)") {
		t.Fatalf("warning = %q, want workspace identity guidance", warnings[0])
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
	for _, want := range []string{
		`/city/city.toml:4: unsupported pre-1.0 rig.path for rig "frontend"`,
		packV1MigrationDocsURL,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
}

func TestLoadWithIncludes_WarnsOnLegacyWorkspaceIdentityInSchema2City(t *testing.T) {
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

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var found bool
	for _, warning := range prov.Warnings {
		if strings.Contains(warning, "workspace identity fields are deprecated in v2; move them to .gc/site.toml (workspace.name, workspace.prefix)") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want legacy workspace identity guidance", prov.Warnings)
	}
}

func TestLoadWithIncludes_WarnsOnLegacyWorkspaceIdentityInSchema2Fragments(t *testing.T) {
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
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var found bool
	for _, warning := range prov.Warnings {
		if strings.Contains(warning, "/city/fragments/legacy.toml: workspace identity fields are deprecated in v2; move them to .gc/site.toml (workspace.name)") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want fragment workspace identity guidance", prov.Warnings)
	}
}

func TestLoadWithIncludes_WarnsOnLegacyRigPathInSchema2Fragment(t *testing.T) {
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
[[rigs]]
name = "frontend"
path = "/legacy/frontend"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var found bool
	for _, warning := range prov.Warnings {
		if strings.Contains(warning, `/city/fragments/legacy.toml: rig.path is deprecated in v2; move it to .gc/site.toml for rig "frontend"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want fragment rig.path guidance", prov.Warnings)
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
