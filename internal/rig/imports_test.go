package rig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// writePackDir materializes a minimal pack directory (a dir holding
// pack.toml) at dir, which is what the pack loader requires of any local
// import source.
func writePackDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"local\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveIncludeSourcesRejectsUnresolvableName is the gascity#4620
// regression guard: a --include token that names nothing gc can resolve —
// not a bundled pack, not a registry pack, not a [packs] key, no pack.toml
// on disk — must fail at rig-add time instead of degrading to the literal
// "./<name>" source.
//
// The literal form resolves against the CITY ROOT, where the directory does
// not exist, so persisting it breaks pack expansion for every rig in the
// city, not just the rig being added. Reporting success while writing it is
// the worst outcome: the operator gets no signal at the point of the
// mistake, and the resulting failure presents as a Dolt/connectivity problem.
func TestResolveIncludeSourcesRejectsUnresolvableName(t *testing.T) {
	cityPath := t.TempDir()

	_, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"koolkats"}, nil, nil)
	if err == nil {
		t.Fatal("resolveIncludeSources accepted an unresolvable --include name; it must fail loudly at add time")
	}
	msg := err.Error()
	// The message must name the offending token and the path that was
	// checked, so the operator can act without re-deriving the resolution
	// rules.
	if !strings.Contains(msg, "koolkats") {
		t.Errorf("error does not name the offending token: %q", msg)
	}
	if !strings.Contains(msg, filepath.Join(cityPath, "koolkats")) {
		t.Errorf("error does not report the local path that was checked: %q", msg)
	}
}

// TestResolveIncludeSourcesReportsEveryUnresolvableToken guards the
// diagnosis cost: an operator who passed several bad --include names must
// see all of them in one failure, not fix-and-retry once per token.
func TestResolveIncludeSourcesReportsEveryUnresolvableToken(t *testing.T) {
	cityPath := t.TempDir()

	_, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"koolkats", "oversight-rig"}, nil, nil)
	if err == nil {
		t.Fatal("expected an error for two unresolvable --include names")
	}
	for _, want := range []string{"koolkats", "oversight-rig"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits unresolvable token %q: %q", want, err.Error())
		}
	}
}

// TestResolveIncludeSourcesAcceptsResolvableForms pins the other half of the
// contract: every token gc CAN resolve keeps working, and each resolves
// through the same classifier so the outcome is consistent across names.
// The observed defect surface in gascity#4620 was the split where "gastown"
// canonicalized to a URL while sibling names silently degraded — these cases
// are the ones that must not start failing.
func TestResolveIncludeSourcesAcceptsResolvableForms(t *testing.T) {
	cityPath := t.TempDir()
	writePackDir(t, filepath.Join(cityPath, "localpack"))
	writePackDir(t, filepath.Join(cityPath, "packs", "nested"))
	absPack := filepath.Join(t.TempDir(), "abspack")
	writePackDir(t, absPack)

	bundled, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	packs := map[string]config.PackSource{
		"registered": {Source: "https://github.com/example/registered"},
	}
	registrySource := "https://github.com/example/wespd-cacc-twin-team.git"
	resolveRegistry := func(name string) (string, bool) {
		if name == "wespd/cacc-twin-team" {
			return registrySource, true
		}
		return "", false
	}

	cases := []struct {
		name    string
		include string
		want    string
	}{
		{"bundled builtin name", "gastown", bundled},
		{"bundled builtin under packs/", "packs/gastown", bundled},
		{"registered [packs] key", "registered", "registered"},
		{"local pack dir", "localpack", "localpack"},
		{"local pack dir with ./ prefix", "./localpack", "./localpack"},
		{"nested local pack dir", "packs/nested", "packs/nested"},
		{"absolute local pack dir", absPack, absPack},
		{"https remote source", "https://github.com/example/pack", "https://github.com/example/pack"},
		{"github shorthand remote source", "github.com/example/pack", "github.com/example/pack"},
		{"scp-style remote source", "git@github.com:example/pack.git", "git@github.com:example/pack.git"},
		{"remote source with subpath and ref", "https://github.com/example/pack//sub#v1", "https://github.com/example/pack//sub#v1"},
		// Registry rewrite is precedence 4: validation must run AFTER it, or
		// a valid catalog name would be rejected as an unresolvable bare token.
		{"registry pack name", "wespd/cacc-twin-team", registrySource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{tc.include}, packs, resolveRegistry)
			if err != nil {
				t.Fatalf("resolveIncludeSources(%q) errored: %v", tc.include, err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("resolveIncludeSources(%q) = %v, want [%q]", tc.include, got, tc.want)
			}
		})
	}
}

// TestResolveIncludeSourcesPrefersLocalAndConfiguredOverBuiltin preserves the
// gascity#3137 precedence rules through the added validation: a token that
// names a registered [packs] key or a real local pack dir keeps that
// meaning and is never shadowed by a same-named bundled pack.
func TestResolveIncludeSourcesPrefersLocalAndConfiguredOverBuiltin(t *testing.T) {
	cityPath := t.TempDir()
	writePackDir(t, filepath.Join(cityPath, "gastown"))

	got, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"gastown"}, nil, nil)
	if err != nil {
		t.Fatalf("resolveIncludeSources: %v", err)
	}
	if len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("local pack dir was shadowed by the builtin: got %v, want [\"gastown\"]", got)
	}

	packs := map[string]config.PackSource{"gastown": {Source: "https://github.com/example/gastown"}}
	got, err = resolveIncludeSources(fsys.OSFS{}, t.TempDir(), []string{"gastown"}, packs, nil)
	if err != nil {
		t.Fatalf("resolveIncludeSources: %v", err)
	}
	if len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("configured [packs] key was shadowed by the builtin: got %v, want [\"gastown\"]", got)
	}
}

// TestResolveIncludeSourcesRejectsUnmatchedRegistryName ensures a scoped
// name that the registry catalog does not know is still rejected — the
// post-registry validation is what catches "looks like a name, rewrote
// nothing" tokens that would otherwise become "./owner/pack".
func TestResolveIncludeSourcesRejectsUnmatchedRegistryName(t *testing.T) {
	cityPath := t.TempDir()
	resolve := func(string) (string, bool) { return "", false }
	_, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"wespd/missing-pack"}, nil, resolve)
	if err == nil {
		t.Fatal("expected error for registry miss that left the token unrewritten")
	}
	if !strings.Contains(err.Error(), "wespd/missing-pack") {
		t.Errorf("error does not name the token: %q", err.Error())
	}
}
