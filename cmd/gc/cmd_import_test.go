package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestDoImportAddRemoteWritesConfigAndLock(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imp, ok := cfg.Imports["tools"]
	if !ok {
		t.Fatal("missing imports.tools")
	}
	if imp.Version != "^1.4" {
		t.Fatalf("Version = %q, want %q", imp.Version, "^1.4")
	}
	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if strings.Contains(string(cityData), "[imports.tools]") {
		t.Fatalf("city.toml should not contain imports:\n%s", string(cityData))
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if _, ok := lock.Packs["https://github.com/example/tools.git"]; !ok {
		t.Fatal("missing lock entry")
	}
}

func TestDoImportAddPreservesExistingPackTomlContent(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
version = "0.9.0"
schema = 1
includes = ["./packs/base"]

[agent_defaults]
default_sling_formula = "mol-pack-default"
append_fragments = ["pack-fragment"]

[[agent]]
name = "mayor"
scope = "city"

[providers.default]
base = "builtin:claude"

[[commands]]
name = "status"
description = "Show status"
long_description = "commands/status.md"
script = "commands/status.sh"

[global]
session_live = ["echo hi"]
`)

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`version = "0.9.0"`,
		`includes = ["./packs/base"]`,
		`[agent_defaults]`,
		`default_sling_formula = "mol-pack-default"`,
		`append_fragments = ["pack-fragment"]`,
		`name = "mayor"`,
		`[providers.default]`,
		`base = "builtin:claude"`,
		`[[commands]]`,
		`session_live = ["echo hi"]`,
		`[imports.tools]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pack.toml missing %q:\n%s", want, text)
		}
	}
}

// Regression for the ga-lurp5d follow-up review: gc import add rewrites
// pack.toml through the reduced cityPackManifest struct. When the on-disk
// pack.toml carries a key this gc binary does not recognize, the rewrite must
// refuse rather than silently drop it (the city.toml rewrite guard's contract,
// now extended to pack.toml).
func TestDoImportAddRefusesUnknownPackTomlKeys(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	original := `[pack]
name = "demo"
schema = 1

[future_unknown_section]
knob = "keep-me"
`
	writePackToml(t, dir, original)

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("code = 0, want non-zero refusal; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "future_unknown_section") {
		t.Fatalf("stderr = %q, want mention of future_unknown_section", stderr.String())
	}
	// The pack.toml must survive an aborted rewrite unchanged.
	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	if string(data) != original {
		t.Fatalf("pack.toml was rewritten despite refusal:\n%s", data)
	}
}

func TestDoImportAddPreservesRootPackDefaultRigImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.gastown]
source = "./packs/gastown"
`)

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	defaultRigImports, err := config.LoadRootPackDefaultRigImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadRootPackDefaultRigImports: %v", err)
	}
	if len(defaultRigImports) != 1 {
		t.Fatalf("len(defaultRigImports) = %d, want 1", len(defaultRigImports))
	}
	if got := defaultRigImports[0].Binding; got != "gastown" {
		t.Fatalf("binding = %q, want %q", got, "gastown")
	}
	if got := defaultRigImports[0].Import.Source; got != "./packs/gastown" {
		t.Fatalf("source = %q, want %q", got, "./packs/gastown")
	}

	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`[defaults.rig.imports."gastown"]`,
		`source = "./packs/gastown"`,
		`[imports.tools]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pack.toml missing %q:\n%s", want, text)
		}
	}
}

func TestDoImportAddPreservesQuotedRootPackDefaultRigImportBinding(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports."foo.bar"]
source = "./packs/foo"
`)

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `[defaults.rig.imports."foo.bar"]`) {
		t.Fatalf("pack.toml did not preserve quoted default-rig binding:\n%s", text)
	}
	if strings.Contains(text, "[defaults.rig.imports.foo.bar]") {
		t.Fatalf("pack.toml split quoted default-rig binding:\n%s", text)
	}
}

func TestDoImportAddPreservesRootPackDefaultRigImportOrder(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.z-pack]
source = "./packs/z-pack"

[defaults.rig.imports.a-pack]
source = "./packs/a-pack"
`)

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	defaultRigImports, err := config.LoadRootPackDefaultRigImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadRootPackDefaultRigImports: %v", err)
	}
	if len(defaultRigImports) != 2 {
		t.Fatalf("len(defaultRigImports) = %d, want 2", len(defaultRigImports))
	}
	if got := defaultRigImports[0].Binding; got != "z-pack" {
		t.Fatalf("first binding = %q, want %q", got, "z-pack")
	}
	if got := defaultRigImports[1].Binding; got != "a-pack" {
		t.Fatalf("second binding = %q, want %q", got, "a-pack")
	}
}

func TestDoImportAddPathRejectsVersionFlag(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"local\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "./packs/local", "", "^1.2", &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected failure for path import with version")
	}
	if !strings.Contains(stderr.String(), "--version is only valid for git-backed imports") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoImportAddRejectsRepositoryRefInSource(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "file:///tmp/repo.git#main", "", "", &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected failure for source ref")
	}
	if !strings.Contains(stderr.String(), "embed refs in --version") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoImportAddAcceptsGitHubTreeSource(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	prevSync := syncImports
	t.Cleanup(func() {
		syncImports = prevSync
	})
	source := "https://github.com/example/repo/tree/main/packs/base"
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		imp, ok := imports["pack:base"]
		if !ok {
			t.Fatalf("missing imports.pack:base in %#v", imports)
		}
		if imp.Source != source {
			t.Fatalf("Source = %q, want %q", imp.Source, source)
		}
		if imp.Version != "^1.2.0" {
			t.Fatalf("Version = %q, want ^1.2.0", imp.Version)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				source: {Version: "1.2.3", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, source, "", "^1.2.0", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imp := cfg.Imports["base"]
	if imp.Source != source {
		t.Fatalf("Source = %q, want %q", imp.Source, source)
	}
	if imp.Version != "^1.2.0" {
		t.Fatalf("Version = %q, want ^1.2.0", imp.Version)
	}
}

func TestDoImportAddGitHubSubpathWithVersionWritesImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	prevSync := syncImports
	t.Cleanup(func() {
		syncImports = prevSync
	})
	source := "github.com/example/tools//packs/review"
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		imp, ok := imports["pack:review"]
		if !ok {
			t.Fatalf("missing imports.pack:review in %#v", imports)
		}
		if imp.Source != source {
			t.Fatalf("Source = %q, want %q", imp.Source, source)
		}
		if imp.Version != "^1.2.0" {
			t.Fatalf("Version = %q, want ^1.2.0", imp.Version)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				source: {Version: "1.2.3", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, source, "", "^1.2.0", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imp := cfg.Imports["review"]
	if imp.Source != source {
		t.Fatalf("Source = %q, want %q", imp.Source, source)
	}
	if imp.Version != "^1.2.0" {
		t.Fatalf("Version = %q, want ^1.2.0", imp.Version)
	}
}

func TestDoImportAddPlainDirectoryOmitsVersion(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"suggested-display-name\"\nschema = 1\n")

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "./packs/local", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imp, ok := cfg.Imports["local"]
	if !ok {
		t.Fatal("missing imports.local")
	}
	if imp.Source != "./packs/local" {
		t.Fatalf("Source = %q, want %q", imp.Source, "./packs/local")
	}
	if imp.Version != "" {
		t.Fatalf("Version = %q, want empty", imp.Version)
	}
	text, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	for _, forbidden := range []string{
		"suggested-display-name",
		"export",
		"transitive",
		"shadow",
	} {
		if strings.Contains(string(text), forbidden) {
			t.Fatalf("authored import leaked %q into pack.toml:\n%s", forbidden, string(text))
		}
	}
}

func TestDoImportRemoveRewritesConfig(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n\n[imports.tools]\nsource = \"https://github.com/example/tools.git\"\nversion = \"^1.4\"\n")

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "tools", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Imports["tools"]; ok {
		t.Fatal("imports.tools still present")
	}
}

func TestDoImportAddRefusesCityOwnedRootImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")
	writeCityToml(t, dir, `[workspace]
name = "demo"

[imports.tools]
source = "packs/tools"
`)

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		t.Fatal("syncImports must not run for a refused add")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://example.com/tools.git", "tools", "^1.4", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "city.toml") {
		t.Fatalf("stderr must point at city.toml ownership:\n%s", stderr.String())
	}

	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("loadCityPackManifestFS: %v", err)
	}
	if len(manifest.Imports) != 0 {
		t.Fatalf("pack.toml imports = %#v, want untouched", manifest.Imports)
	}
}

func TestDoImportRemoveRefusesCityOverriddenPackImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	writeCityToml(t, dir, `[workspace]
name = "demo"

[imports.tools]
source = "packs/tools"
`)

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		t.Fatal("syncImports must not run for a refused remove")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "tools", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "city.toml") {
		t.Fatalf("stderr must point at city.toml ownership:\n%s", stderr.String())
	}

	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("loadCityPackManifestFS: %v", err)
	}
	if _, ok := manifest.Imports["tools"]; !ok {
		t.Fatal("pack.toml imports.tools must survive a refused remove")
	}
	cfg, err := loadCityImportManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("loadCityImportManifestFS: %v", err)
	}
	if _, ok := cfg.Imports["tools"]; !ok {
		t.Fatal("city.toml imports.tools must survive a refused remove")
	}
}

func TestDoImportRemoveRemovesCityOnlyRootImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")
	writeCityToml(t, dir, `[workspace]
name = "demo"

[imports.tools]
source = "packs/tools"
`)

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	var synced map[string]config.Import
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		synced = imports
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "tools", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if _, ok := synced["pack:tools"]; ok {
		t.Fatalf("synced imports still contain removed city import: %#v", synced)
	}
	cfg, err := loadCityImportManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("loadCityImportManifestFS: %v", err)
	}
	if _, ok := cfg.Imports["tools"]; ok {
		t.Fatal("city.toml imports.tools must be removed")
	}
	manifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("loadCityPackManifestFS: %v", err)
	}
	if len(manifest.Imports) != 0 {
		t.Fatalf("pack.toml imports = %#v, want untouched", manifest.Imports)
	}
}

func TestDoImportRemovePreservesRootPackDefaultRigImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"

[defaults.rig.imports.gastown]
source = "./packs/gastown"
`)

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "tools", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	defaultRigImports, err := config.LoadRootPackDefaultRigImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadRootPackDefaultRigImports: %v", err)
	}
	if len(defaultRigImports) != 1 {
		t.Fatalf("len(defaultRigImports) = %d, want 1", len(defaultRigImports))
	}
	if got := defaultRigImports[0].Binding; got != "gastown" {
		t.Fatalf("binding = %q, want %q", got, "gastown")
	}
	if got := defaultRigImports[0].Import.Source; got != "./packs/gastown" {
		t.Fatalf("source = %q, want %q", got, "./packs/gastown")
	}

	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("ReadFile(pack.toml): %v", err)
	}
	text := string(data)
	if strings.Contains(text, `[imports.tools]`) {
		t.Fatalf("pack.toml still contains removed import:\n%s", text)
	}
	for _, want := range []string{
		`[defaults.rig.imports."gastown"]`,
		`source = "./packs/gastown"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pack.toml missing %q:\n%s", want, text)
		}
	}
}

func TestDoImportInstallUsesLockMode(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.4.2", Commit: "abc123"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	syncImports = func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != dir {
			t.Fatalf("cityRoot = %q, want %q", cityRoot, dir)
		}
		if mode != packman.InstallResolveIfNeeded {
			t.Fatalf("mode = %v, want InstallResolveIfNeeded", mode)
		}
		found := false
		for _, imp := range imports {
			if imp.Source == "https://example.com/tools.git" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("imports = %#v, want tools source present", imports)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}
	called := false
	installLockedImports = func(_ string) (*packman.Lockfile, error) {
		called = true
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportInstall(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatal("expected InstallLocked to be called")
	}
}

func TestDoImportInstallCityImportOverridesRootPackImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	localPack := filepath.Join(dir, "packs", "tools")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"tools\"\nschema = 1\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	writeCityToml(t, dir, `[workspace]
name = "demo"

[imports.tools]
source = "packs/tools"
`)

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		imp, ok := imports["pack:tools"]
		if !ok {
			t.Fatalf("imports = %#v, want pack:tools", imports)
		}
		if imp.Source != "packs/tools" {
			t.Fatalf("pack:tools source = %q, want city.toml override", imp.Source)
		}
		for name, imp := range imports {
			if imp.Source == "https://example.com/tools.git" {
				t.Fatalf("imports[%s] still uses root pack remote source: %#v", name, imports)
			}
		}
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}
	installLockedImports = func(_ string) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportInstall(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoImportInstallRewritesLockToCurrentGraph(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.a]
source = "https://example.com/a.git"
version = "^1.0"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa"},
			"https://example.com/b.git": {Version: "2.0.0", Commit: "bbbb"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa"},
			},
		}, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		lock, err := packman.ReadLockfile(fsys.OSFS{}, cityRoot)
		if err != nil {
			t.Fatalf("ReadLockfile during install: %v", err)
		}
		if len(lock.Packs) != 1 {
			t.Fatalf("len(Packs) = %d, want 1", len(lock.Packs))
		}
		if _, ok := lock.Packs["https://example.com/a.git"]; !ok {
			t.Fatalf("lock = %#v, want only import a", lock.Packs)
		}
		return lock, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportInstall(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoImportInstallBootstrapsMissingLockfile(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})

	syncImports = func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != dir {
			t.Fatalf("cityRoot = %q, want %q", cityRoot, dir)
		}
		if mode != packman.InstallResolveIfNeeded {
			t.Fatalf("mode = %v, want InstallResolveIfNeeded", mode)
		}
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("imports = %#v, want pack:tools import", imports)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		lock, err := packman.ReadLockfile(fsys.OSFS{}, cityRoot)
		if err != nil {
			t.Fatalf("ReadLockfile during install: %v", err)
		}
		if _, ok := lock.Packs["https://example.com/tools.git"]; !ok {
			t.Fatalf("lock = %#v, want tools entry", lock.Packs)
		}
		return lock, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportInstall(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if _, ok := lock.Packs["https://example.com/tools.git"]; !ok {
		t.Fatalf("lock = %#v, want tools entry", lock.Packs)
	}
}

func TestDoImportInstallWithNoImportsSucceeds(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportInstall(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed 0 remote import(s)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if len(lock.Packs) != 0 {
		t.Fatalf("len(Packs) = %d, want 0", len(lock.Packs))
	}
}

func TestDoImportCheckReportsOKForInstalledImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"

[rigs.imports.ui]
source = "https://example.com/ui.git"
version = "^2.0"
`)
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
transitive = false
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(cityRoot string, imports map[string]config.Import) (*packman.CheckReport, error) {
		if cityRoot != dir {
			t.Fatalf("cityRoot = %q, want %q", cityRoot, dir)
		}
		for _, name := range []string{"pack:tools", "default-rig:worker", "rig:frontend:ui"} {
			if _, ok := imports[name]; !ok {
				t.Fatalf("imports = %#v, want %s", imports, name)
			}
		}
		return &packman.CheckReport{CheckedSources: 3}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportCheck(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Import state OK: 3 remote import(s) checked") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoImportCheckPrintsIssueDetails(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       "missing-cache",
				ImportName: "pack:tools",
				Source:     "https://example.com/tools.git",
				Commit:     "abc123",
				Path:       filepath.Join(dir, ".gc", "cache", "repos", "abc"),
				Message:    "locked import is missing from the local repo cache",
				RepairHint: `run "gc import install"`,
			}},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportCheck(dir, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Import state has 1 issue(s):",
		"[error] missing-cache pack:tools",
		"locked import is missing from the local repo cache",
		`repair: run "gc import install"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s\nstderr:\n%s", want, out, stderr.String())
		}
	}
}

func TestDoImportUpgradeTargetedMergesPreservedImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.a]
source = "https://example.com/a.git"
version = "^1.0"

[imports.b]
source = "https://example.com/b.git"
version = "^2.0"
`)

	prevSync := syncImports
	prevSelective := syncImportsSelective
	t.Cleanup(func() {
		syncImports = prevSync
		syncImportsSelective = prevSelective
	})
	syncImportsSelective = func(_ string, imports map[string]config.Import, upgradeSources map[string]struct{}) (*packman.Lockfile, error) {
		if len(imports) != 2 {
			t.Fatalf("len(imports) = %d, want 2", len(imports))
		}
		if _, ok := imports["pack:a"]; !ok {
			t.Fatalf("imports = %#v, want pack:a present", imports)
		}
		if _, ok := imports["pack:b"]; !ok {
			t.Fatalf("imports = %#v, want pack:b present", imports)
		}
		if len(upgradeSources) != 1 {
			t.Fatalf("len(upgradeSources) = %d, want 1", len(upgradeSources))
		}
		if _, ok := upgradeSources["https://example.com/a.git"]; !ok {
			t.Fatalf("upgradeSources = %#v, want a.git", upgradeSources)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/a.git": {Version: "1.1.0", Commit: "aaaa"},
				"https://example.com/b.git": {Version: "2.0.0", Commit: "bbbb"},
			},
		}, nil
	}
	syncImports = func(_ string, _ map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if mode != packman.InstallUpgrade {
			t.Fatalf("mode = %v, want InstallUpgrade", mode)
		}
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportUpgrade(dir, "a", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if len(lock.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(lock.Packs))
	}
}

func TestDoImportUpgradeTargetedUsesSelectiveUpgradeForSharedSource(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "./frontend"

[rigs.imports.shared]
source = "https://example.com/shared.git"
version = "<2.0"
`)
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.shared]
source = "https://example.com/shared.git"
version = ">=1.0"
`)

	prevSelective := syncImportsSelective
	t.Cleanup(func() { syncImportsSelective = prevSelective })
	syncImportsSelective = func(_ string, imports map[string]config.Import, upgradeSources map[string]struct{}) (*packman.Lockfile, error) {
		if len(imports) != 2 {
			t.Fatalf("len(imports) = %d, want 2", len(imports))
		}
		if got := imports["pack:shared"].Version; got != ">=1.0" {
			t.Fatalf("pack:shared version = %q, want >=1.0", got)
		}
		if got := imports["rig:frontend:shared"].Version; got != "<2.0" {
			t.Fatalf("rig:frontend:shared version = %q, want <2.0", got)
		}
		if len(upgradeSources) != 1 {
			t.Fatalf("len(upgradeSources) = %d, want 1", len(upgradeSources))
		}
		if _, ok := upgradeSources["https://example.com/shared.git"]; !ok {
			t.Fatalf("upgradeSources = %#v, want shared.git", upgradeSources)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/shared.git": {Version: "1.5.0", Commit: "bbbb"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportUpgrade(dir, "shared", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if got := lock.Packs["https://example.com/shared.git"].Version; got != "1.5.0" {
		t.Fatalf("Version = %q, want %q", got, "1.5.0")
	}
}

func TestDoImportUpgradeRejectsPathImportTarget(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.local]
source = "../packs/local"
`)

	var stdout, stderr bytes.Buffer
	code := doImportUpgrade(dir, "local", &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected path import upgrade to fail")
	}
	if !strings.Contains(stderr.String(), "is a path import and cannot be upgraded") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoImportUpgradeTargetsDefaultRigImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
transitive = false
`)

	prevSelective := syncImportsSelective
	t.Cleanup(func() { syncImportsSelective = prevSelective })
	syncImportsSelective = func(_ string, imports map[string]config.Import, upgradeSources map[string]struct{}) (*packman.Lockfile, error) {
		if _, ok := imports["default-rig:worker"]; !ok {
			t.Fatalf("imports = %#v, want default-rig:worker", imports)
		}
		if len(upgradeSources) != 1 {
			t.Fatalf("len(upgradeSources) = %d, want 1", len(upgradeSources))
		}
		if _, ok := upgradeSources["https://example.com/worker.git"]; !ok {
			t.Fatalf("upgradeSources = %#v, want worker.git", upgradeSources)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://example.com/worker.git": {Version: "3.2.0", Commit: "worker"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportUpgrade(dir, "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Upgraded import "worker"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDoImportRemoveTargetsDefaultRigImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
`)

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		if _, ok := imports["default-rig:worker"]; ok {
			t.Fatalf("imports = %#v, did not expect default-rig:worker", imports)
		}
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	defaults, err := config.LoadRootPackDefaultRigImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadRootPackDefaultRigImports: %v", err)
	}
	if len(defaults) != 0 {
		t.Fatalf("defaults = %#v, want empty", defaults)
	}
}

func TestDoImportAddRejectsReservedDefaultRigPrefix(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://example.com/worker.git", "default-rig:worker", "^1.0", &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected reserved prefix import add to fail")
	}
	if !strings.Contains(stderr.String(), "reserved prefix") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoImportListShowsDirectAndTransitive(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, "[pack]\nname = \"demo\"\nschema = 1\n\n[imports.tools]\nsource = \"https://example.com/tools.git\"\nversion = \"^1.4\"\n")
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.4.2", Commit: "aaaa"},
			"https://example.com/base.git":  {Version: "2.0.0", Commit: "bbbb"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tools\thttps://example.com/tools.git\t^1.4\t1.4.2") {
		t.Fatalf("missing direct import line:\n%s", out)
	}
	if !strings.Contains(out, "(transitive)\thttps://example.com/base.git\t\t2.0.0") {
		t.Fatalf("missing transitive line:\n%s", out)
	}
}

func TestDoImportListShowsPathImportsInFlatOutput(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.local]
source = "../packs/local"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs:  map[string]packman.LockedPack{},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "local\t../packs/local\t\t(path)") {
		t.Fatalf("unexpected flat output:\n%s", stdout.String())
	}
}

func TestDoImportListShowsCityImportOverrideForRootPackImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	writeCityToml(t, dir, `[workspace]
name = "demo"

[imports.tools]
source = "packs/tools"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs:  map[string]packman.LockedPack{},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tools\tpacks/tools\t\t(path)") {
		t.Fatalf("missing city override path import:\n%s", out)
	}
	if strings.Contains(out, "https://example.com/tools.git") {
		t.Fatalf("output still shows root pack remote source:\n%s", out)
	}
}

func TestDoImportListShowsDefaultRigImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
transitive = false
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/worker.git": {Version: "3.1.0", Commit: "worker"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "default-rig:worker\thttps://example.com/worker.git\t^3.0\t3.1.0") {
		t.Fatalf("missing default-rig import line:\n%s", stdout.String())
	}
}

func TestDoImportListTreeShowsDependencyGraph(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCmdCachedPackGit(t)

	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.4.2", Commit: "aaaa"},
			"https://example.com/base.git":  {Version: "2.0.0", Commit: "bbbb"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	stageCmdCachedPack(t, "https://example.com/tools.git", "aaaa", `
[pack]
name = "tools"
schema = 1

[imports.base]
source = "https://example.com/base.git"
version = "^2.0"
`)
	stageCmdCachedPack(t, "https://example.com/base.git", "bbbb", `
[pack]
name = "base"
schema = 1
`)

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tools 1.4.2 (^1.4) - https://example.com/tools.git") {
		t.Fatalf("missing direct tree node:\n%s", out)
	}
	if !strings.Contains(out, "  base 2.0.0 (^2.0) - https://example.com/base.git") {
		t.Fatalf("missing nested tree node:\n%s", out)
	}
}

func TestDoImportListTreeShowsPathImports(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.local]
source = "../packs/local"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs:  map[string]packman.LockedPack{},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "local (path) - ../packs/local") {
		t.Fatalf("unexpected tree output:\n%s", stdout.String())
	}
}

func TestDoImportAddWritesRigScopedImportToCityToml(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"local\"\nschema = 1\n")
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "./frontend"
`)
	writePackToml(t, dir, `
[pack]
name = "demo"
schema = 1

[imports.shared]
source = "https://example.com/shared.git"
version = "^1.0"
`)

	prevRigFlag := rigFlag
	prevSync := syncImports
	rigFlag = "frontend"
	t.Cleanup(func() {
		rigFlag = prevRigFlag
		syncImports = prevSync
	})

	var syncedSources []string
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		for _, imp := range imports {
			syncedSources = append(syncedSources, imp.Source)
		}
		sort.Strings(syncedSources)
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "./packs/local", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("len(Rigs) = %d, want 1", len(cfg.Rigs))
	}
	if _, ok := cfg.Rigs[0].Imports["local"]; !ok {
		t.Fatalf("rig imports = %#v, want local", cfg.Rigs[0].Imports)
	}
	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if strings.Contains(string(cityData), "path = ") {
		t.Fatalf("city.toml should not retain machine-local rig path:\n%s", string(cityData))
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != "./frontend" {
		t.Fatalf("site binding rigs = %#v, want frontend path", binding.Rigs)
	}

	packCfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := packCfg.Imports["shared"]; !ok {
		t.Fatalf("pack imports = %#v, want shared", packCfg.Imports)
	}
	if _, ok := packCfg.Imports["local"]; ok {
		t.Fatalf("pack imports = %#v, local import should remain rig-scoped", packCfg.Imports)
	}

	wantSources := []string{"./packs/local", "https://example.com/shared.git"}
	if strings.Join(syncedSources, ",") != strings.Join(wantSources, ",") {
		t.Fatalf("synced sources = %v, want %v", syncedSources, wantSources)
	}
}

func TestDoImportRemoveDeletesRigScopedImportOnlyFromCityToml(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "./frontend"

[rigs.imports.sidecar]
source = "https://example.com/sidecar.git"
version = "^2.0"
`)
	writePackToml(t, dir, `
[pack]
name = "demo"
schema = 1

[imports.shared]
source = "https://example.com/shared.git"
version = "^1.0"
`)

	prevRigFlag := rigFlag
	prevSync := syncImports
	rigFlag = "frontend"
	t.Cleanup(func() {
		rigFlag = prevRigFlag
		syncImports = prevSync
	})

	var syncedSources []string
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		for _, imp := range imports {
			syncedSources = append(syncedSources, imp.Source)
		}
		sort.Strings(syncedSources)
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportRemove(fsys.OSFS{}, dir, "sidecar", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if _, ok := cfg.Rigs[0].Imports["sidecar"]; ok {
		t.Fatalf("rig imports = %#v, want sidecar removed", cfg.Rigs[0].Imports)
	}

	packCfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := packCfg.Imports["shared"]; !ok {
		t.Fatalf("pack imports = %#v, want shared preserved", packCfg.Imports)
	}

	wantSources := []string{"https://example.com/shared.git"}
	if strings.Join(syncedSources, ",") != strings.Join(wantSources, ",") {
		t.Fatalf("synced sources = %v, want %v", syncedSources, wantSources)
	}
}

func TestDoImportListWithRigShowsOnlyRigScopedClosure(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCmdCachedPackGit(t)

	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "./frontend"

[rigs.imports.sidecar]
source = "https://example.com/sidecar.git"
version = "^2.0"
`)
	writePackToml(t, dir, `
[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/sidecar.git": {Version: "2.1.0", Commit: "sidecar"},
			"https://example.com/helper.git":  {Version: "0.9.0", Commit: "helper"},
			"https://example.com/tools.git":   {Version: "1.4.2", Commit: "tools"},
			"https://example.com/base.git":    {Version: "3.0.0", Commit: "base"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	stageCmdCachedPack(t, "https://example.com/sidecar.git", "sidecar", `
[pack]
name = "sidecar"
schema = 1

[imports.helper]
source = "https://example.com/helper.git"
version = "^0.9"
`)
	stageCmdCachedPack(t, "https://example.com/helper.git", "helper", `
[pack]
name = "helper"
schema = 1
`)
	stageCmdCachedPack(t, "https://example.com/tools.git", "tools", `
[pack]
name = "tools"
schema = 1

[imports.base]
source = "https://example.com/base.git"
version = "^3.0"
`)
	stageCmdCachedPack(t, "https://example.com/base.git", "base", `
[pack]
name = "base"
schema = 1
`)

	prevRigFlag := rigFlag
	rigFlag = "frontend"
	t.Cleanup(func() { rigFlag = prevRigFlag })

	var stdout, stderr bytes.Buffer
	code := doImportList(dir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"sidecar\thttps://example.com/sidecar.git\t^2.0\t2.1.0",
		"(transitive)\thttps://example.com/helper.git\t\t0.9.0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"tools\t", "https://example.com/base.git"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("output should stay scoped to rig imports, got:\n%s", out)
		}
	}
}

func TestDoImportAddFindsRigByPathRelativeToCity(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "./frontend"
`)
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"local\"\nschema = 1\n")

	prevRigFlag := rigFlag
	prevSync := syncImports
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rigFlag = "./frontend"
	t.Cleanup(func() {
		rigFlag = prevRigFlag
		syncImports = prevSync
		if chdirErr := os.Chdir(cwd); chdirErr != nil {
			t.Fatalf("restoring cwd: %v", chdirErr)
		}
	})
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatalf("Chdir(%s): %v", outsideDir, err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "./packs/local", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if _, ok := cfg.Rigs[0].Imports["local"]; !ok {
		t.Fatalf("rig imports = %#v, want local", cfg.Rigs[0].Imports)
	}
}

func TestDoImportAddFindsRigByMigratedSiteBindingPath(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, `
[workspace]
name = "demo"

[[rigs]]
name = "frontend"
`)
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, dir, []config.Rig{{
		Name: "frontend",
		Path: filepath.Join(dir, "frontend"),
	}}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, localPack, "[pack]\nname = \"local\"\nschema = 1\n")

	prevRigFlag := rigFlag
	prevSync := syncImports
	rigFlag = "./frontend"
	t.Cleanup(func() {
		rigFlag = prevRigFlag
		syncImports = prevSync
	})
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "./packs/local", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if _, ok := cfg.Rigs[0].Imports["local"]; !ok {
		t.Fatalf("rig imports = %#v, want local", cfg.Rigs[0].Imports)
	}
	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if strings.Contains(string(cityData), "path = ") {
		t.Fatalf("city.toml should not gain machine-local rig path:\n%s", string(cityData))
	}
}

func TestDoImportWhyExplainsTransitiveImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCmdCachedPackGit(t)

	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `
[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.4.2", Commit: "tools"},
			"https://example.com/base.git":  {Version: "2.0.0", Commit: "base"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	stageCmdCachedPack(t, "https://example.com/tools.git", "tools", `
[pack]
name = "tools"
schema = 1

[imports.base]
source = "https://example.com/base.git"
version = "^2.0"
`)
	stageCmdCachedPack(t, "https://example.com/base.git", "base", `
[pack]
name = "base"
schema = 1
`)

	var stdout, stderr bytes.Buffer
	code := doImportWhy(dir, "base", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"base is present transitively",
		"source: https://example.com/base.git",
		"via: tools -> base",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestDoImportWhyShowsDefaultRigImport(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[defaults.rig.imports.worker]
source = "https://example.com/worker.git"
version = "^3.0"
transitive = false
`)
	if err := packman.WriteLockfile(fsys.OSFS{}, dir, &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/worker.git": {Version: "3.1.0", Commit: "worker"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportWhy(dir, "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "worker is a direct import") || !strings.Contains(out, "https://example.com/worker.git") {
		t.Fatalf("unexpected why output:\n%s", out)
	}
}

func TestDoImportAddRemoteEndToEndLoadsImportedPack(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	repo := initImportBarePackRepo(t, "remote-pack", "v1.2.3", `
[pack]
name = "remote-pack"
schema = 1

[[agent]]
name = "polecat"
scope = "city"
`)
	source := "file://" + repo

	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, cityDir, source, "", "^1.2", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.QualifiedName() == "remote-pack.polecat" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected imported agent remote-pack.polecat, got %#v", cfg.Agents)
	}
}

func TestDoImportAddRemoteSubpathEndToEndLoadsImportedPack(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	repo := initImportBarePackRepo(t, "mono", "v1.2.3", `
[pack]
name = "root"
schema = 1
`)
	workDir := t.TempDir()
	mustGitImport(t, "", "clone", repo, workDir)
	if err := os.MkdirAll(filepath.Join(workDir, "packs", "base"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "packs", "base", "pack.toml"), []byte(`
[pack]
name = "base"
schema = 1

[[agent]]
name = "scout"
scope = "city"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitImport(t, workDir, "add", "-A")
	mustGitImport(t, workDir, "commit", "-m", "add subpath pack")
	mustGitImport(t, workDir, "tag", "-f", "-a", "v1.2.3", "-m", "release v1.2.3")
	mustGitImport(t, workDir, "push", "--force", "--tags", "origin", "HEAD:master")

	source := "file://" + repo + "//packs/base"
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, cityDir, source, "", "^1.2", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.QualifiedName() == "base.scout" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected imported agent base.scout, got %#v", cfg.Agents)
	}
}

func TestDoImportAddRemoteSHAPinEndToEndLoadsImportedPack(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	repo := initImportBarePackRepo(t, "sha-pack", "", `
[pack]
name = "sha-pack"
schema = 1

[[agent]]
name = "sentinel"
scope = "city"
`)
	commit := gitOutputImport(t, repo, "rev-parse", "HEAD")
	source := "file://" + repo

	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, cityDir, source, "", "sha:"+commit, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfgFile, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if got := cfgFile.Imports["sha-pack"].Version; got != "sha:"+commit {
		t.Fatalf("Version = %q, want %q", got, "sha:"+commit)
	}

	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if got := lock.Packs[source].Commit; got != commit {
		t.Fatalf("Commit = %q, want %q", got, commit)
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.QualifiedName() == "sha-pack.sentinel" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected imported agent sha-pack.sentinel, got %#v", cfg.Agents)
	}
}

func TestDoImportAddLocalGitRepoWithoutTagsWritesSHAPin(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	packDir := initImportWorktreePackRepo(t, "local-sha-pack", "", `
[pack]
name = "local-sha-pack"
schema = 1

[[agent]]
name = "sentinel"
scope = "city"
`)
	relSource, err := filepath.Rel(dir, packDir)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, relSource, "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfgFile, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	imp := cfgFile.Imports["local-sha-pack"]
	resolvedPackDir, err := filepath.EvalSymlinks(packDir)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := "file://" + filepath.ToSlash(resolvedPackDir)
	if imp.Source != wantSource {
		t.Fatalf("Source = %q, want %q", imp.Source, wantSource)
	}
	commit := gitOutputImport(t, packDir, "rev-parse", "HEAD")
	if got, want := imp.Version, "sha:"+commit; got != want {
		t.Fatalf("Version = %q, want %q", got, want)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if got := lock.Packs[wantSource].Commit; got != commit {
		t.Fatalf("Commit = %q, want %q", got, commit)
	}
}

func TestDoImportAddLocalGitRepoWithTagsWritesDefaultConstraint(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	packDir := initImportWorktreePackRepo(t, "local-tagged-pack", "v1.2.3", `
[pack]
name = "local-tagged-pack"
schema = 1

[[agent]]
name = "scout"
scope = "city"
`)
	relSource, err := filepath.Rel(dir, packDir)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, relSource, "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfgFile, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	imp := cfgFile.Imports["local-tagged-pack"]
	resolvedPackDir, err := filepath.EvalSymlinks(packDir)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := "file://" + filepath.ToSlash(resolvedPackDir)
	if imp.Source != wantSource {
		t.Fatalf("Source = %q, want %q", imp.Source, wantSource)
	}
	if got, want := imp.Version, "^1.2"; got != want {
		t.Fatalf("Version = %q, want %q", got, want)
	}
}

// The pack rewrite key-loss guards must not refuse legitimate packs: every
// section the gc binary recognizes has to decode cleanly into the structs the
// writers round-trip (initPackConfig for gc agent suspend, cityPackManifest for
// import-manifest rewrites). This pins zero false positives for the known pack
// schema, so the guards fire only on genuinely unknown keys. The fixture must
// include the sections where the reduced structs historically diverged from
// config.PackConfig — the legacy [agents] alias and [[pricing]] — because those
// are the keys the guards would otherwise flag as unrecognized.
func TestGuardPackRewriteKeyLossAcceptsKnownPackSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	packPath := filepath.Join(dir, "pack.toml")
	src := `[pack]
name = "full-pack"
schema = 1
version = "1.0.0"
description = "exercises known sections"

[imports.gastown]
source = "https://example/gastown.git"
version = "^1.2"

[agent_defaults]
provider = "claude"

[agents]
append_fragments = ["legacy-alias"]

[defaults.rig.imports.review]
source = "https://example/review.git"

[providers.claude]
base = "builtin:claude"

[[pricing]]
provider = "claude"
model = "claude-opus-4-8"
last_verified = "2026-06-01"

[pricing.tier]
prompt_usd_per_1m = 15.0
completion_usd_per_1m = 75.0

[[agent]]
name = "worker"
provider = "claude"
`
	if err := os.WriteFile(packPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.GuardRewriteKeyLoss[initPackConfig](fsys.OSFS{}, packPath); err != nil {
		t.Fatalf("GuardRewriteKeyLoss[initPackConfig] = %v, want nil for known schema", err)
	}
	if err := config.GuardRewriteKeyLoss[cityPackManifest](fsys.OSFS{}, packPath); err != nil {
		t.Fatalf("GuardRewriteKeyLoss[cityPackManifest] = %v, want nil for known schema", err)
	}
}

// An import-manifest rewrite must round-trip [[pricing]] (which compose.go
// consumes) and canonicalize the legacy [agents] alias into [agent_defaults],
// rather than refusing the rewrite or silently dropping recognized data. This
// pins the cityPackManifest reduced struct as a faithful superset of the pack
// schema it guards.
func TestWriteCityPackManifestPreservesPricingAndFoldsAgentsAlias(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 1

[agents]
append_fragments = ["legacy-alias"]

[[pricing]]
provider = "claude"
model = "claude-opus-4-8"
last_verified = "2026-06-01"

[pricing.tier]
prompt_usd_per_1m = 15.0
completion_usd_per_1m = 75.0
`)

	manifest, err := loadCityPackManifestFS(fs, "/city")
	if err != nil {
		t.Fatalf("loadCityPackManifestFS: %v", err)
	}
	if err := writeCityPackManifest(fs, "/city", manifest); err != nil {
		t.Fatalf("writeCityPackManifest: %v", err)
	}

	out := string(fs.Files["/city/pack.toml"])
	for _, want := range []string{
		"[[pricing]]",
		`provider = "claude"`,
		`model = "claude-opus-4-8"`,
		`last_verified = "2026-06-01"`,
		"prompt_usd_per_1m",
		"completion_usd_per_1m",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rewritten pack.toml dropped %q:\n%s", want, out)
		}
	}
	// The alias value survives, canonicalized under [agent_defaults]; the legacy
	// [agents] table name is not re-emitted.
	if !strings.Contains(out, "[agent_defaults]") || !strings.Contains(out, `append_fragments = ["legacy-alias"]`) {
		t.Fatalf("rewritten pack.toml dropped the [agents] alias value:\n%s", out)
	}
	if strings.Contains(out, "[agents]") {
		t.Fatalf("rewritten pack.toml still contains the legacy [agents] table:\n%s", out)
	}
}

func TestDoImportAddBareGitHubSourceDefaultsVersion(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})
	resolveImportVersion = func(source, _ string) (packman.ResolvedVersion, error) {
		if source != "github.com/example/tools" {
			t.Fatalf("ResolveVersion source = %q", source)
		}
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"github.com/example/tools": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "github.com/example/tools", "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	imp := cfg.Imports["tools"]
	if imp.Source != "github.com/example/tools" {
		t.Fatalf("Source = %q", imp.Source)
	}
	if imp.Version != "^1.4" {
		t.Fatalf("Version = %q, want %q", imp.Version, "^1.4")
	}
}

func TestDefaultImportVersionForSourceFallsBackToSHAWhenTagsAbsent(t *testing.T) {
	prevResolve := resolveImportVersion
	prevHead := resolveImportHeadCommit
	t.Cleanup(func() {
		resolveImportVersion = prevResolve
		resolveImportHeadCommit = prevHead
	})
	resolveImportVersion = func(source, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{}, fmt.Errorf("%w for %q", packman.ErrNoSemverTags, source)
	}
	resolveImportHeadCommit = func(_ string) (string, error) {
		return "deadbeef", nil
	}

	got, err := defaultImportVersionForSource("github.com/example/tools")
	if err != nil {
		t.Fatalf("defaultImportVersionForSource: %v", err)
	}
	if got != "sha:deadbeef" {
		t.Fatalf("got %q, want %q", got, "sha:deadbeef")
	}
}

func TestDeriveImportName(t *testing.T) {
	if got := deriveImportName("https://github.com/example/tools.git"); got != "tools" {
		t.Fatalf("deriveImportName = %q", got)
	}
}

func TestHasRepositoryRefInSource(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/example/repo.git":                  false,
		"file:///tmp/repo.git//packs/base":                     false,
		"file:///tmp/repo.git#main":                            true,
		"https://github.com/example/repo/tree/main/packs/base": false,
		"git@github.com:example/repo.git#main":                 true,
	}
	for input, want := range cases {
		if got := hasRepositoryRefInSource(input); got != want {
			t.Fatalf("hasRepositoryRefInSource(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestResolveImportRootFallsBackToStandalonePackDir(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo-pack"
schema = 1
`)

	t.Chdir(dir)

	prevCityFlag := cityFlag
	prevRigFlag := rigFlag
	cityFlag = ""
	rigFlag = ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if got != want {
		t.Fatalf("resolveImportRoot() = %q, want %q", got, want)
	}
}

func TestFindNearestImportRootSkipsRuntimeOnlyDirs(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	rigDir := filepath.Join(cityDir, "rigs", "work")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(rigDir, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	root, ok, err := findNearestImportRoot(nested)
	if err != nil {
		t.Fatalf("findNearestImportRoot: %v", err)
	}
	if !ok {
		t.Fatal("expected the walk to reach the city root")
	}
	if root != cityDir {
		t.Fatalf("root = %q, want city %q (bare .gc/ dirs must not be import roots)", root, cityDir)
	}
}

func TestFindNearestImportRootStopsAtCeiling(t *testing.T) {
	clearGCEnv(t)
	base := t.TempDir()
	writePackToml(t, base, "[pack]\nname = \"demo\"\nschema = 1\n")
	ceiling := filepath.Join(base, "ceiling")
	nested := filepath.Join(ceiling, "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CEILING_DIRECTORIES", ceiling)

	root, ok, err := findNearestImportRoot(nested)
	if err != nil {
		t.Fatalf("findNearestImportRoot: %v", err)
	}
	if ok {
		t.Fatalf("root = %q, want no match above the ceiling", root)
	}
}

func TestResolveImportRootPrefersNearestPackUnderCity(t *testing.T) {
	clearGCEnv(t)
	resetFlags(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, "[pack]\nname = \"demo\"\nschema = 1\n")
	packDir := filepath.Join(cityDir, "packs", "tools")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackToml(t, packDir, "[pack]\nname = \"tools\"\nschema = 1\n")
	nested := filepath.Join(packDir, "prompts")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	setCwd(t, nested)

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	want, err := filepath.EvalSymlinks(packDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", packDir, err)
	}
	if got != want {
		t.Fatalf("resolveImportRoot() = %q, want nearest pack %q", got, want)
	}
}

func TestResolveImportRootRuntimeOnlyAncestorResolvesRegisteredRigCity(t *testing.T) {
	clearGCEnv(t)
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "import-city")
	base := canonicalTestPath(t.TempDir())
	rigDir := filepath.Join(base, "workrepo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(rigDir, "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Bound the marker walk inside the fixture so stray pack.toml/city.toml
	// files in shared temp ancestors cannot hijack the test.
	t.Setenv("GC_CEILING_DIRECTORIES", base)
	registerRigBindingForResolution(t, gcHome, cityPath, "import-city", "workrepo", rigDir)
	setCwd(t, nested)

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	assertSameTestPath(t, got, cityPath)
}

func TestResolveImportRootHonorsRigFlagOverNearestPack(t *testing.T) {
	clearGCEnv(t)
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "rigflag-city")
	rigDir := filepath.Join(canonicalTestPath(t.TempDir()), "workrepo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, gcHome, cityPath, "rigflag-city", "workrepo", rigDir)

	packDir := t.TempDir()
	writePackToml(t, packDir, "[pack]\nname = \"bystander\"\nschema = 1\n")
	setCwd(t, packDir)
	rigFlag = "workrepo"

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	assertSameTestPath(t, got, cityPath)
}

func TestResolveImportRootHonorsGCDirOverNearestPack(t *testing.T) {
	clearGCEnv(t)
	resetFlags(t)
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := setupCity(t, "gcdir-city")
	packDir := t.TempDir()
	writePackToml(t, packDir, "[pack]\nname = \"bystander\"\nschema = 1\n")
	setCwd(t, packDir)
	t.Setenv("GC_DIR", cityPath)

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	assertSameTestPath(t, got, cityPath)
}

func TestResolveImportRootGCCityEnvWinsOverNearestPack(t *testing.T) {
	clearGCEnv(t)
	resetFlags(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	packDir := t.TempDir()
	writePackToml(t, packDir, "[pack]\nname = \"bystander\"\nschema = 1\n")
	setCwd(t, packDir)
	t.Setenv("GC_CITY", cityDir)

	got, err := resolveImportRoot()
	if err != nil {
		t.Fatalf("resolveImportRoot: %v", err)
	}
	assertSameTestPath(t, got, cityDir)
}

func TestImportAddCommandWorksInStandalonePackDir(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo-pack"
schema = 1
`)

	t.Chdir(dir)

	prevCityFlag := cityFlag
	prevRigFlag := rigFlag
	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	cityFlag = ""
	rigFlag = ""
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})

	var stdout, stderr bytes.Buffer
	cmd := newImportCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"add", "https://github.com/example/tools.git"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := cfg.Imports["tools"]; !ok {
		t.Fatalf("imports = %#v, want tools", cfg.Imports)
	}
}

func TestImportAddCommandIgnoresInheritedLiveEnv(t *testing.T) {
	external := t.TempDir()
	writeCityToml(t, external, "[workspace]\nname = \"external\"\n")
	writePackToml(t, external, `[pack]
name = "external"
schema = 1
`)
	externalPackPath := filepath.Join(external, "pack.toml")
	externalPackBefore, err := os.ReadFile(externalPackPath)
	if err != nil {
		t.Fatalf("ReadFile(external pack.toml): %v", err)
	}
	externalBeadsPath := filepath.Join(external, ".beads", "issues.jsonl")
	if err := os.MkdirAll(filepath.Dir(externalBeadsPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(external .beads): %v", err)
	}
	externalBeadsBefore := []byte(`{"id":"live-1","title":"do not touch"}` + "\n")
	if err := os.WriteFile(externalBeadsPath, externalBeadsBefore, 0o600); err != nil {
		t.Fatalf("WriteFile(external beads): %v", err)
	}

	t.Setenv("GC_CITY_PATH", external)
	t.Setenv("GC_CITY_ROOT", external)
	t.Setenv("BEADS_DB_PATH", externalBeadsPath)
	t.Setenv("DOLT_ROOT_PATH", filepath.Join(external, "dolt-home"))
	clearGCEnv(t)

	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo-pack"
schema = 1
`)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	prevCityFlag := cityFlag
	prevRigFlag := rigFlag
	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	cityFlag = ""
	rigFlag = ""
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})

	var stdout, stderr bytes.Buffer
	cmd := newImportCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"add", "https://github.com/example/tools.git"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := cfg.Imports["tools"]; !ok {
		t.Fatalf("imports = %#v, want tools in standalone pack", cfg.Imports)
	}
	externalPackAfter, err := os.ReadFile(externalPackPath)
	if err != nil {
		t.Fatalf("ReadFile(external pack.toml after import): %v", err)
	}
	if !bytes.Equal(externalPackAfter, externalPackBefore) {
		t.Fatalf("external pack.toml was modified:\n%s", string(externalPackAfter))
	}
	externalBeadsAfter, err := os.ReadFile(externalBeadsPath)
	if err != nil {
		t.Fatalf("ReadFile(external beads after import): %v", err)
	}
	if !bytes.Equal(externalBeadsAfter, externalBeadsBefore) {
		t.Fatalf("external beads store was modified:\n%s", string(externalBeadsAfter))
	}
}

func TestImportAddCommandAcceptsCityFlagForStandalonePackDir(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writePackToml(t, dir, `[pack]
name = "demo-pack"
schema = 1
`)

	prevCityFlag := cityFlag
	prevRigFlag := rigFlag
	prevResolve := resolveImportVersion
	prevConstraint := defaultImportConstraint
	prevSync := syncImports
	cityFlag = dir
	rigFlag = ""
	resolveImportVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncImports = func(cityRoot string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != dir {
			t.Fatalf("cityRoot = %q, want %q", cityRoot, dir)
		}
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
			},
		}, nil
	}
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
		resolveImportVersion = prevResolve
		defaultImportConstraint = prevConstraint
		syncImports = prevSync
	})

	var stdout, stderr bytes.Buffer
	cmd := newImportCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"add", "https://github.com/example/tools.git"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := cfg.Imports["tools"]; !ok {
		t.Fatalf("imports = %#v, want tools", cfg.Imports)
	}
}

//nolint:unparam // test helper keeps the explicit city.toml payload at call sites.
func writeCityToml(t *testing.T, dir, content string) {
	t.Helper()
	if err := (fsys.OSFS{}).WriteFile(filepath.Join(dir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func writePackToml(t *testing.T, dir, content string) {
	t.Helper()
	if err := (fsys.OSFS{}).WriteFile(filepath.Join(dir, "pack.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}

func initImportBarePackRepo(t *testing.T, name, tag, packToml string) string {
	t.Helper()
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	bareDir := filepath.Join(dir, name+".git")

	mustGitImport(t, "", "init", workDir)
	writeTestFile := func(rel, content string) {
		t.Helper()
		path := filepath.Join(workDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile("pack.toml", packToml)
	mustGitImport(t, workDir, "add", "-A")
	mustGitImport(t, workDir, "commit", "-m", "initial")
	if tag != "" {
		mustGitImport(t, workDir, "tag", "-a", tag, "-m", "release "+tag)
	}
	mustGitImport(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

func initImportWorktreePackRepo(t *testing.T, name, tag, packToml string) string {
	t.Helper()
	dir := t.TempDir()
	workDir := filepath.Join(dir, name)

	mustGitImport(t, "", "init", workDir)
	writeTestFile := func(rel, content string) {
		t.Helper()
		path := filepath.Join(workDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile("pack.toml", packToml)
	mustGitImport(t, workDir, "add", "-A")
	mustGitImport(t, workDir, "commit", "-m", "initial")
	if tag != "" {
		mustGitImport(t, workDir, "tag", "-a", tag, "-m", "release "+tag)
	}
	return workDir
}

func stageCmdCachedPack(t *testing.T, source, commit, packToml string) {
	t.Helper()
	path, err := packman.RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, ".packman-test-commit"), []byte(commit), 0o644); err != nil {
		t.Fatalf("WriteFile(.packman-test-commit): %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}

func stubCmdCachedPackGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := `#!/bin/sh
set -eu
dir="$PWD"
while [ "$#" -gt 0 ]; do
	case "$1" in
		-C)
			dir="$2"
			shift 2
			;;
		-c)
			shift 2
			;;
		*)
			break
			;;
	esac
done
case "${1:-}" in
	rev-parse)
		if [ "${2:-}" = "HEAD" ]; then
			cat "$dir/.packman-test-commit"
			exit 0
		fi
		;;
	status)
		if [ "${2:-}" = "--porcelain" ]; then
			if [ -f "$dir/.packman-test-dirty" ]; then
				printf '%s\n' ' M pack.toml'
			fi
			exit 0
		fi
		;;
esac
printf 'unexpected git command: %s\n' "$*" >&2
exit 1
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fake git): %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func mustGitImport(t *testing.T, dir string, args ...string) {
	t.Helper()
	_, err := gitOutputImportE(t, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func gitOutputImport(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutputImportE(t, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func gitOutputImportE(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// TestLocalGitRepoRootIgnoresPoisonedGitEnv proves localGitRepoRoot resolves
// the toplevel of the requested targetDir even when git-locating environment
// variables point at an unrelated repository. Running gc inside a pre-commit
// hook or nested worktree tooling exports GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE
// for the parent repo; the import probe must strip those so the local import
// target is resolved from its own working directory.
func TestLocalGitRepoRootIgnoresPoisonedGitEnv(t *testing.T) {
	repo := t.TempDir()
	mustGitImport(t, repo, "init")
	// Capture git's own resolved toplevel before poisoning so symlink
	// normalization matches localGitRepoRoot's later result.
	wantRoot := gitOutputImport(t, repo, "rev-parse", "--show-toplevel")

	poison := t.TempDir()
	mustGitImport(t, poison, "init")
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	got, ok, err := localGitRepoRoot(repo)
	if err != nil {
		t.Fatalf("localGitRepoRoot with poisoned git env: %v", err)
	}
	if !ok {
		t.Fatalf("localGitRepoRoot ok = false, want true for an initialized repo")
	}
	if got != wantRoot {
		t.Fatalf("localGitRepoRoot = %q, want %q (must ignore poisoned GIT_DIR)", got, wantRoot)
	}
}

// TestDefaultImportHeadCommitIgnoresPoisonedGitEnv proves defaultImportHeadCommit
// resolves the HEAD of the requested source under a poisoned git environment.
// `git ls-remote <url>` lists refs from the explicit URL, so the sanitized
// environment is defense-in-depth (against config injection or a future change
// that relies on local repo discovery); the resolved HEAD must still be the
// requested source's, never the leaked parent's.
func TestDefaultImportHeadCommitIgnoresPoisonedGitEnv(t *testing.T) {
	repo := t.TempDir()
	mustGitImport(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGitImport(t, repo, "add", ".")
	mustGitImport(t, repo, "commit", "-m", "initial")
	wantHead := gitOutputImport(t, repo, "rev-parse", "HEAD")

	// A second repo whose HEAD differs from the source.
	poison := t.TempDir()
	mustGitImport(t, poison, "init")
	if err := os.WriteFile(filepath.Join(poison, "p.txt"), []byte("poison\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(poison): %v", err)
	}
	mustGitImport(t, poison, "add", ".")
	mustGitImport(t, poison, "commit", "-m", "poison")
	poisonHead := gitOutputImport(t, poison, "rev-parse", "HEAD")
	if poisonHead == wantHead {
		t.Fatalf("poison HEAD unexpectedly equals source HEAD %s", wantHead)
	}
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	got, err := defaultImportHeadCommit(repo)
	if err != nil {
		t.Fatalf("defaultImportHeadCommit with poisoned git env: %v", err)
	}
	if got != wantHead {
		t.Fatalf("defaultImportHeadCommit = %q, want %q (must resolve the requested source)", got, wantHead)
	}
}
