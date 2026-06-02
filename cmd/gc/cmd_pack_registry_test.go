package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	writeEmptyRegistryConfig(t, home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("local", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryList(false, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "local") || !strings.Contains(stdout.String(), catalogDir) {
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
	if !strings.Contains(stdout.String(), "local:lighthouse") || !strings.Contains(stdout.String(), "1.2.0") {
		t.Fatalf("unexpected show output: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryRemove("local", false, &stdout, &stderr); code != 0 {
		t.Fatalf("remove code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "registry-cache", "local")); err != nil {
		t.Fatalf("registry cache pruned during remove, stat err=%v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryRefresh("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh after remove code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "registry-cache", "local")); !os.IsNotExist(err) {
		t.Fatalf("registry cache not pruned by refresh, stat err=%v", err)
	}
}

func TestPackRegistryCommandsPreRegisterDefaultRegistryOnVanillaHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryList(false, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), packregistry.DefaultRegistryName) || !strings.Contains(stdout.String(), packregistry.DefaultRegistrySource) {
		t.Fatalf("list output = %q, want default main registry", stdout.String())
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Registries) != 1 || cfg.Registries[0] != packregistry.DefaultRegistry() {
		t.Fatalf("registries = %+v, want default registry", cfg.Registries)
	}

	if err := packregistry.WriteCatalogCache(home, packregistry.DefaultRegistryName, []byte(packRegistryTestCatalog)); err != nil {
		t.Fatalf("WriteCatalogCache(default main): %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("light", "", false, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lighthouse") || strings.Contains(stderr.String(), "cache unavailable") {
		t.Fatalf("search output = stdout=%q stderr=%q, want default main cached results without cache warning", stdout.String(), stderr.String())
	}
}

func TestPackRegistryAddFreshHomePreservesDefaultRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("local", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Registries) != 2 {
		t.Fatalf("registries = %+v, want default and local", cfg.Registries)
	}
	got := map[string]string{}
	for _, reg := range cfg.Registries {
		got[reg.Name] = reg.Source
	}
	if got[packregistry.DefaultRegistryName] != packregistry.DefaultRegistrySource {
		t.Fatalf("default registry source = %q, want %q", got[packregistry.DefaultRegistryName], packregistry.DefaultRegistrySource)
	}
	if got["local"] != catalogDir {
		t.Fatalf("local registry source = %q, want %q", got["local"], catalogDir)
	}
}

func TestPackRegistryRemoveMainFreshHomeWritesExplicitEmptyConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryRemove(packregistry.DefaultRegistryName, false, &stdout, &stderr); code != 0 {
		t.Fatalf("remove code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := packregistry.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Registries) != 0 {
		t.Fatalf("registries = %+v, want explicit empty config", cfg.Registries)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryList(false, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No pack registries configured.") {
		t.Fatalf("list output = %q, want explicit empty config to remain unseeded", stdout.String())
	}
}

func TestPackRegistryShowBareNameAmbiguous(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	mainDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	otherDir := writeRegistryCatalog(t, packRegistryOtherCatalog)
	writeEmptyRegistryConfig(t, home)

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
	writeEmptyRegistryConfig(t, home)
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

func TestPackRegistryAddNoValidateInvalidatesReusedNameCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryRemove("main", false, &stdout, &stderr); code != 0 {
		t.Fatalf("remove main: %d %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistryAdd("main", filepath.Join(t.TempDir(), "missing"), true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("re-add main --no-validate: %d %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("light", "", false, 50, false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("search reused stale cache stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "lighthouse") {
		t.Fatalf("search returned stale pack from previous source: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no registry caches were available") {
		t.Fatalf("search stderr=%q, want no-cache failure", stderr.String())
	}
}

func TestPackRegistryLatestUsesHighestNonWithdrawnSemver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
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
	writeEmptyRegistryConfig(t, home)
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
	writeEmptyRegistryConfig(t, home)
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

func TestPackRegistrySearchRefreshesMissingCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	catalogDir := writeRegistryCatalog(t, packRegistryTestCatalog)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add main: %d %s", code, stderr.String())
	}
	if err := os.Remove(filepath.Join(home, "registry-cache", "main", "registry.toml")); err != nil {
		t.Fatalf("Remove cache: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("light", "", false, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lighthouse") || strings.Contains(stderr.String(), "cache unavailable") {
		t.Fatalf("search did not refresh missing cache cleanly stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPackRegistrySearchRefreshFallsBackToCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
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
	writeEmptyRegistryConfig(t, home)
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
	writeEmptyRegistryConfig(t, home)
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

func TestPackCommandTreeKeepsRegistryAndLegacySurfacesSeparate(t *testing.T) {
	cmd := newPackCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, args := range [][]string{{"registry", "list"}, {"fetch"}, {"list"}} {
		if found, _, err := cmd.Find(args); err != nil || found == cmd {
			t.Fatalf("gc pack %s not found: found=%v err=%v", strings.Join(args, " "), found, err)
		}
	}
	for _, name := range []string{"add", "remove", "refresh", "search", "show"} {
		if found, _, err := cmd.Find([]string{"registry", name}); err != nil || found == cmd {
			t.Fatalf("gc pack registry %s not found: found=%v err=%v", name, found, err)
		}
	}
}

func TestPackRegistryLiveGascityPacksCatalog(t *testing.T) {
	source := strings.TrimSpace(os.Getenv("GC_TEST_GASCITY_PACKS_REGISTRY"))
	if source == "" {
		t.Skip("set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary")
	}
	home := t.TempDir()
	t.Setenv("GC_HOME", home)

	var stdout, stderr bytes.Buffer
	if source == packregistry.DefaultRegistryName {
		if code := doPackRegistryRefresh(packregistry.DefaultRegistryName, false, &stdout, &stderr); code != 0 {
			t.Fatalf("refresh gascity-packs registry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	} else {
		writeEmptyRegistryConfig(t, home)
		if code := doPackRegistryAdd(packregistry.DefaultRegistryName, source, false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("add gascity-packs registry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("", "main", false, 50, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search gascity-packs registry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	searchOutput := stdout.String()
	for _, name := range []string{"gascity", "gastown", "discord", "github-intake", "slack"} {
		if !strings.Contains(searchOutput, name) {
			t.Fatalf("search output missing %q:\n%s", name, searchOutput)
		}
	}

	for _, name := range []string{"gascity", "gastown", "discord", "github-intake", "slack"} {
		stdout.Reset()
		stderr.Reset()
		if code := doPackRegistryShow("main:"+name, false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("show %s code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "Source:") || !strings.Contains(out, "Latest:") || !strings.Contains(out, "Releases:") {
			t.Fatalf("show %s output missing registry contract fields:\n%s", name, out)
		}
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

func writeEmptyRegistryConfig(t *testing.T, home string) {
	t.Helper()
	if err := packregistry.SaveConfig(home, packregistry.Config{}); err != nil {
		t.Fatalf("SaveConfig(empty): %v", err)
	}
}
