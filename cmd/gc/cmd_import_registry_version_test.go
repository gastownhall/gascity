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
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/testutil"
)

func registryVersionFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.RunGit(t, dir, append([]string{"-c", "user.email=test@example.com", "-c", "user.name=Test", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
}

// The import commands `gc pack registry show` prints must work as printed. The
// fixture mirrors gascity-packs: one repository holding a pack at a subpath,
// versioned only by registry release entries, plus a repository-level tag that
// belongs to no pack (so tag resolution of the pack version cannot succeed).
func TestPackRegistryShowPrintedImportCommandsWork(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(filepath.Join(repo, "packs", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	registryVersionFixtureGit(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "packs", "demo", "pack.toml"), []byte("[pack]\nname = \"demo\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryVersionFixtureGit(t, repo, "add", ".")
	registryVersionFixtureGit(t, repo, "commit", "-q", "-m", "demo 0.1.0")
	registryVersionFixtureGit(t, repo, "tag", "v9.0.0")
	commit := registryVersionFixtureGit(t, repo, "rev-parse", "HEAD")
	hash, err := packregistry.PackContentHash(repo, commit, "packs/demo")
	if err != nil {
		t.Fatalf("PackContentHash: %v", err)
	}
	source := "file://" + repo + "//packs/demo"
	catalogDir := writeRegistryCatalog(t, fmt.Sprintf(`schema = 1

[[pack]]
name = "demo"
description = "Demo pack."
source = %q
source_kind = "git"

  [[pack.release]]
  version = "0.1.0"
  ref = "main"
  commit = %q
  hash = %q
  description = "First."
`, source, commit, hash))

	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	writeEmptyRegistryConfig(t, home)
	var stdout, stderr bytes.Buffer
	if code := doPackRegistryAdd("local", catalogDir, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("registry add code=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	if code := doPackRegistryShow("local:demo", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("registry show code=%d stderr=%q", code, stderr.String())
	}
	printed := map[string]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		for _, label := range []string{"This version or later:", "Exactly this version:"} {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), label); ok {
				printed[label] = strings.TrimSpace(rest)
			}
		}
	}
	if len(printed) != 2 {
		t.Fatalf("show printed %d import commands, want 2:\n%s", len(printed), stdout.String())
	}

	for label, command := range printed {
		t.Run(label, func(t *testing.T) {
			args := shellquote.Split(command)
			if len(args) < 2 || args[0] != "gc" {
				t.Fatalf("printed command %q does not start with gc", command)
			}
			city := t.TempDir()
			writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
			writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

			var out, errOut bytes.Buffer
			if code := run(append([]string{"--city", city}, args[1:]...), &out, &errOut); code != 0 {
				t.Fatalf("%s exited %d\nstdout: %s\nstderr: %s", command, code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "Resolved registry release local:demo 0.1.0 to sha:"+commit) {
				t.Fatalf("stdout does not report the resolved release:\n%s", out.String())
			}
			manifest, err := loadCityPackManifestFS(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := manifest.Imports["demo"]; got.Source != source || got.Version != "sha:"+commit {
				t.Fatalf("pack.toml import = %+v, want %s at sha:%s", got, source, commit)
			}
			lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := lock.Packs[source]; got.Commit != commit {
				t.Fatalf("packs.lock entry = %+v, want commit %s", got, commit)
			}
		})
	}
}
