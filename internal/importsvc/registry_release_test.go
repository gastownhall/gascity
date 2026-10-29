package importsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/testutil"
)

// useRegistryHome points the Gas City home (registries + repo cache) at a fresh
// temp dir with one registry, "fixture", reading catalogPath.
func useRegistryHome(t *testing.T, catalogPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	if err := packregistry.SaveConfig(home, packregistry.Config{
		Registries: []packregistry.Registry{{Name: "fixture", Source: catalogPath}},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

func newCityDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), "[pack]\nname = \"demo\"\nschema = 1\n")
	return dir
}

func runFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.RunGit(t, dir, append([]string{"-c", "user.email=test@example.com", "-c", "user.name=Test", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
}

func writeTree(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// packRepo is a local git repository holding one pack at packs/demo with two
// commits. Like gascity-packs it carries a repository-level semver tag that
// belongs to no pack, so tag resolution of a pack version answers wrongly.
type packRepo struct {
	dir              string
	source           string
	commit1, commit2 string
	hash1, hash2     string
}

func newPackRepo(t *testing.T) packRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, dir, "init", "-q", "-b", "main")
	writeTree(t, dir, "packs/demo/pack.toml", "[pack]\nname = \"demo\"\nschema = 2\n", 0o644)
	writeTree(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho one\n", 0o755)
	runFixtureGit(t, dir, "add", ".")
	runFixtureGit(t, dir, "commit", "-q", "-m", "demo 0.1.0")
	commit1 := runFixtureGit(t, dir, "rev-parse", "HEAD")
	writeTree(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho two\n", 0o755)
	runFixtureGit(t, dir, "commit", "-q", "-am", "demo 0.2.0")
	commit2 := runFixtureGit(t, dir, "rev-parse", "HEAD")
	writeTree(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho unreleased\n", 0o755)
	runFixtureGit(t, dir, "commit", "-q", "-am", "repository release, no pack release")
	runFixtureGit(t, dir, "tag", "v0.4.0")

	hash := func(commit string) string {
		h, err := packregistry.PackContentHash(dir, commit, "packs/demo")
		if err != nil {
			t.Fatalf("PackContentHash: %v", err)
		}
		return h
	}
	return packRepo{
		dir:     dir,
		source:  "file://" + dir + "//packs/demo",
		commit1: commit1, commit2: commit2,
		hash1: hash(commit1), hash2: hash(commit2),
	}
}

// writeCatalog writes a one-pack registry catalog for repo. hash1 overrides the
// 0.1.0 release hash (to simulate a registry entry that does not match).
func writeCatalog(t *testing.T, repo packRepo, hash1 string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.toml")
	body := fmt.Sprintf(`schema = 1

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

  [[pack.release]]
    version = "0.2.0"
    ref = "main"
    commit = %q
    hash = %q
    description = "Second."

  [[pack.release]]
    version = "0.3.0"
    ref = "main"
    commit = %q
    hash = %q
    description = "Pulled."
    withdrawn = true
    withdrawn_reason = "broken prompts"
`, repo.source, repo.commit1, hash1, repo.commit2, repo.hash2, repo.commit2, repo.hash2)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The fix end to end with no stubs: a registry pack's --version resolves to
// the release (never the repository's v0.4.0 tag). As for a git-tag source,
// the manifest keeps the constraint as given and the lock records the
// resolved release version and commit, verified against the release hash.
// Without --version the import defaults to a caret constraint on the newest
// release, not on the repository tag.
func TestAddImportRegistryVersionResolvesToRelease(t *testing.T) {
	repo := newPackRepo(t)
	catalog := writeCatalog(t, repo, repo.hash1)

	cases := []struct {
		constraint, wantManifest, wantVersion, wantCommit string
	}{
		{"0.1.0", "0.1.0", "0.1.0", repo.commit1},
		{">=0.1.0", ">=0.1.0", "0.2.0", repo.commit2},
		{"^0.1", "^0.1", "0.1.0", repo.commit1},
		{"", "^0.2", "0.2.0", repo.commit2},
	}
	for _, tc := range cases {
		t.Run("version="+tc.constraint, func(t *testing.T) {
			useRegistryHome(t, catalog)
			city := newCityDir(t)
			res, err := AddImport(fsys.OSFS{}, city, repo.source, "demo", tc.constraint)
			if err != nil {
				t.Fatalf("AddImport --version %q: %v", tc.constraint, err)
			}
			wantRelease := "fixture:demo " + tc.wantVersion
			if res.Version != tc.wantManifest || res.RegistryRelease != wantRelease {
				t.Fatalf("result = %+v, want version %q locked to %s", res, tc.wantManifest, wantRelease)
			}
			manifest, err := loadCityPackManifest(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := manifest.Imports["demo"]; got.Source != repo.source || got.Version != tc.wantManifest {
				t.Fatalf("pack.toml import = %+v, want version %q", got, tc.wantManifest)
			}
			lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := lock.Packs[repo.source]; got.Version != tc.wantVersion || got.Commit != tc.wantCommit {
				t.Fatalf("packs.lock entry = %+v, want %s@%s", got, tc.wantVersion, tc.wantCommit)
			}
		})
	}
}

func TestAddImportRegistryVersionRejectsHashMismatch(t *testing.T) {
	repo := newPackRepo(t)
	wrong := "sha256:" + strings.Repeat("0", 64)
	useRegistryHome(t, writeCatalog(t, repo, wrong))
	city := newCityDir(t)

	_, err := AddImport(fsys.OSFS{}, city, repo.source, "demo", "0.1.0")
	if !errors.Is(err, ErrVersionResolveFailed) || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("AddImport err = %v, want ErrVersionResolveFailed hash mismatch", err)
	}
	manifest, err := loadCityPackManifest(fsys.OSFS{}, city)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Imports["demo"]; ok {
		t.Fatalf("pack.toml gained an import despite the hash mismatch: %+v", manifest.Imports)
	}
	if _, err := os.Stat(filepath.Join(city, packman.LockfileName)); !os.IsNotExist(err) {
		t.Fatalf("packs.lock written despite the hash mismatch (stat err %v)", err)
	}
}

func TestAddImportRegistryVersionWithoutMatchingReleaseListsAvailable(t *testing.T) {
	repo := newPackRepo(t)
	useRegistryHome(t, writeCatalog(t, repo, repo.hash1))

	for constraint, want := range map[string]string{
		"0.9.0": `no release matching "0.9.0" (available: 0.1.0, 0.2.0)`,
		"0.3.0": "0.3.0 was withdrawn: broken prompts",
		// The repository's v0.4.0 tag belongs to no pack: a registry pack
		// never falls through to tag resolution.
		"0.4.0": `no release matching "0.4.0"`,
	} {
		_, err := AddImport(fsys.OSFS{}, newCityDir(t), repo.source, "demo", constraint)
		if !errors.Is(err, ErrVersionResolveFailed) || !strings.Contains(err.Error(), want) {
			t.Errorf("AddImport --version %s err = %v, want %q", constraint, err, want)
		}
	}
}

// A source no registry publishes keeps the git-tag path: the constraint is
// written as given and the lock pins the matching tag's commit.
func TestAddImportNonRegistrySourceStillResolvesGitTags(t *testing.T) {
	repo := newPackRepo(t)
	useRegistryHome(t, writeCatalog(t, repo, repo.hash1))

	tagged := filepath.Join(t.TempDir(), "tagged")
	if err := os.MkdirAll(tagged, 0o755); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, tagged, "init", "-q", "-b", "main")
	writeTree(t, tagged, "pack.toml", "[pack]\nname = \"tools\"\nschema = 2\n", 0o644)
	runFixtureGit(t, tagged, "add", ".")
	runFixtureGit(t, tagged, "commit", "-q", "-m", "tools 1.2.0")
	runFixtureGit(t, tagged, "tag", "v1.2.0")
	tagCommit := runFixtureGit(t, tagged, "rev-parse", "HEAD")
	writeTree(t, tagged, "pack.toml", "[pack]\nname = \"tools\"\nschema = 2\n# 2.0\n", 0o644)
	runFixtureGit(t, tagged, "commit", "-q", "-am", "tools 2.0.0")
	runFixtureGit(t, tagged, "tag", "v2.0.0")

	source := "file://" + tagged
	city := newCityDir(t)
	res, err := AddImport(fsys.OSFS{}, city, source, "tools", "^1.2")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	if res.Version != "^1.2" || res.RegistryRelease != "" {
		t.Fatalf("result = %+v, want the constraint kept and no registry release", res)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Packs[source]; got.Commit != tagCommit || got.Version != "1.2.0" {
		t.Fatalf("packs.lock entry = %+v, want tag v1.2.0 at %s", got, tagCommit)
	}
}
