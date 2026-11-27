package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// rigQuarantineCity is a city with a healthy rig "good" (agent worker from a
// local pack import) and a rig "bad" whose locked remote import's cache has
// local worktree changes (issue #6656 trigger 3).
type rigQuarantineCity struct {
	cityDir string
	goodDir string
	badDir  string
}

func newRigQuarantineCity(t *testing.T) rigQuarantineCity {
	t.Helper()
	root := t.TempDir()
	gcHome := filepath.Join(root, "gc-home")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Cleanup(config.ResetRemoteCacheValidationCache)

	commit := seedDirtyBrokenCache(t, root, gcHome)

	writeQuarantineFile(t, root, "goodpack/pack.toml", "[pack]\nname = \"goodpack\"\nschema = 1\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\n")

	c := rigQuarantineCity{
		cityDir: filepath.Join(root, "city"),
		goodDir: filepath.Join(root, "rigs", "good"),
		badDir:  filepath.Join(root, "rigs", "bad"),
	}
	for _, d := range []string{c.goodDir, c.badDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeQuarantineFile(t, c.cityDir, "city.toml", fmt.Sprintf(`[workspace]
name = "quarantine-city"

[beads]
provider = "file"

[[rigs]]
name = "good"
prefix = "gd"

[rigs.imports.tools]
source = "../goodpack"

[[rigs]]
name = "bad"
prefix = "bd"

[rigs.imports.broken]
source = %q
version = "^1.0"
`, quarantineBrokenSource))
	writeQuarantineFile(t, c.cityDir, ".gc/site.toml", quarantineSiteBindings(c.goodDir, c.badDir))
	writeQuarantineFile(t, c.cityDir, "packs.lock", "schema = 1\n"+brokenLockEntry(commit))
	return c
}

const quarantineBrokenSource = "https://github.com/example/broken.git"

// seedDirtyBrokenCache installs quarantineBrokenSource in the repo cache under
// gcHome, then leaves a local edit in it. It returns the locked commit.
func seedDirtyBrokenCache(t *testing.T, scratch, gcHome string) string {
	t.Helper()
	seed := filepath.Join(scratch, "seed")
	writeQuarantineFile(t, seed, "pack.toml", "[pack]\nname = \"broken\"\nschema = 1\n\n[[agent]]\nname = \"brokenworker\"\nscope = \"rig\"\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "pack.toml"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "seed"},
	} {
		runGit(t, seed, args...)
	}
	commit := strings.TrimSpace(runGit(t, seed, "rev-parse", "HEAD"))
	cacheDir := filepath.Join(gcHome, "cache", "repos", config.RepoCacheKey(quarantineBrokenSource, commit))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(seed, cacheDir); err != nil {
		t.Fatal(err)
	}
	writeQuarantineFile(t, cacheDir, "pack.toml", "[pack]\nname = \"tampered\"\nschema = 1\n")
	return commit
}

// quarantineSiteBindings binds rigs good and bad to their directories.
func quarantineSiteBindings(goodDir, badDir string) string {
	return fmt.Sprintf("\n[[rig]]\nname = \"good\"\npath = %q\n\n[[rig]]\nname = \"bad\"\npath = %q\n", goodDir, badDir)
}

// brokenLockEntry is the packs.lock entry pinning quarantineBrokenSource.
func brokenLockEntry(commit string) string {
	return fmt.Sprintf("\n[packs.%q]\nversion = \"1.0.0\"\ncommit = %q\nfetched = \"2026-04-10T00:00:00Z\"\n", quarantineBrokenSource, commit)
}

func writeQuarantineFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
