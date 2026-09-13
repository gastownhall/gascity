package config

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A city that imports a pack whose pack.toml declares [upstreams.gasworks]
// inherits that upstream preset — "import the pack, get the upstream for free".
func TestLoadWithIncludes_PackUpstreamInheritedByCity(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "c"

[imports.local]
source = "packs/local"
`)
	fs.Files["/city/packs/local/pack.toml"] = []byte(`
[pack]
name = "local"
schema = 2

[upstreams.gasworks]
description = "Gasworks managed gateway"
base_url = "https://gasworks.example/anthropic"
api_key  = "$GASWORKS_API_KEY"

[upstreams.gasworks.env]
GASWORKS_TRACE = "$GASWORKS_TRACE"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	u, ok := cfg.Upstreams["gasworks"]
	if !ok {
		t.Fatalf("pack upstream missing from composed config: %v", cfg.Upstreams)
	}
	if u.BaseURL != "https://gasworks.example/anthropic" {
		t.Errorf("base_url = %q, want https://gasworks.example/anthropic", u.BaseURL)
	}
	if u.APIKey != "$GASWORKS_API_KEY" {
		t.Errorf("api_key = %q, want $GASWORKS_API_KEY (env-ref)", u.APIKey)
	}
	if got := u.Env["GASWORKS_TRACE"]; got != "$GASWORKS_TRACE" {
		t.Errorf("env GASWORKS_TRACE = %q, want $GASWORKS_TRACE", got)
	}
}

// Precedence: when the city ALSO declares [upstreams.gasworks], the CITY's
// definition wins — the pack is the base layer and is NOT overwritten over the
// existing city entry (mirrors the providers "additive, no overwrite" rule).
func TestLoadWithIncludes_CityUpstreamOverridesPack(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "c"

[imports.local]
source = "packs/local"

[upstreams.gasworks]
base_url = "https://city.example/anthropic"
api_key  = "$CITY_API_KEY"
`)
	fs.Files["/city/packs/local/pack.toml"] = []byte(`
[pack]
name = "local"
schema = 2

[upstreams.gasworks]
base_url = "https://pack.example/anthropic"
api_key  = "$PACK_API_KEY"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	u, ok := cfg.Upstreams["gasworks"]
	if !ok {
		t.Fatalf("upstream missing from composed config: %v", cfg.Upstreams)
	}
	if u.BaseURL != "https://city.example/anthropic" {
		t.Errorf("base_url = %q, want city definition to win (https://city.example/anthropic)", u.BaseURL)
	}
	if u.APIKey != "$CITY_API_KEY" {
		t.Errorf("api_key = %q, want $CITY_API_KEY (city wins)", u.APIKey)
	}
}

// A root pack.toml (the city's own pack) that declares [upstreams.*] merges as
// a base, and an [upstreams.<name>] in a pack.toml no longer produces a fatal
// undecoded-key error: it loads cleanly.
func TestLoadWithIncludes_RootPackUpstreamLoadsCleanly(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "c"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "test"
schema = 2

[upstreams.gasworks]
base_url = "https://gasworks.example/anthropic"
api_key  = "$GASWORKS_API_KEY"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	u, ok := cfg.Upstreams["gasworks"]
	if !ok {
		t.Fatalf("root pack upstream missing from composed config: %v", cfg.Upstreams)
	}
	if u.BaseURL != "https://gasworks.example/anthropic" {
		t.Errorf("base_url = %q, want https://gasworks.example/anthropic", u.BaseURL)
	}
}

// Nested pack-imports-a-pack: pack B includes pack A. A declares
// [upstreams.fromA]; B declares [upstreams.fromB] and a colliding
// [upstreams.shared]. A city imports B. The composed city inherits fromA AND
// fromB (the recursive merge), and for the collision B (the importing/parent
// pack) wins over A (the included pack) — mirrors the provider merge where
// tc.Upstreams overwrites includedUpstreams. This winner is deterministic:
// includes are processed in declaration order and the parent pack's own
// upstreams overwrite inherited ones.
func TestLoadWithIncludes_NestedPackUpstreamsMerged(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "c"

[imports.b]
source = "packs/b"
`)
	fs.Files["/city/packs/b/pack.toml"] = []byte(`
[pack]
name = "b"
schema = 2
includes = ["../a"]

[upstreams.fromB]
base_url = "https://b.example/anthropic"

[upstreams.shared]
base_url = "https://shared-from-b.example/anthropic"
`)
	fs.Files["/city/packs/a/pack.toml"] = []byte(`
[pack]
name = "a"
schema = 2

[upstreams.fromA]
base_url = "https://a.example/anthropic"

[upstreams.shared]
base_url = "https://shared-from-a.example/anthropic"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Upstreams["fromA"].BaseURL; got != "https://a.example/anthropic" {
		t.Errorf("fromA base_url = %q, want https://a.example/anthropic (inherited from included pack A)", got)
	}
	if got := cfg.Upstreams["fromB"].BaseURL; got != "https://b.example/anthropic" {
		t.Errorf("fromB base_url = %q, want https://b.example/anthropic (from importing pack B)", got)
	}
	// Collision: the importing pack (B) wins over the included pack (A).
	if got := cfg.Upstreams["shared"].BaseURL; got != "https://shared-from-b.example/anthropic" {
		t.Errorf("shared base_url = %q, want https://shared-from-b.example/anthropic (importing pack B wins over included pack A)", got)
	}
}

// Two packs declaring the same upstream → deterministic first-wins. A city
// imports two packs that both declare [upstreams.gasworks] with distinct
// base_urls. Import binding names are processed in sorted order with "no
// overwrite", so the lexically-first binding ("one") wins — mirrors the
// provider determinism in TestExpandCityPacks_ProvidersMerged.
func TestLoadWithIncludes_TwoPacksSameUpstreamFirstWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "c"

[imports.one]
source = "packs/one"

[imports.two]
source = "packs/two"
`)
	fs.Files["/city/packs/one/pack.toml"] = []byte(`
[pack]
name = "one"
schema = 2

[upstreams.gasworks]
base_url = "https://one.example/anthropic"
`)
	fs.Files["/city/packs/two/pack.toml"] = []byte(`
[pack]
name = "two"
schema = 2

[upstreams.gasworks]
base_url = "https://two.example/anthropic"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	// Binding "one" sorts before "two" → first wins, no overwrite.
	if got := cfg.Upstreams["gasworks"].BaseURL; got != "https://one.example/anthropic" {
		t.Errorf("gasworks base_url = %q, want https://one.example/anthropic (first import 'one' wins)", got)
	}
}

// Clone/cache isolation: the cached pack-load result must hand out detached
// copies so a caller mutating the returned upstreams map (or a nested Env
// value) cannot corrupt the cache for the next load. Proves
// deepCopyUpstreamSpecs isolates both the map and each spec's Env.
func TestLoadPackWithCache_DetachesUpstreams(t *testing.T) {
	dir := t.TempDir()
	cityRoot := filepath.Join(dir, "city")
	packDir := filepath.Join(dir, "shared")
	for _, d := range []string{cityRoot, packDir} {
		mustMkdirAll(t, d, 0o755)
	}

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "shared"
schema = 1

[upstreams.helper]
base_url = "https://helper.example/anthropic"
api_key  = "$HELPER_API_KEY"

[upstreams.helper.env]
HELPER_TRACE = "base"
`)

	cache := &packLoadCache{results: map[string]*packLoadResult{}}
	topoPath := filepath.Join(packDir, "pack.toml")

	_, _, _, upstreams, _, _, _, _, err := loadPackWithCache(
		fsys.OSFS{}, topoPath, packDir, cityRoot, "", nil, cache)
	if err != nil {
		t.Fatalf("loadPackWithCache first pass: %v", err)
	}
	if len(upstreams) != 1 {
		t.Fatalf("unexpected first-pass upstreams: %v", upstreams)
	}

	// Mutate the returned map AND a nested Env value, then drop the entry.
	upstreams["helper"].Env["HELPER_TRACE"] = "mutated"
	delete(upstreams, "helper")

	_, _, _, upstreams2, _, _, _, _, err := loadPackWithCache(
		fsys.OSFS{}, topoPath, packDir, cityRoot, "", nil, cache)
	if err != nil {
		t.Fatalf("loadPackWithCache second pass: %v", err)
	}
	helper2, ok := upstreams2["helper"]
	if !ok {
		t.Fatalf("cached upstream entry leaked deletion: %v", upstreams2)
	}
	if helper2.BaseURL != "https://helper.example/anthropic" {
		t.Errorf("cached upstream base_url leaked mutation: got %q, want %q", helper2.BaseURL, "https://helper.example/anthropic")
	}
	if got := helper2.Env["HELPER_TRACE"]; got != "base" {
		t.Errorf("cached upstream env leaked mutation: got %q, want %q", got, "base")
	}
}

// Rig-scope pack expansion merges pack upstreams into the city (additive,
// first-wins, no overwrite of an existing city entry) — the rig path threads
// upstreams through a separate merge-into-cfg block from the city path, so it
// gets its own coverage mirroring TestExpandPacks_ProvidersMerged.
func TestExpandPacks_UpstreamsMerged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[upstreams.gasworks]
base_url = "https://pack.example/anthropic"

[[agent]]
name = "witness"
`)

	cfg := &City{
		Upstreams: map[string]UpstreamSpec{
			"existing": {BaseURL: "https://city.example/anthropic"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// gasworks upstream should be added from the pack.
	if got := cfg.Upstreams["gasworks"].BaseURL; got != "https://pack.example/anthropic" {
		t.Errorf("gasworks base_url = %q, want https://pack.example/anthropic (merged from pack)", got)
	}
	// existing city upstream should still exist, untouched.
	if got := cfg.Upstreams["existing"].BaseURL; got != "https://city.example/anthropic" {
		t.Errorf("existing base_url = %q, want https://city.example/anthropic (city entry preserved)", got)
	}
}

// LoadPackForLint surfaces pack-declared [upstreams] the same way it surfaces
// [providers], so lint-style tooling can inspect them instead of the loader
// silently discarding the loaded map.
func TestLoadPackForLint_SurfacesUpstreams(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/packs/local/pack.toml"] = []byte(`
[pack]
name = "local"
schema = 2

[upstreams.gasworks]
description = "Gasworks managed gateway"
base_url = "https://gasworks.example/anthropic"
api_key  = "$GASWORKS_API_KEY"

[upstreams.gasworks.env]
GASWORKS_TRACE = "$GASWORKS_TRACE"
`)

	loaded, err := LoadPackForLint(fs, "/packs/local")
	if err != nil {
		t.Fatalf("LoadPackForLint: %v", err)
	}
	u, ok := loaded.Upstreams["gasworks"]
	if !ok {
		t.Fatalf("lint load missing pack upstream: %v", loaded.Upstreams)
	}
	if u.BaseURL != "https://gasworks.example/anthropic" {
		t.Errorf("base_url = %q, want https://gasworks.example/anthropic", u.BaseURL)
	}
	if u.APIKey != "$GASWORKS_API_KEY" {
		t.Errorf("api_key = %q, want $GASWORKS_API_KEY (env-ref)", u.APIKey)
	}
	if got := u.Env["GASWORKS_TRACE"]; got != "$GASWORKS_TRACE" {
		t.Errorf("env GASWORKS_TRACE = %q, want $GASWORKS_TRACE", got)
	}
}
