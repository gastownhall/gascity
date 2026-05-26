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

func TestPackRegistryCommandsDoNotImplicitlySeedDefaultRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)

	var stdout, stderr bytes.Buffer
	if code := doPackRegistryList(false, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No pack registries configured.") {
		t.Fatalf("list output = %q, want no registries message", stdout.String())
	}
	if _, err := os.Stat(packregistry.ConfigPath(home)); !os.IsNotExist(err) {
		t.Fatalf("registry list should not seed registries.toml, stat err = %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doPackRegistrySearch("gastown", "", false, 50, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("search code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No registry packs found.") {
		t.Fatalf("search output = %q, want no packs message", stdout.String())
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

func TestPackRegistryAddNoValidateInvalidatesReusedNameCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
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
	if code := doPackRegistryAdd("main", source, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("add gascity-packs registry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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
