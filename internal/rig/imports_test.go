package rig

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestCanonicalizePackIncludesClassifiesNamesAndPaths pins how an --include
// token is classified as a pack NAME versus a local PATH, and the precedence
// between the two. Before registry packs gained scoped owner/pack names, a
// slash in the token was treated as proof of a path and short-circuited every
// lookup, so `--include alice/foo` silently became the local import
// ./alice/foo (registry-sfn). The grammar decides now, and the resolver is
// only consulted for tokens that could be names.
func TestCanonicalizePackIncludesClassifiesNamesAndPaths(t *testing.T) {
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}

	const scopedSource = "https://packages.example/cacc-twin-team.git"
	const flatSource = "https://packages.example/lighthouse.git"
	catalog := map[string]string{
		"wespd/cacc-twin-team": scopedSource,
		"lighthouse":           flatSource,
		"packs/planner":        "https://packages.example/squatted-planner.git",
		"core":                 "https://packages.example/squatted-core.git",
	}

	cases := []struct {
		name string
		// dirs are created under the city before canonicalization; a path
		// ending in pack.toml is written as that file.
		dirs     []string
		packs    map[string]config.PackSource
		includes []string
		want     []string
		// wantLookups are the tokens the registry resolver must be asked
		// about, in order. Path-shaped tokens must never reach it.
		wantLookups []string
	}{{
		name:        "scoped registry name resolves to its catalog source",
		includes:    []string{"wespd/cacc-twin-team"},
		want:        []string{scopedSource},
		wantLookups: []string{"wespd/cacc-twin-team"},
	}, {
		name:        "flat registry name resolves to its catalog source",
		includes:    []string{"lighthouse"},
		want:        []string{flatSource},
		wantLookups: []string{"lighthouse"},
	}, {
		name:        "bare builtin name still canonicalizes to the bundled source",
		includes:    []string{"core"},
		want:        []string{coreSource},
		wantLookups: nil,
	}, {
		name:        "packs/<builtin> still canonicalizes to the bundled source",
		includes:    []string{"packs/core"},
		want:        []string{coreSource},
		wantLookups: nil,
	}, {
		name:        "local pack directory keeps its local import",
		dirs:        []string{"packs/planner/pack.toml"},
		includes:    []string{"packs/planner"},
		want:        []string{"packs/planner"},
		wantLookups: nil,
	}, {
		name:        "packs/<name> is never a scoped registry name",
		includes:    []string{"packs/planner"},
		want:        []string{"packs/planner"},
		wantLookups: nil,
	}, {
		name:        "local directory beats a scoped registry name",
		dirs:        []string{"wespd/cacc-twin-team/pack.toml"},
		includes:    []string{"wespd/cacc-twin-team"},
		want:        []string{"wespd/cacc-twin-team"},
		wantLookups: nil,
	}, {
		name:        "local directory without pack.toml still beats the registry",
		dirs:        []string{"wespd/cacc-twin-team"},
		includes:    []string{"wespd/cacc-twin-team"},
		want:        []string{"wespd/cacc-twin-team"},
		wantLookups: nil,
	}, {
		name:        "builtin beats a registry pack of the same name",
		includes:    []string{"core"},
		want:        []string{coreSource},
		wantLookups: nil,
	}, {
		name:        "configured [packs] entry beats the registry",
		packs:       map[string]config.PackSource{"wespd/cacc-twin-team": {Source: "https://example.test/configured.git"}},
		includes:    []string{"wespd/cacc-twin-team"},
		want:        []string{"wespd/cacc-twin-team"},
		wantLookups: nil,
	}, {
		name:        "./<name> declares a path and is never a registry name",
		includes:    []string{"./lighthouse"},
		want:        []string{"./lighthouse"},
		wantLookups: nil,
	}, {
		name:        "./<builtin> still canonicalizes to the bundled source",
		includes:    []string{"./core"},
		want:        []string{coreSource},
		wantLookups: nil,
	}, {
		name:        "path-shaped tokens are never looked up",
		includes:    []string{"./local/pack", "../sibling/pack", "/abs/pack", "deep/nested/pack", "Upper/Case", "under_score/pack", "github.com/org/repo", "https://example.test/x.git", "git@example.test:org/x.git", "packs/"},
		want:        []string{"./local/pack", "../sibling/pack", "/abs/pack", "deep/nested/pack", "Upper/Case", "under_score/pack", "github.com/org/repo", "https://example.test/x.git", "git@example.test:org/x.git", "packs/"},
		wantLookups: nil,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			for _, entry := range tc.dirs {
				full := filepath.Join(cityPath, filepath.FromSlash(entry))
				if filepath.Base(entry) == "pack.toml" {
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(full, []byte("[pack]\nschema = 2\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(full, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var lookups []string
			resolve := func(name string) (string, bool) {
				lookups = append(lookups, name)
				source, ok := catalog[name]
				return source, ok
			}
			got := canonicalizePackIncludes(fsys.OSFS{}, cityPath, tc.includes, tc.packs, resolve)
			if !slices.Equal(got, tc.want) {
				t.Errorf("canonicalizePackIncludes = %q, want %q", got, tc.want)
			}
			if !slices.Equal(lookups, tc.wantLookups) {
				t.Errorf("registry lookups = %q, want %q", lookups, tc.wantLookups)
			}
		})
	}
}

// TestCanonicalizePackIncludesWithoutResolver proves the registry step is
// nil-safe: a caller that injects no resolver (rig.Deps.ResolveRegistryPack
// nil) keeps the pre-registry behavior, leaving a scoped token verbatim for
// path handling instead of panicking.
func TestCanonicalizePackIncludesWithoutResolver(t *testing.T) {
	cityPath := t.TempDir()
	got := canonicalizePackIncludes(fsys.OSFS{}, cityPath, []string{"wespd/cacc-twin-team"}, nil, nil)
	if want := []string{"wespd/cacc-twin-team"}; !slices.Equal(got, want) {
		t.Errorf("canonicalizePackIncludes = %q, want %q", got, want)
	}
}
