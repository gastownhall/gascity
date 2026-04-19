package bootstrap

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestEnsureBootstrapPopulatesCacheAndWritesImplicitFile(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, `
[pack]
name = "registry"
version = "0.1.0"
schema = 1

[[agent]]
name = "runner"
scope = "city"
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("reading implicit-import.toml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `[imports."registry"]`) {
		t.Fatalf("implicit-import.toml missing registry entry:\n%s", text)
	}
	if !strings.Contains(text, `version = "0.1.0"`) {
		t.Fatalf("implicit-import.toml missing version:\n%s", text)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry := entries["registry"]
	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Fatalf("bootstrap cache missing pack.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap cache should not contain .git, stat err = %v", err)
	}
}

func TestBootstrapPacksDoNotIncludeLegacyImportPack(t *testing.T) {
	for _, entry := range BootstrapPacks {
		if entry.Name == "import" {
			t.Fatalf("BootstrapPacks should not include legacy import pack: %#v", entry)
		}
	}
}

// Regression for the PR #846 upgrade-path blocker: a pre-existing
// implicit-import.toml written by an older release carried
// [imports.import] pointing at the retired gc-import pack. Without
// pruning, the loader would splice that entry forever and cache
// eviction would surface as an undiagnosable missing-pack error.
func TestEnsureBootstrapPrunesRetiredImportEntry(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, `
[pack]
name = "registry"
version = "0.1.0"
schema = 1
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	// Simulate the pre-upgrade state: implicit-import.toml already has the
	// retired [imports.import] entry plus a user-authored custom entry.
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.import]
source = "github.com/gastownhall/gc-import"
version = "0.2.0"
commit = "deadbeef"

[imports.custom]
source = "example.com/custom"
version = "1.0.0"
commit = "cafebabe"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	if _, ok := entries["import"]; ok {
		t.Fatalf("retired [imports.import] entry was not pruned: %+v", entries)
	}
	if _, ok := entries["custom"]; !ok {
		t.Fatalf("user-authored custom entry must survive pruning: %+v", entries)
	}
	if _, ok := entries["registry"]; !ok {
		t.Fatalf("registry bootstrap entry missing after bootstrap: %+v", entries)
	}
}

// Retired-pack matching is conservative: a hand-edited entry that
// reuses the retired name but points at a different source must not
// be pruned, since the user explicitly authored it.
func TestEnsureBootstrapPreservesHandEditedEntryUnderRetiredName(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, `
[pack]
name = "registry"
version = "0.1.0"
schema = 1
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.import]
source = "example.com/my-fork"
version = "9.9.9"
commit = "feedface"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	imp, ok := entries["import"]
	if !ok {
		t.Fatalf("hand-edited [imports.import] with non-matching source must survive: %+v", entries)
	}
	if imp.Source != "example.com/my-fork" {
		t.Fatalf("hand-edited source was rewritten: %+v", imp)
	}
}

func TestEnsureBootstrapPreservesExistingEntriesAndIsIdempotent(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, `
[pack]
name = "registry"
version = "0.1.0"
schema = 1
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.custom]
source = "example.com/custom"
version = "1.0.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("first EnsureBootstrap: %v", err)
	}
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	wantTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(implicitPath, wantTime, wantTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("second EnsureBootstrap: %v", err)
	}
	info, err := os.Stat(implicitPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", implicitPath, err)
	}
	if !info.ModTime().Equal(wantTime) {
		t.Fatalf("implicit-import.toml modtime changed on idempotent bootstrap: got %v, want %v", info.ModTime(), wantTime)
	}

	entries, err := readImplicitFile(implicitPath)
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	if _, ok := entries["custom"]; !ok {
		t.Fatal("custom implicit entry was removed")
	}
	if _, ok := entries["registry"]; !ok {
		t.Fatal("registry bootstrap entry missing")
	}
}

func TestEnsureBootstrapEmbedsCorePackSkills(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "core",
		Source:   "github.com/gastownhall/gc-core",
		Version:  "0.1.0",
		AssetDir: "packs/core",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry, ok := entries["core"]
	if !ok {
		t.Fatalf("core entry missing from implicit-import.toml: %v", entries)
	}
	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)

	wantSkills := []string{
		"gc-agents",
		"gc-city",
		"gc-dashboard",
		"gc-dispatch",
		"gc-mail",
		"gc-rigs",
		"gc-work",
	}
	for _, name := range wantSkills {
		skillPath := filepath.Join(cacheDir, "skills", name, "SKILL.md")
		if _, err := os.Stat(skillPath); err != nil {
			t.Fatalf("embedded core skill %s missing from cache: %v", name, err)
		}
	}

	workSkill, err := os.ReadFile(filepath.Join(cacheDir, "skills", "gc-work", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading gc-work SKILL.md: %v", err)
	}
	text := string(workSkill)
	wantFrontmatter := []string{
		"---\n",
		"name: gc-work\n",
		"description: Finding, creating, claiming, and closing work items (beads)\n",
	}
	for _, needle := range wantFrontmatter {
		if !strings.Contains(text, needle) {
			t.Fatalf("gc-work SKILL.md missing frontmatter %q:\n%s", needle, text)
		}
	}
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("gc-work SKILL.md should start with frontmatter delimiter, got:\n%s", text)
	}

	packToml, err := os.ReadFile(filepath.Join(cacheDir, "pack.toml"))
	if err != nil {
		t.Fatalf("reading core pack.toml: %v", err)
	}
	if !strings.Contains(string(packToml), `name = "core"`) {
		t.Fatalf("core pack.toml missing name:\n%s", packToml)
	}
}

func TestEnsureBootstrapAllowsConcurrentCallers(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, `
[pack]
name = "registry"
version = "0.1.0"
schema = 1
`)
	commandsDir := filepath.Join(assetsRoot, "packs", "registry", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commandsDir, "sync.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- EnsureBootstrap(gcHome)
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureBootstrap under concurrency: %v", err)
		}
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry, ok := entries["registry"]
	if !ok {
		t.Fatalf("missing registry entry after concurrent bootstrap: %v", entries)
	}

	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)
	for _, rel := range []string{"pack.toml", "commands/sync.sh"} {
		if _, err := os.Stat(filepath.Join(cacheDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("bootstrap cache missing %s after concurrent bootstrap: %v", rel, err)
		}
	}
	stageGlobs, err := filepath.Glob(cacheDir + ".tmp-*")
	if err != nil {
		t.Fatalf("Glob(stage tmp): %v", err)
	}
	if len(stageGlobs) != 0 {
		t.Fatalf("bootstrap temp dirs should be cleaned up, found %v", stageGlobs)
	}
	fileGlobs, err := filepath.Glob(filepath.Join(gcHome, "implicit-import.toml.tmp-*"))
	if err != nil {
		t.Fatalf("Glob(implicit tmp): %v", err)
	}
	if len(fileGlobs) != 0 {
		t.Fatalf("implicit-import temp files should be cleaned up, found %v", fileGlobs)
	}
}

func TestEnsureBootstrapFailsWhenAssetMissing(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/missing",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	if err := EnsureBootstrap(t.TempDir()); err == nil {
		t.Fatal("EnsureBootstrap should fail for missing asset")
	}
}

func TestEnsureBootstrapFailsWhenPackTomlMissing(t *testing.T) {
	assetsRoot := t.TempDir()
	path := filepath.Join(assetsRoot, "packs", "registry", "commands")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "sync.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	err := EnsureBootstrap(t.TempDir())
	if err == nil {
		t.Fatal("EnsureBootstrap should fail when pack.toml is missing")
	}
	if !strings.Contains(err.Error(), "missing pack.toml") {
		t.Fatalf("EnsureBootstrap error = %v, want missing pack.toml", err)
	}
}

func writeBootstrapPackAsset(t *testing.T, root, packToml string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash("packs/registry"))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(strings.TrimSpace(packToml)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ fs.FS = os.DirFS(".")
