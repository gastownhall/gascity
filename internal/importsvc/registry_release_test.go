package importsvc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/packregistry"
)

const realRegistryFixture = "testdata/gascity-packs-registry.toml"

// useRegistryHome points the Gas City home (registries + repo cache) at a fresh
// temp dir with one registry named name reading catalogPath.
func useRegistryHome(t *testing.T, name, catalogPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	if err := packregistry.SaveConfig(home, packregistry.Config{
		Registries: []packregistry.Registry{{Name: name, Source: catalogPath}},
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

func absFixture(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func runFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=test@example.com", "-c", "user.name=Test", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return strings.TrimSpace(string(out))
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
	runFixtureGit(t, dir, "tag", "v3.0.0")

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
// the release commit (not a git tag), the manifest records the sha pin, the
// lock entry matches, and the content hash is verified against the fetched
// checkout.
func TestAddImportRegistryVersionResolvesToReleaseCommit(t *testing.T) {
	repo := newPackRepo(t)
	catalog := writeCatalog(t, repo, repo.hash1)

	cases := []struct {
		constraint, wantCommit, wantRelease string
	}{
		{"0.1.0", repo.commit1, "fixture:demo 0.1.0"},
		{">=0.1.0", repo.commit2, "fixture:demo 0.2.0"},
		{"^0.1", repo.commit1, "fixture:demo 0.1.0"},
	}
	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			useRegistryHome(t, "fixture", catalog)
			city := newCityDir(t)
			res, err := AddImport(fsys.OSFS{}, city, repo.source, "demo", tc.constraint)
			if err != nil {
				t.Fatalf("AddImport --version %s: %v", tc.constraint, err)
			}
			if res.Version != "sha:"+tc.wantCommit || res.RegistryRelease != tc.wantRelease {
				t.Fatalf("result = %+v, want sha:%s from %s", res, tc.wantCommit, tc.wantRelease)
			}
			manifest, err := loadCityPackManifest(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := manifest.Imports["demo"]; got.Source != repo.source || got.Version != "sha:"+tc.wantCommit {
				t.Fatalf("pack.toml import = %+v, want sha:%s", got, tc.wantCommit)
			}
			lock, err := packman.ReadLockfile(fsys.OSFS{}, city)
			if err != nil {
				t.Fatal(err)
			}
			if got := lock.Packs[repo.source]; got.Commit != tc.wantCommit {
				t.Fatalf("packs.lock entry = %+v, want commit %s", got, tc.wantCommit)
			}
		})
	}
}

func TestAddImportRegistryVersionRejectsHashMismatch(t *testing.T) {
	repo := newPackRepo(t)
	wrong := "sha256:" + strings.Repeat("0", 64)
	useRegistryHome(t, "fixture", writeCatalog(t, repo, wrong))
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
	useRegistryHome(t, "fixture", writeCatalog(t, repo, repo.hash1))

	for constraint, want := range map[string]string{
		"0.9.0": `no release matching "0.9.0" (available: 0.1.0, 0.2.0)`,
		"0.3.0": "0.3.0 was withdrawn: broken prompts",
		// The repository's v3.0.0 tag belongs to no pack: a registry pack
		// never falls through to tag resolution.
		"3.0.0": `no release matching "3.0.0"`,
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
	useRegistryHome(t, "fixture", writeCatalog(t, repo, repo.hash1))

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

func TestSelectRegistryReleaseRefusesConflictingRegistries(t *testing.T) {
	release := func(commit string) packregistry.CatalogRelease {
		return packregistry.CatalogRelease{Version: "0.1.0", Commit: commit, Hash: "sha256:" + strings.Repeat("a", 64)}
	}
	matches := []packregistry.PackMatch{
		{Registry: "one", Pack: packregistry.CatalogPack{Name: "demo", Releases: []packregistry.CatalogRelease{release(strings.Repeat("1", 40))}}},
		{Registry: "two", Pack: packregistry.CatalogPack{Name: "demo", Releases: []packregistry.CatalogRelease{release(strings.Repeat("2", 40))}}},
	}
	if _, err := selectRegistryRelease(matches, "0.1.0"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("selectRegistryRelease err = %v, want an ambiguity refusal", err)
	}
	// The same release published identically by both is not ambiguous.
	matches[1].Pack.Releases = []packregistry.CatalogRelease{release(strings.Repeat("1", 40))}
	pin, err := selectRegistryRelease(matches, "0.1.0")
	if err != nil || pin.commit != strings.Repeat("1", 40) {
		t.Fatalf("selectRegistryRelease = %+v, %v; want the shared commit", pin, err)
	}
}

// The releases the reported failure named, resolved against a verbatim copy of
// the real gascity-packs registry. Lock sync and hash verification are stubbed
// here (they need the network for most of these commits); the selection, the
// manifest pin, and the commit handed to lock sync are real.
func TestAddImportResolvesRealRegistryReleases(t *testing.T) {
	useRegistryHome(t, "main", absFixture(t, realRegistryFixture))
	const tree = "https://github.com/gastownhall/gascity-packs/tree/main/"
	cases := []struct {
		pack, constraint, version, commit string
	}{
		{"gascity", "0.1.6", "0.1.6", "3b3b89f2011e06d84459aa7bea1552382f13930a"},
		{"gascity", ">=0.1.6", "0.1.6", "3b3b89f2011e06d84459aa7bea1552382f13930a"},
		{"gascity", "0.1.4", "0.1.4", "99464ed9240b1f6e6b7ab1d351f67016e1a973ff"},
		{"gastown", "^0.1", "0.1.10", "33d3a430a67d1782ad364556cb566bdb01d0afe3"},
		{"gstack", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"compound-engineering", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"bmad", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"superpowers", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"gstack", "0.1.0", "0.1.0", "636fdfaaf54195b293e8e56d5e9c30e4b8d5a9ef"},
	}
	for _, tc := range cases {
		t.Run(tc.pack+"@"+tc.constraint, func(t *testing.T) {
			source := tree + tc.pack
			var synced, verified string
			deps := Deps{
				SyncLock: func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
					for _, imp := range imports {
						if imp.Source == source {
							synced = imp.Version
						}
					}
					return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{
						source: {Version: synced, Commit: strings.TrimPrefix(synced, "sha:")},
					}}, nil
				},
				VerifyRegistryRelease: func(gotSource, commit, _ string) error {
					if gotSource != source {
						t.Errorf("verified source %q, want %q", gotSource, source)
					}
					verified = commit
					return nil
				},
				ResolveVersion: func(_, _, _ string) (packman.ResolvedVersion, error) {
					t.Fatal("a registry pack must not resolve through git tags")
					return packman.ResolvedVersion{}, nil
				},
			}
			res, err := AddImportWith(fsys.OSFS{}, newCityDir(t), source, tc.pack, tc.constraint, deps)
			if err != nil {
				t.Fatalf("AddImportWith: %v", err)
			}
			want := "sha:" + tc.commit
			if res.Version != want || synced != want || verified != tc.commit {
				t.Fatalf("version %q, synced %q, verified %q; want %s", res.Version, synced, verified, want)
			}
			if res.RegistryRelease != "main:"+tc.pack+" "+tc.version {
				t.Fatalf("RegistryRelease = %q, want main:%s %s", res.RegistryRelease, tc.pack, tc.version)
			}
		})
	}
}

// Fully unstubbed against the real registry copy: the canonical gascity pin is
// served offline from the synthetic cache gc materializes from its embedded
// gascity pack, and that content must hash to the registry release it claims
// to be. This is also the guard that the embedded gascity pack and its pin
// agree with the published release.
func TestAddImportRealRegistryCanonicalGascityReleaseVerifiesEmbeddedContent(t *testing.T) {
	useRegistryHome(t, "main", absFixture(t, realRegistryFixture))
	catalog, err := packregistry.ParseCatalog(mustRead(t, realRegistryFixture))
	if err != nil {
		t.Fatal(err)
	}
	pinned := strings.TrimPrefix(config.PublicGascityPackVersion, "sha:")
	version := ""
	for _, pack := range catalog.Packs {
		if pack.Name != "gascity" {
			continue
		}
		for _, release := range pack.Releases {
			if release.Commit == pinned {
				version = release.Version
			}
		}
	}
	if version == "" {
		t.Fatalf("config.PublicGascityPackVersion %s is not a release in %s; refresh the fixture from gascity-packs", pinned, realRegistryFixture)
	}

	city := newCityDir(t)
	res, err := AddImport(fsys.OSFS{}, city, config.PublicGascityPackSource, "gascity", version)
	if err != nil {
		t.Fatalf("AddImport gascity --version %s: %v", version, err)
	}
	if res.Version != config.PublicGascityPackVersion {
		t.Fatalf("Version = %q, want %q", res.Version, config.PublicGascityPackVersion)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
