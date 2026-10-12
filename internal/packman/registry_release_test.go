package packman

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packregistry"
	"github.com/gastownhall/gascity/internal/testutil"
)

const realRegistryFixture = "testdata/gascity-packs-registry.toml"

// registryRepo is a local git repository shaped like gascity-packs: one pack
// at packs/demo with two release commits, plus a later repository-level
// v0.4.0 tag that belongs to no pack release. Git-tag resolution of ">=0.1.0"
// would pick that tag; registry resolution must not.
type registryRepo struct {
	dir                      string
	source                   string
	commit1, commit2, tagged string
	hash1, hash2             string
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return testutil.RunGit(t, dir, append([]string{"-c", "user.email=test@example.com", "-c", "user.name=Test", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
}

func writeFixtureFile(t *testing.T, root, rel, body string, mode os.FileMode) {
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

func newRegistryRepo(t *testing.T) registryRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "packs-repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "init", "-q", "-b", "main")
	writeFixtureFile(t, dir, "packs/demo/pack.toml", "[pack]\nname = \"demo\"\nschema = 2\n", 0o644)
	writeFixtureFile(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho one\n", 0o755)
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-q", "-m", "demo 0.1.0")
	commit1 := fixtureGit(t, dir, "rev-parse", "HEAD")
	writeFixtureFile(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho two\n", 0o755)
	fixtureGit(t, dir, "commit", "-q", "-am", "demo 0.2.0")
	commit2 := fixtureGit(t, dir, "rev-parse", "HEAD")
	writeFixtureFile(t, dir, "packs/demo/scripts/run.sh", "#!/bin/sh\necho unreleased\n", 0o755)
	fixtureGit(t, dir, "commit", "-q", "-am", "repository release, no pack release")
	fixtureGit(t, dir, "tag", "v0.4.0")
	tagged := fixtureGit(t, dir, "rev-parse", "HEAD")

	hash := func(commit string) string {
		h, err := packregistry.PackContentHash(dir, commit, "packs/demo")
		if err != nil {
			t.Fatalf("PackContentHash: %v", err)
		}
		return h
	}
	return registryRepo{
		dir:     dir,
		source:  "file://" + dir + "//packs/demo",
		commit1: commit1, commit2: commit2, tagged: tagged,
		hash1: hash(commit1), hash2: hash(commit2),
	}
}

type fixtureRelease struct {
	version, commit, hash string
}

// writeRegistry writes a one-pack catalog for repo and points a fresh Gas City
// home's only registry ("fixture") at it. It returns the catalog path so a test
// can publish more releases.
func writeRegistry(t *testing.T, repo registryRepo, releases ...fixtureRelease) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	catalog := filepath.Join(t.TempDir(), "registry.toml")
	writeCatalogReleases(t, catalog, repo, releases...)
	if err := packregistry.SaveConfig(home, packregistry.Config{
		Registries: []packregistry.Registry{{Name: "fixture", Source: catalog}},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return catalog
}

func writeCatalogReleases(t *testing.T, catalog string, repo registryRepo, releases ...fixtureRelease) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "schema = 1\n\n[[pack]]\n  name = \"demo\"\n  description = \"Demo pack.\"\n  source = %q\n  source_kind = \"git\"\n", repo.source)
	for _, r := range releases {
		fmt.Fprintf(&b, "\n  [[pack.release]]\n    version = %q\n    ref = \"main\"\n    commit = %q\n    hash = %q\n    description = \"Release.\"\n", r.version, r.commit, r.hash)
	}
	if err := os.WriteFile(catalog, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func demoImports(repo registryRepo, constraint string) map[string]config.Import {
	return map[string]config.Import{"demo": {Source: repo.source, Version: constraint}}
}

func TestSyncLockResolvesRegistryPackConstraintToReleaseNotTag(t *testing.T) {
	repo := newRegistryRepo(t)
	writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1}, fixtureRelease{"0.2.0", repo.commit2, repo.hash2})

	for constraint, want := range map[string]LockedPack{
		">=0.1.0": {Version: "0.2.0", Commit: repo.commit2},
		"^0.1":    {Version: "0.1.0", Commit: repo.commit1},
		"0.1.0":   {Version: "0.1.0", Commit: repo.commit1},
		"":        {Version: "0.2.0", Commit: repo.commit2},
	} {
		lock, err := SyncLock(t.TempDir(), demoImports(repo, constraint), InstallResolveIfNeeded)
		if err != nil {
			t.Fatalf("SyncLock(%q): %v", constraint, err)
		}
		got := lock.Packs[repo.source]
		if got.Version != want.Version || got.Commit != want.Commit {
			t.Errorf("SyncLock(%q) locked %s@%s, want registry release %s@%s (the v0.4.0 tag is %s)", constraint, got.Version, got.Commit, want.Version, want.Commit, repo.tagged)
		}
	}
}

// A lock entry an older gc resolved from the repository tag satisfies the
// constraint textually but is no registry release; install re-resolves it.
func TestSyncLockReResolvesLegacyTagLockForRegistryPack(t *testing.T) {
	repo := newRegistryRepo(t)
	writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1}, fixtureRelease{"0.2.0", repo.commit2, repo.hash2})
	if _, err := packregistry.LookupPacksBySource(t.Context(), os.Getenv("GC_HOME"), repo.source); err != nil {
		t.Fatalf("seeding the registry cache: %v", err)
	}
	city := t.TempDir()
	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{Schema: LockfileSchema, Packs: map[string]LockedPack{
		repo.source: {Version: "0.4.0", Commit: repo.tagged},
	}}); err != nil {
		t.Fatal(err)
	}
	lock, err := SyncLock(city, demoImports(repo, ">=0.1.0"), InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if got := lock.Packs[repo.source]; got.Version != "0.2.0" || got.Commit != repo.commit2 {
		t.Fatalf("locked %s@%s, want the legacy tag lock replaced by release 0.2.0@%s", got.Version, got.Commit, repo.commit2)
	}

	// A lock entry that is a registry release is kept as is.
	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{Schema: LockfileSchema, Packs: map[string]LockedPack{
		repo.source: {Version: "0.1.0", Commit: repo.commit1},
	}}); err != nil {
		t.Fatal(err)
	}
	lock, err = SyncLock(city, demoImports(repo, ">=0.1.0"), InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if got := lock.Packs[repo.source]; got.Commit != repo.commit1 {
		t.Fatalf("locked %s@%s, want the existing release lock 0.1.0 kept", got.Version, got.Commit)
	}
}

func TestSyncLockUpgradeMovesRegistryPackToNewestMatchingRelease(t *testing.T) {
	repo := newRegistryRepo(t)
	catalog := writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1})
	city := t.TempDir()
	lock, err := SyncLock(city, demoImports(repo, ">=0.1.0"), InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if got := lock.Packs[repo.source]; got.Commit != repo.commit1 {
		t.Fatalf("initial lock %s@%s, want 0.1.0@%s", got.Version, got.Commit, repo.commit1)
	}
	if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
		t.Fatal(err)
	}

	// The registry publishes 0.2.0 after the cache was written; install keeps
	// the lock, upgrade refreshes the registry and moves to it.
	writeCatalogReleases(t, catalog, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1}, fixtureRelease{"0.2.0", repo.commit2, repo.hash2})
	lock, err = SyncLock(city, demoImports(repo, ">=0.1.0"), InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock (install): %v", err)
	}
	if got := lock.Packs[repo.source]; got.Commit != repo.commit1 {
		t.Fatalf("install moved the lock to %s@%s; it must keep 0.1.0", got.Version, got.Commit)
	}
	lock, err = SyncLock(city, demoImports(repo, ">=0.1.0"), InstallUpgrade)
	if err != nil {
		t.Fatalf("SyncLock (upgrade): %v", err)
	}
	if got := lock.Packs[repo.source]; got.Version != "0.2.0" || got.Commit != repo.commit2 {
		t.Fatalf("upgrade locked %s@%s, want release 0.2.0@%s (not the v0.4.0 tag)", got.Version, got.Commit, repo.commit2)
	}
}

func TestSyncLockRejectsRegistryReleaseHashMismatch(t *testing.T) {
	repo := newRegistryRepo(t)
	wrong := "sha256:" + strings.Repeat("0", 64)

	t.Run("install", func(t *testing.T) {
		writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, wrong})
		_, err := SyncLock(t.TempDir(), demoImports(repo, "0.1.0"), InstallResolveIfNeeded)
		if !errors.Is(err, ErrRegistryRelease) || !strings.Contains(err.Error(), "hash mismatch") {
			t.Fatalf("install err = %v, want ErrRegistryRelease hash mismatch", err)
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		catalog := writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1})
		city := t.TempDir()
		lock, err := SyncLock(city, demoImports(repo, ">=0.1.0"), InstallResolveIfNeeded)
		if err != nil {
			t.Fatalf("SyncLock: %v", err)
		}
		if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
			t.Fatal(err)
		}
		// The registry then publishes 0.2.0 with a hash its content does not have.
		writeCatalogReleases(t, catalog, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1}, fixtureRelease{"0.2.0", repo.commit2, wrong})
		_, err = SyncLock(city, demoImports(repo, ">=0.1.0"), InstallUpgrade)
		if !errors.Is(err, ErrRegistryRelease) || !strings.Contains(err.Error(), "hash mismatch") {
			t.Fatalf("upgrade err = %v, want ErrRegistryRelease hash mismatch", err)
		}
	})
}

func TestSyncLockRegistryPackWithoutMatchingReleaseNeverFallsBackToTags(t *testing.T) {
	repo := newRegistryRepo(t)
	writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1}, fixtureRelease{"0.2.0", repo.commit2, repo.hash2})
	// v0.4.0 exists as a repository tag, but it is no release of the pack.
	_, err := SyncLock(t.TempDir(), demoImports(repo, "0.4.0"), InstallResolveIfNeeded)
	if !errors.Is(err, ErrRegistryRelease) || !strings.Contains(err.Error(), `no release matching "0.4.0" (available: 0.1.0, 0.2.0)`) {
		t.Fatalf("err = %v, want a registry no-match listing the releases", err)
	}
}

// A source no registry publishes keeps git-tag resolution.
func TestSyncLockNonRegistrySourceStillResolvesGitTags(t *testing.T) {
	repo := newRegistryRepo(t)
	writeRegistry(t, repo, fixtureRelease{"0.1.0", repo.commit1, repo.hash1})
	other := filepath.Join(t.TempDir(), "tools")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, other, "init", "-q", "-b", "main")
	writeFixtureFile(t, other, "pack.toml", "[pack]\nname = \"tools\"\nschema = 2\n", 0o644)
	fixtureGit(t, other, "add", ".")
	fixtureGit(t, other, "commit", "-q", "-m", "tools")
	fixtureGit(t, other, "tag", "v1.2.0")
	commit := fixtureGit(t, other, "rev-parse", "HEAD")

	source := "file://" + other
	lock, err := SyncLock(t.TempDir(), map[string]config.Import{"tools": {Source: source, Version: "^1.2"}}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if got := lock.Packs[source]; got.Version != "1.2.0" || got.Commit != commit {
		t.Fatalf("locked %+v, want tag v1.2.0 at %s", got, commit)
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
	if _, err := SelectRegistryRelease(matches, "0.1.0"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("SelectRegistryRelease err = %v, want an ambiguity refusal", err)
	}
	matches[1].Pack.Releases = []packregistry.CatalogRelease{release(strings.Repeat("1", 40))}
	got, err := SelectRegistryRelease(matches, "0.1.0")
	if err != nil || got.Commit != strings.Repeat("1", 40) {
		t.Fatalf("SelectRegistryRelease = %+v, %v; want the shared commit", got, err)
	}
}

// The releases the reported failure named, selected from a verbatim copy of
// the real gascity-packs registry.
func TestSelectRegistryReleaseAgainstRealRegistry(t *testing.T) {
	data, err := os.ReadFile(realRegistryFixture)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := packregistry.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		pack, constraint, version, commit string
	}{
		{"gascity", "0.1.6", "0.1.6", "3b3b89f2011e06d84459aa7bea1552382f13930a"},
		{"gascity", ">=0.1.6", "0.1.6", "3b3b89f2011e06d84459aa7bea1552382f13930a"},
		{"gascity", "0.1.4", "0.1.4", "99464ed9240b1f6e6b7ab1d351f67016e1a973ff"},
		{"gascity", "", "0.1.6", "3b3b89f2011e06d84459aa7bea1552382f13930a"},
		{"gastown", "^0.1", "0.1.10", "33d3a430a67d1782ad364556cb566bdb01d0afe3"},
		{"gstack", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"compound-engineering", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"bmad", ">=0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"superpowers", "0.1.1", "0.1.1", "390cac1a8a0478b6dfde603dae00a43ed9748d13"},
		{"gstack", "0.1.0", "0.1.0", "636fdfaaf54195b293e8e56d5e9c30e4b8d5a9ef"},
	}
	for _, tc := range cases {
		var pack packregistry.CatalogPack
		for _, p := range catalog.Packs {
			if p.Name == tc.pack {
				pack = p
			}
		}
		got, err := SelectRegistryRelease([]packregistry.PackMatch{{Registry: "main", Pack: pack}}, tc.constraint)
		if err != nil {
			t.Errorf("%s %q: %v", tc.pack, tc.constraint, err)
			continue
		}
		if got.Version != tc.version || got.Commit != tc.commit {
			t.Errorf("%s %q = %s@%s, want %s@%s", tc.pack, tc.constraint, got.Version, got.Commit, tc.version, tc.commit)
		}
	}
}

// Fully unstubbed against the real registry copy: the canonical gascity pin is
// served offline from the synthetic cache gc materializes from its embedded
// gascity pack, and that content must hash to the registry release it claims
// to be. This also guards that the embedded gascity pack and its pin agree
// with the published release.
func TestSyncLockRealRegistryCanonicalGascityReleaseVerifiesEmbeddedContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	abs, err := filepath.Abs(realRegistryFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := packregistry.SaveConfig(home, packregistry.Config{Registries: []packregistry.Registry{{Name: "main", Source: abs}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(realRegistryFixture)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := packregistry.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	pinned := strings.TrimPrefix(config.PublicGascityPackVersion, "sha:")
	version := ""
	for _, pack := range catalog.Packs {
		for _, release := range pack.Releases {
			if pack.Name == "gascity" && release.Commit == pinned {
				version = release.Version
			}
		}
	}
	if version == "" {
		t.Fatalf("config.PublicGascityPackVersion %s is not a gascity release in %s; refresh the fixture from gascity-packs", pinned, realRegistryFixture)
	}

	lock, err := SyncLock(t.TempDir(), map[string]config.Import{
		"gascity": {Source: config.PublicGascityPackSource, Version: version},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock gascity %s: %v", version, err)
	}
	if got := lock.Packs[config.PublicGascityPackSource]; got.Version != version || got.Commit != pinned {
		t.Fatalf("locked %s@%s, want %s@%s", got.Version, got.Commit, version, pinned)
	}
}
