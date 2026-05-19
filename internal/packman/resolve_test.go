package packman

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packregistry"
)

func TestResolveVersionLatestMatchingConstraint(t *testing.T) {
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.3.1\nccc\trefs/tags/v2.0.0\n", nil
	}
	t.Cleanup(func() { runGit = prev })

	got, err := ResolveVersion("https://github.com/example/repo", "^1.2")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.3.1" || got.Commit != "bbb" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestResolveVersionSupportsComparators(t *testing.T) {
	prev := runGit
	runGit = func(_ string, _ ...string) (string, error) {
		return "aaa\trefs/tags/v1.2.0\nbbb\trefs/tags/v1.2.5\nccc\trefs/tags/v1.3.0\n", nil
	}
	t.Cleanup(func() { runGit = prev })

	got, err := ResolveVersion("https://github.com/example/repo", ">=1.2.0,<1.3.0")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "1.2.5" {
		t.Fatalf("Version = %q, want %q", got.Version, "1.2.5")
	}
}

func TestResolveVersionSupportsSHA(t *testing.T) {
	got, err := ResolveVersion("https://github.com/example/repo", "sha:deadbeef")
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if got.Version != "sha:deadbeef" || got.Commit != "deadbeef" {
		t.Fatalf("ResolveVersion = %#v", got)
	}
}

func TestDefaultConstraint(t *testing.T) {
	got, err := DefaultConstraint("1.4.2")
	if err != nil {
		t.Fatalf("DefaultConstraint: %v", err)
	}
	if got != "^1.4" {
		t.Fatalf("DefaultConstraint = %q, want %q", got, "^1.4")
	}
}

func TestResolveRegistryVersionSelectsHighestNonWithdrawn(t *testing.T) {
	home := registryResolverHome(t, registryCatalogFixture())

	got, err := ResolveVersionWithOptions("registry:main:lighthouse", "^1.0", ResolveOptions{GCHome: home})
	if err != nil {
		t.Fatalf("ResolveVersionWithOptions: %v", err)
	}
	if got.Version != "1.1.0" || got.Commit != commitB || got.Source != "https://packages.example/lighthouse.git" {
		t.Fatalf("resolved = %#v", got)
	}
	if got.Registry != "main" || got.RegistrySource != "file:///tmp/main/registry.toml" || got.Pack != "lighthouse" {
		t.Fatalf("registry metadata = %#v", got)
	}
	if got.Ref != "v1.1.0" || got.Hash != hashB || got.SourceKind != "git" {
		t.Fatalf("release metadata = %#v", got)
	}
}

func TestResolveRegistryVersionReusesExistingLockedRelease(t *testing.T) {
	home := registryResolverHome(t, registryCatalogFixture())

	got, err := ResolveVersionWithOptions("registry:main:lighthouse", "^1.0", ResolveOptions{
		GCHome: home,
		Existing: &LockedPack{
			Version: "1.0.0",
			Commit:  commitA,
			Ref:     "v1.0.0",
			Hash:    hashA,
		},
	})
	if err != nil {
		t.Fatalf("ResolveVersionWithOptions: %v", err)
	}
	if got.Version != "1.0.0" || got.Commit != commitA {
		t.Fatalf("resolved = %#v, want existing 1.0.0", got)
	}
}

func TestResolveRegistryVersionSkipsWithdrawnUnlessSHAPinned(t *testing.T) {
	home := registryResolverHome(t, registryCatalogFixture())

	got, err := ResolveVersionWithOptions("registry:main:lighthouse", "", ResolveOptions{GCHome: home})
	if err != nil {
		t.Fatalf("ResolveVersionWithOptions latest: %v", err)
	}
	if got.Version != "1.1.0" {
		t.Fatalf("latest version = %q, want non-withdrawn 1.1.0", got.Version)
	}

	got, err = ResolveVersionWithOptions("registry:main:lighthouse", "sha:"+commitWithdrawn, ResolveOptions{GCHome: home})
	if err != nil {
		t.Fatalf("ResolveVersionWithOptions sha: %v", err)
	}
	if got.Version != "2.0.0" || !got.Withdrawn || got.WithdrawnReason != "bad release" {
		t.Fatalf("sha-pinned withdrawn release = %#v", got)
	}
}

func TestResolveRegistryVersionRejectsNonStrictSHAPins(t *testing.T) {
	home := registryResolverHome(t, registryCatalogFixture())
	for _, constraint := range []string{
		"sha:abc123",
		"sha:ABCDEF0123456789ABCDEF0123456789ABCDEF01",
	} {
		if _, err := ResolveVersionWithOptions("registry:main:lighthouse", constraint, ResolveOptions{GCHome: home}); err == nil {
			t.Fatalf("ResolveVersionWithOptions(%q) succeeded, want error", constraint)
		}
	}
}

func TestResolveRegistryVersionRequiresCachedCatalog(t *testing.T) {
	home := t.TempDir()
	if err := packregistry.SaveConfig(home, packregistry.Config{Registries: []packregistry.Registry{{
		Name:   "main",
		Source: "file:///tmp/main/registry.toml",
	}}}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	_, err := ResolveVersionWithOptions("registry:main:lighthouse", "", ResolveOptions{GCHome: home})
	if err == nil || !strings.Contains(err.Error(), "gc pack registry refresh main") {
		t.Fatalf("error = %v, want refresh hint", err)
	}
}

func TestWriteLockfileUsesSchema2OnlyWhenRegistryMetadataPresent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLockfile(fsys.OSFS{}, dir, &Lockfile{Packs: map[string]LockedPack{
		"https://example.com/repo.git": {Version: "1.0.0", Commit: commitA, Fetched: time.Unix(1, 0).UTC()},
	}}); err != nil {
		t.Fatalf("WriteLockfile legacy: %v", err)
	}
	lock, err := ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile legacy: %v", err)
	}
	if lock.Schema != LockfileSchema {
		t.Fatalf("legacy schema = %d, want %d", lock.Schema, LockfileSchema)
	}

	dir = t.TempDir()
	if err := WriteLockfile(fsys.OSFS{}, dir, &Lockfile{Packs: map[string]LockedPack{
		"https://packages.example/lighthouse.git": {
			Version:      "1.0.0",
			Commit:       commitA,
			Fetched:      time.Unix(1, 0).UTC(),
			Source:       "https://packages.example/lighthouse.git",
			SourceKind:   "git",
			Ref:          "v1.0.0",
			Hash:         hashA,
			Registry:     "main",
			RegistryPack: "lighthouse",
		},
	}}); err != nil {
		t.Fatalf("WriteLockfile registry: %v", err)
	}
	lock, err = ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile registry: %v", err)
	}
	if lock.Schema != LockfileSchemaV2 {
		t.Fatalf("registry schema = %d, want %d", lock.Schema, LockfileSchemaV2)
	}
}

const (
	commitA         = "0123456789abcdef0123456789abcdef01234567"
	commitB         = "89abcdef0123456789abcdef0123456789abcdef"
	commitWithdrawn = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashA           = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
	hashB           = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	hashWithdrawn   = "sha256:18f5384d58bcb1bba0bcd9e6a6781d1a6ac2cc280a989a08999f22eb006da8b6"
)

func registryResolverHome(t *testing.T, catalog string) string {
	t.Helper()
	home := t.TempDir()
	if err := packregistry.SaveConfig(home, packregistry.Config{Registries: []packregistry.Registry{{
		Name:   "main",
		Source: "file:///tmp/main/registry.toml",
	}}}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := packregistry.WriteCatalogCache(home, "main", []byte(catalog)); err != nil {
		t.Fatalf("WriteCatalogCache: %v", err)
	}
	return home
}

func registryCatalogFixture() string {
	return `schema = 1

[[pack]]
name = "lighthouse"
description = "Harbor checks."
source = "https://packages.example/lighthouse.git"
source_kind = "git"

  [[pack.release]]
  version = "1.0.0"
  ref = "v1.0.0"
  commit = "` + commitA + `"
  hash = "` + hashA + `"
  description = "Initial release."

  [[pack.release]]
  version = "1.1.0"
  ref = "v1.1.0"
  commit = "` + commitB + `"
  hash = "` + hashB + `"
  description = "Second release."

  [[pack.release]]
  version = "2.0.0"
  ref = "v2.0.0"
  commit = "` + commitWithdrawn + `"
  hash = "` + hashWithdrawn + `"
  description = "Withdrawn release."
  withdrawn = true
  withdrawn_reason = "bad release"
`
}
