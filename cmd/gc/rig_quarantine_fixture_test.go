package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

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
		if out, err := exec.Command("git", append([]string{"-C", seed}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", seed, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	commit := strings.TrimSpace(string(out))
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
