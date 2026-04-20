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
type = "claude"

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
		`[[commands]]`,
		`session_live = ["echo hi"]`,
		`[imports.tools]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pack.toml missing %q:\n%s", want, text)
		}
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
		`[defaults.rig.imports.gastown]`,
		`source = "./packs/gastown"`,
		`[imports.tools]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pack.toml missing %q:\n%s", want, text)
		}
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

func TestDoImportAddRejectsGitHubTreeSource(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, dir, "https://github.com/example/repo/tree/main/packs/base", "", "", &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected failure for GitHub tree source")
	}
	if !strings.Contains(stderr.String(), "embed refs in --version") {
		t.Fatalf("stderr = %q", stderr.String())
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
	writePackToml(t, localPack, "[pack]\nname = \"local\"\nschema = 1\n")

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
		`[defaults.rig.imports.gastown]`,
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

func TestDoImportListTreeShowsDependencyGraph(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

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

func TestDoImportAddRemoteEndToEndLoadsImportedPack(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)

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
		"https://github.com/example/repo/tree/main/packs/base": true,
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

func TestImportAddCommandWorksInStandalonePackDir(t *testing.T) {
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
		t.Fatalf("imports = %#v, want tools", cfg.Imports)
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
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
