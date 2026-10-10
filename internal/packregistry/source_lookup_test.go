package packregistry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackSourceIdentityTreatsSpellingsAlike(t *testing.T) {
	tree := "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
	for _, other := range []string{
		"https://github.com/gastownhall/gascity-packs.git//gascity",
		"https://github.com/gastownhall/gascity-packs//gascity",
		"https://github.com/gastownhall/gascity-packs/tree/release/gascity",
	} {
		if PackSourceIdentity(other) != PackSourceIdentity(tree) {
			t.Errorf("PackSourceIdentity(%q) = %q, want %q", other, PackSourceIdentity(other), PackSourceIdentity(tree))
		}
	}
	for _, different := range []string{
		"https://github.com/gastownhall/gascity-packs/tree/main/gastown",
		"https://github.com/gastownhall/gascity-packs/tree/main/gascity/roles",
		"https://github.com/other/gascity-packs/tree/main/gascity",
	} {
		if PackSourceIdentity(different) == PackSourceIdentity(tree) {
			t.Errorf("PackSourceIdentity(%q) collides with %q", different, tree)
		}
	}
}

const lookupCatalog = `schema = 1

[[pack]]
  name = "demo"
  description = "Demo pack."
  source = "https://github.com/example/packs/tree/main/demo"
  source_kind = "git"

  [[pack.release]]
    version = "0.1.0"
    ref = "main"
    commit = "1111111111111111111111111111111111111111"
    hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
    description = "First."
`

func TestLookupPacksBySourceRefreshesMissingCacheAndMatches(t *testing.T) {
	home := t.TempDir()
	catalogPath := filepath.Join(t.TempDir(), "registry.toml")
	if err := os.WriteFile(catalogPath, []byte(lookupCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, Config{Registries: []Registry{{Name: "local", Source: catalogPath}}}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	lookup, err := LookupPacksBySource(context.Background(), home, "https://github.com/example/packs.git//demo")
	if err != nil {
		t.Fatalf("LookupPacksBySource: %v", err)
	}
	if len(lookup.Matches) != 1 || lookup.Matches[0].Registry != "local" || lookup.Matches[0].Pack.Name != "demo" {
		t.Fatalf("Matches = %#v, want local:demo", lookup.Matches)
	}
	if len(lookup.Unavailable) != 0 {
		t.Fatalf("Unavailable = %v, want none", lookup.Unavailable)
	}
	if _, err := os.Stat(CachePath(home, "local")); err != nil {
		t.Fatalf("missing cache was not refreshed: %v", err)
	}

	miss, err := LookupPacksBySource(context.Background(), home, "https://github.com/example/other.git")
	if err != nil {
		t.Fatalf("LookupPacksBySource(miss): %v", err)
	}
	if len(miss.Matches) != 0 {
		t.Fatalf("Matches = %#v, want none for an unpublished source", miss.Matches)
	}
}

func TestLookupPacksBySourceRecordsUnavailableRegistry(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(t.TempDir(), "absent", "registry.toml")
	if err := SaveConfig(home, Config{Registries: []Registry{{Name: "gone", Source: missing}}}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	lookup, err := LookupPacksBySource(context.Background(), home, "https://github.com/example/packs/tree/main/demo")
	if err != nil {
		t.Fatalf("LookupPacksBySource: %v", err)
	}
	if len(lookup.Matches) != 0 {
		t.Fatalf("Matches = %#v, want none", lookup.Matches)
	}
	if len(lookup.Unavailable) != 1 || !strings.Contains(lookup.Unavailable[0].Error(), "registry gone") {
		t.Fatalf("Unavailable = %v, want the unreadable registry named", lookup.Unavailable)
	}
}

// A remote catalog may not publish a local source, so looking one up never
// touches a remote registry (and never fetches it).
func TestLookupPacksBySourceSkipsRemoteRegistriesForLocalSources(t *testing.T) {
	home := t.TempDir()
	if err := SaveConfig(home, Config{Registries: []Registry{{Name: "remote", Source: "https://registry.invalid/registry.toml"}}}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	lookup, err := LookupPacksBySource(context.Background(), home, "file:///tmp/packs.git//demo")
	if err != nil {
		t.Fatalf("LookupPacksBySource: %v", err)
	}
	if len(lookup.Matches) != 0 || len(lookup.Unavailable) != 0 {
		t.Fatalf("lookup = %+v, want the remote registry skipped entirely", lookup)
	}
}
