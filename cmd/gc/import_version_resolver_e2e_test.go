package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// dummyReleaseHash is a syntactically valid (but meaningless) sha256 catalog
// hash; resolution does not verify it, and the catalog validator only checks
// its shape.
const dummyReleaseHash = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// buildTidesPackRepoWithStaleTag creates a bare pack repo whose stale git tag
// (v9.0.0) points at an OLD commit and whose HEAD is the commit the registry
// publishes as release 0.2.1. It returns the file:// source plus both commits,
// so a test can prove which namespace resolution consulted.
func buildTidesPackRepoWithStaleTag(t *testing.T, dir string) (source, releaseCommit, staleCommit string) {
	t.Helper()
	work := filepath.Join(dir, "work")
	mustGitImport(t, "", "init", work)
	writeFile := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "pack.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(`
[pack]
name = "tides"
schema = 1

[[agent]]
name = "stale-scout"
scope = "city"
`)
	mustGitImport(t, work, "add", "-A")
	mustGitImport(t, work, "commit", "-m", "stale")
	mustGitImport(t, work, "tag", "-a", "v9.0.0", "-m", "stale tag")
	staleCommit = gitOutputImport(t, work, "rev-parse", "HEAD")

	writeFile(`
[pack]
name = "tides"
schema = 1

[[agent]]
name = "tide-scout"
scope = "city"
`)
	mustGitImport(t, work, "add", "-A")
	mustGitImport(t, work, "commit", "-m", "release 0.2.1")
	releaseCommit = gitOutputImport(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(dir, "tides.git")
	mustGitImport(t, "", "clone", "--bare", work, bare)
	return "file://" + bare, releaseCommit, staleCommit
}

// seedTidesReleaseCatalog caches a "main" registry catalog under home that maps
// pack "tides" release 0.2.1 to releaseCommit.
func seedTidesReleaseCatalog(t *testing.T, home, source, releaseCommit string) {
	t.Helper()
	writeEmptyRegistryConfig(t, home)
	catalog := fmt.Sprintf(`schema = 1

[[pack]]
name = "tides"
description = "Tide planning helpers."
source = %q
source_kind = "git"

  [[pack.release]]
  version = "0.2.1"
  ref = "v0.2.1"
  commit = "%s"
  hash = "%s"
  description = "Registry release."
`, source, releaseCommit, dummyReleaseHash)
	catalogDir := writeRegistryCatalog(t, catalog)
	var out, errb bytes.Buffer
	if code := doPackRegistryAdd("main", catalogDir, false, false, &out, &errb); code != 0 {
		t.Fatalf("doPackRegistryAdd: code=%d stderr=%q", code, errb.String())
	}
}

func setupRegistryImportCity(t *testing.T) (cityDir, source, releaseCommit, staleCommit string) {
	t.Helper()
	clearGCEnv(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	gcHome := filepath.Join(home, ".gc")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", gcHome)

	// Install the registry-aware resolver exactly as run() does, restoring it
	// afterward so package state stays clean (B30/B33: set-once-then-read; this
	// test must not run in parallel while the resolver is installed).
	packman.SetVersionResolver(newRegistryVersionResolver())
	t.Cleanup(func() { packman.SetVersionResolver(nil) })

	source, releaseCommit, staleCommit = buildTidesPackRepoWithStaleTag(t, dir)
	seedTidesReleaseCatalog(t, gcHome, source, releaseCommit)

	cityDir = filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	return cityDir, source, releaseCommit, staleCommit
}

// TestDoImportAddRegistryVersionPrefersReleaseCommitOverStaleTag is the direct
// #3710 regression: ">=0.2.1" must pin the registry's published release commit,
// not the higher-but-stale git tag v9.0.0 that tag resolution would otherwise
// select.
func TestDoImportAddRegistryVersionPrefersReleaseCommitOverStaleTag(t *testing.T) {
	cityDir, source, releaseCommit, staleCommit := setupRegistryImportCity(t)

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, cityDir, source, "", ">=0.2.1", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	locked := lock.Packs[source]
	if locked.Commit != releaseCommit {
		t.Fatalf("locked commit = %q, want registry release %q (stale tag commit was %q)",
			locked.Commit, releaseCommit, staleCommit)
	}
	if locked.Version != "0.2.1" {
		t.Fatalf("locked version = %q, want registry release 0.2.1", locked.Version)
	}
}

// TestDoImportAddRegistryVersionFailsClosedWhenNoReleaseMatches proves the
// fail-closed contract: a registry-owned source with no release satisfying the
// constraint errors out (naming available versions) rather than silently
// degrading to a git tag.
func TestDoImportAddRegistryVersionFailsClosedWhenNoReleaseMatches(t *testing.T) {
	cityDir, source, _, _ := setupRegistryImportCity(t)

	var stdout, stderr bytes.Buffer
	code := doImportAdd(fsys.OSFS{}, cityDir, source, "", ">=9.9.9", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("code=0; want non-zero fail-closed exit. stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "0.2.1") {
		t.Fatalf("stderr %q must name the available release 0.2.1", stderr.String())
	}

	// No lock entry should have been written for the unresolved source.
	lock, err := packman.ReadLockfile(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if _, ok := lock.Packs[source]; ok {
		t.Fatalf("lock unexpectedly recorded %q after a fail-closed resolution", source)
	}
}
