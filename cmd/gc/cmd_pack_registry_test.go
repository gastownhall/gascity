package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
)

const packRegistryTestCatalog = `schema = 1

[[pack]]
name = "lighthouse"
description = "Harbor-watch checks."
source = "https://packages.example/lighthouse.git"
source_kind = "git"

  [[pack.release]]
  version = "1.2.0"
  ref = "v1.2.0"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Stable release."
`

const packRegistryOtherCatalog = `schema = 1

[[pack]]
name = "lighthouse"
description = "Another lighthouse."
source = "https://packages.example/other-lighthouse.git"
source_kind = "git"

  [[pack.release]]
  version = "2.0.0"
  ref = "v2.0.0"
  commit = "89abcdef0123456789abcdef0123456789abcdef"
  hash = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  description = "Other release."
`

const packRegistryUnsortedCatalog = `schema = 1

[[pack]]
name = "tides"
description = "Tide planning helpers."
source = "https://packages.example/tides.git"
source_kind = "git"

  [[pack.release]]
  version = "2.0.0"
  ref = "v2.0.0"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Latest release."

  [[pack.release]]
  version = "3.0.0"
  ref = "v3.0.0"
  commit = "1111111111111111111111111111111111111111"
  hash = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  description = "Withdrawn release."
  withdrawn = true

  [[pack.release]]
  version = "1.0.0"
  ref = "v1.0.0"
  commit = "2222222222222222222222222222222222222222"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Older release."
`

func TestPackRegistryAddListSearchShowRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryList(false, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "main") || !strings.Contains(stdout.String(), catalogDir) {
		t.Fatalf("list output missing registry: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("light", "", false, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lighthouse") || strings.Contains(stderr.String(), "warning") {
		t.Fatalf("unexpected search output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("lighthouse", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "main:lighthouse") || !strings.Contains(stdout.String(), "1.2.0") {
		t.Fatalf("unexpected show output: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryRemove("main", false, &stdout, &stderr); code != 0 {
		t.Fatalf("remove code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "registry-cache", "main")); err != nil {
		t.Fatalf("registry cache pruned during remove, stat err=%v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryRefresh("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh after remove code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "registry-cache", "main")); !os.IsNotExist(err) {
		t.Fatalf("registry cache not pruned by refresh, stat err=%v", err)
	}
}

func TestPackRegistryShowBareNameAmbiguous(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	mainDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	otherDir := writeRegistryCatalog(t, packRegistryOtherCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", mainDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryAdd("other", otherDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add other: %d %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("lighthouse", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("show ambiguous succeeded stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "ambiguous") || !strings.Contains(stderr.String(), "main:lighthouse") || !strings.Contains(stderr.String(), "other:lighthouse") {
		t.Fatalf("ambiguous error missing choices: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("main:lighthouse", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("show qualified code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestPackRegistryAddDuplicateDoesNotPoisonCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	mainDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	otherDir := writeRegistryCatalog(t, packRegistryOtherCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", mainDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryAdd("main", otherDir, false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("duplicate add succeeded stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	catalog, _, err := packregistry.ReadCachedCatalog(home, "main")
	if err != nil {
		t.Fatalf("ReadCachedCatalog: %v", err)
	}
	if got := catalog.Packs[0].Description; got != "Harbor-watch checks." {
		t.Fatalf("cache was poisoned by duplicate add, description=%q", got)
	}
}

func TestPackRegistryLatestUsesHighestNonWithdrawnSemver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryUnsortedCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("tides", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Latest:      2.0.0") {
		t.Fatalf("latest did not use highest non-withdrawn semver: %q", stdout.String())
	}
}

func TestPackRegistrySearchPartialReachabilityWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	goodDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("good", goodDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add good: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryAdd("bad", filepath.Join(t.TempDir(), "missing"), true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add bad: %d %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("", "", true, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lighthouse") || !strings.Contains(stderr.String(), "warning: registry bad refresh failed") {
		t.Fatalf("partial output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPackRegistrySearchAllCachesUnavailableFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("bad", filepath.Join(t.TempDir(), "missing"), true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add bad: %d %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("", "", false, 50, false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("search succeeded with no caches stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no registry caches were available") {
		t.Fatalf("missing all-cache failure stderr=%q", stderr.String())
	}
}

func TestPackRegistrySearchRefreshFallsBackToCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	if err := os.Remove(filepath.Join(catalogDir, "registry.toml")); err != nil {
		t.Fatalf("Remove registry.toml: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("LIGHT", "", true, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lighthouse") || !strings.Contains(stderr.String(), "refresh failed") {
		t.Fatalf("fallback output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPackRegistryShowUnqualifiedFailsClosedWithUnavailableRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	goodDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("good", goodDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add good: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryAdd("bad", filepath.Join(t.TempDir(), "missing"), true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add bad: %d %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("lighthouse", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("show unqualified succeeded stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unavailable") {
		t.Fatalf("missing unavailable failure stderr=%q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryShow("good:lighthouse", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("show qualified code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestPackRegistrySearchWarnsOnStaleCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv("GC_REGISTRY_FRESHNESS", "1s")
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add: %d %s", code, stderr.String())
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(packregistry.CachePath(home, "main"), old, old); err != nil {
		t.Fatalf("Chtimes cache: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("", "", false, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "cache is stale") {
		t.Fatalf("stale warning missing: %q", stderr.String())
	}
}

func TestPackCommandTreeKeepsRegistryAndDependencySurfacesSeparate(t *testing.T) {
	cmd := newPackCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"show", "outdated"} {
		if found, _, err := cmd.Find([]string{name}); err == nil && found != cmd {
			t.Fatalf("gc pack unexpectedly exposes dependency verb %q", name)
		}
	}
	for _, name := range []string{"add", "remove", "sync", "check", "upgrade", "why"} {
		if found, _, err := cmd.Find([]string{name}); err != nil || found == cmd {
			t.Fatalf("gc pack %s not found: found=%v err=%v", name, found, err)
		}
	}
	if found, _, err := cmd.Find([]string{"registry", "list"}); err != nil || found == cmd {
		t.Fatalf("gc pack registry list not found: found=%v err=%v", found, err)
	}
	for _, name := range []string{"list", "add", "remove", "refresh", "search", "show"} {
		if found, _, err := cmd.Find([]string{"registry", name}); err != nil || found == cmd {
			t.Fatalf("gc pack registry %s not found: found=%v err=%v", name, found, err)
		}
	}
}

func TestPackCheckCommandWrapsImportCheck(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, city, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(cityRoot string, imports map[string]config.Import) (*packman.CheckReport, error) {
		if cityRoot != city {
			t.Fatalf("cityRoot = %q, want %q", cityRoot, city)
		}
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("imports = %#v, want pack:tools", imports)
		}
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"pack", "check"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack check code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Pack dependency state OK: 1 remote pack dependency source(s) checked") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = doImportCheck(city, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("import check code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Import state OK: 1 remote import(s) checked") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
}

func TestPackAddCommandWrapsImportAddWithoutChangingImportCommand(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{
			Schema: packman.LockfileSchema,
			Packs: map[string]packman.LockedPack{
				"https://github.com/example/tools.git": {Version: "1.0.0", Commit: "abc123"},
			},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"pack", "add", "https://github.com/example/tools.git", "--name", "tools", "--version", "^1.0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added pack dependency") || strings.Contains(stdout.String(), "Added import") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if got := cfg.Imports["tools"].Source; got != "https://github.com/example/tools.git" {
		t.Fatalf("imports.tools.source = %q", got)
	}

	stdout.Reset()
	stderr.Reset()
	code = doImportAdd(fsys.OSFS{}, city, "https://github.com/example/other.git", "other", "^1.0", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("import add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added import") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
}

func TestPackAddRegistrySelectorWritesConcreteSourceAndRegistryLockMetadata(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

	source, commit, hash := writeGitPackRepo(t)
	catalogDir := writeRegistryCatalog(t, strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(packRegistryTestCatalog,
		`https://packages.example/lighthouse.git`, source),
		`0123456789abcdef0123456789abcdef01234567`, commit),
		`sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7`, hash))
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("registry add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := run([]string{"pack", "add", "main:lighthouse", "--name", "lighthouse"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "selected via registry main:lighthouse") || strings.Contains(stdout.String(), "registry:main:lighthouse") {
		t.Fatalf("pack add stdout = %q", stdout.String())
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if got := cfg.Imports["lighthouse"].Source; got != source {
		t.Fatalf("durable import source = %q, want concrete source %q", got, source)
	}
	if strings.Contains(cfg.Imports["lighthouse"].Source, "registry:") {
		t.Fatalf("durable import stored registry selector: %+v", cfg.Imports["lighthouse"])
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	locked, ok := lock.Packs[source]
	if !ok {
		t.Fatalf("lock missing concrete source %q: %+v", source, lock.Packs)
	}
	if locked.Registry != "main" || locked.RegistrySource != catalogDir || locked.RegistryPack != "lighthouse" || locked.Hash != hash || locked.Ref != "v1.2.0" {
		t.Fatalf("registry lock metadata = %+v", locked)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"pack", "why", "lighthouse"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack why code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "registry: main:lighthouse") {
		t.Fatalf("pack why stdout missing registry provenance: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "registry source: "+catalogDir) {
		t.Fatalf("pack why stdout missing registry source: %q", stdout.String())
	}
}

func TestPackSyncCommandWrapsImportInstallAndLegacyPackFetchStillLegacy(t *testing.T) {
	clearGCEnv(t)
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_CITY", city)
	writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	lock := &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return lock, nil
	}
	installLockedImports = func(_ string) (*packman.Lockfile, error) {
		return lock, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"pack", "sync"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack sync code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Synced 0 remote pack dependencies") {
		t.Fatalf("pack sync stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"pack", "fetch"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack fetch code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No remote packs configured.") {
		t.Fatalf("legacy pack fetch stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"pack", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pack list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No remote packs configured.") {
		t.Fatalf("legacy pack list stdout = %q", stdout.String())
	}
}

func writeRegistryCatalog(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(registry.toml): %v", err)
	}
	return dir
}

func writeGitPackRepo(t *testing.T) (source, commit, hash string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"lighthouse\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	runGitCmd(t, dir, "init")
	runGitCmd(t, dir, "add", "pack.toml")
	runGitCmd(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	out := runGitCmd(t, dir, "rev-parse", "HEAD")
	hash, err := packman.HashPackTree(dir)
	if err != nil {
		t.Fatalf("HashPackTree: %v", err)
	}
	return "file://" + filepath.ToSlash(dir), strings.TrimSpace(out), hash
}

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
