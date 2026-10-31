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

// registryReleaseCatalog renders a one-pack catalog with the given
// version/commit/hash release triples.
func registryReleaseCatalog(source string, releases ...[3]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema = 1\n\n[[pack]]\nname = \"demo\"\ndescription = \"Demo pack.\"\nsource = %q\nsource_kind = \"git\"\n", source)
	for _, r := range releases {
		fmt.Fprintf(&b, "\n  [[pack.release]]\n  version = %q\n  ref = \"main\"\n  commit = %q\n  hash = %q\n  description = \"Release.\"\n", r[0], r[1], r[2])
	}
	return b.String()
}

// The import commands `gc pack registry show` prints must work as printed,
// and `gc import upgrade` must then move to a newer registry release. The
// fixture mirrors gascity-packs: one repository holding a pack at a subpath,
// versioned only by registry release entries, plus a later repository-level
// v0.4.0 tag that belongs to no pack release (tag resolution of the printed
// ">=0.1.0" would lock it).
func TestPackRegistryShowPrintedImportCommandsWork(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(filepath.Join(repo, "packs", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDemo := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "packs", "demo", "pack.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registryVersionFixtureGit(t, repo, "init", "-q", "-b", "main")
	writeDemo("[pack]\nname = \"demo\"\nschema = 2\n")
	registryVersionFixtureGit(t, repo, "add", ".")
	registryVersionFixtureGit(t, repo, "commit", "-q", "-m", "demo 0.1.0")
	commit1 := registryVersionFixtureGit(t, repo, "rev-parse", "HEAD")
	writeDemo("[pack]\nname = \"demo\"\nschema = 2\n# 0.2.0\n")
	registryVersionFixtureGit(t, repo, "commit", "-q", "-am", "demo 0.2.0")
	commit2 := registryVersionFixtureGit(t, repo, "rev-parse", "HEAD")
	writeDemo("[pack]\nname = \"demo\"\nschema = 2\n# unreleased\n")
	registryVersionFixtureGit(t, repo, "commit", "-q", "-am", "repository release, no pack release")
	registryVersionFixtureGit(t, repo, "tag", "v0.4.0")
	hash := func(commit string) string {
		h, err := packregistry.PackContentHash(repo, commit, "packs/demo")
		if err != nil {
			t.Fatalf("PackContentHash: %v", err)
		}
		return h
	}
	source := "file://" + repo + "//packs/demo"
	release1 := [3]string{"0.1.0", commit1, hash(commit1)}
	catalogDir := writeRegistryCatalog(t, registryReleaseCatalog(source, release1))

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

	cities := map[string]string{}
	for label, command := range printed {
		args := shellquote.Split(command)
		if len(args) < 2 || args[0] != "gc" {
			t.Fatalf("printed command %q does not start with gc", command)
		}
		wantVersion := args[len(args)-1]
		city := t.TempDir()
		cities[label] = city
		writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
		writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

		var out, errOut bytes.Buffer
		if code := run(append([]string{"--city", city}, args[1:]...), &out, &errOut); code != 0 {
			t.Fatalf("%s exited %d\nstdout: %s\nstderr: %s", command, code, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), "Locked to registry release local:demo 0.1.0") {
			t.Fatalf("%s: stdout does not report the registry release:\n%s", command, out.String())
		}
		manifest, err := loadCityPackManifestFS(fsys.OSFS{}, city)
		if err != nil {
			t.Fatal(err)
		}
		if got := manifest.Imports["demo"]; got.Source != source || got.Version != wantVersion {
			t.Fatalf("%s: pack.toml import = %+v, want %s at version %q", command, got, source, wantVersion)
		}
		lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
		if err != nil {
			t.Fatal(err)
		}
		if got := lock.Packs[source]; got.Version != "0.1.0" || got.Commit != commit1 {
			t.Fatalf("%s: packs.lock entry = %+v, want release 0.1.0 at %s", command, got, commit1)
		}
	}

	// The registry publishes 0.2.0. Upgrade moves the floating import to it
	// (not to the v0.4.0 tag); the exact import stays on 0.1.0.
	if err := os.WriteFile(filepath.Join(catalogDir, "registry.toml"), []byte(registryReleaseCatalog(source, release1, [3]string{"0.2.0", commit2, hash(commit2)})), 0o644); err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string]string{"This version or later:": commit2, "Exactly this version:": commit1} {
		city := cities[label]
		var out, errOut bytes.Buffer
		if code := run([]string{"--city", city, "import", "upgrade"}, &out, &errOut); code != 0 {
			t.Fatalf("%s gc import upgrade exited %d\nstdout: %s\nstderr: %s", label, code, out.String(), errOut.String())
		}
		lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
		if err != nil {
			t.Fatal(err)
		}
		if got := lock.Packs[source]; got.Commit != want {
			t.Fatalf("%s after upgrade: packs.lock entry = %+v, want commit %s", label, got, want)
		}
	}
}
