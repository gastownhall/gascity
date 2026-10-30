package rig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestParseIncludeSpecs pins when an --include token is read as
// "<binding>=<source>": only when the left side matches the binding grammar
// AND the whole token does not already resolve as a source, so no working
// include changes meaning.
func TestParseIncludeSpecs(t *testing.T) {
	city := t.TempDir()
	literalDir := filepath.Join(city, "tools=x")
	if err := os.MkdirAll(literalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(literalDir, "pack.toml"), []byte("[pack]\nname = \"tools\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packs := map[string]config.PackSource{"a=b": {Source: "https://example.com/ab.git"}}

	cases := []struct {
		token       string
		wantBinding string
		wantSource  string
	}{
		{"gastown", "", "gastown"},
		{"tools=https://github.com/o/r/tree/main/p", "tools", "https://github.com/o/r/tree/main/p"},
		{"my_pack-2=./packs/foo", "my_pack-2", "./packs/foo"},
		{"tools= https://example.com/p.git", "tools", "https://example.com/p.git"},
		{"https://host/x?a=b", "", "https://host/x?a=b"},
		{"./tools=x", "", "./tools=x"},
		{"packs/a=b", "", "packs/a=b"},
		{"=foo", "", "=foo"},
		{"-x=foo", "", "-x=foo"},
		{"tools =x", "", "tools =x"},
		{"a=b", "", "a=b"},
		{"tools=x", "", "tools=x"},
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			specs, err := parseIncludeSpecs(fsys.OSFS{}, city, []string{tc.token}, packs)
			if err != nil {
				t.Fatalf("parseIncludeSpecs(%q) error: %v", tc.token, err)
			}
			if len(specs) != 1 {
				t.Fatalf("parseIncludeSpecs(%q) = %d specs, want 1", tc.token, len(specs))
			}
			got := specs[0]
			if got.Binding != tc.wantBinding || got.Source != tc.wantSource || got.Token != tc.token {
				t.Fatalf("parseIncludeSpecs(%q) = %+v, want binding %q source %q", tc.token, got, tc.wantBinding, tc.wantSource)
			}
		})
	}
}

func TestParseIncludeSpecsRejectsBindingWithoutSource(t *testing.T) {
	for _, token := range []string{"tools=", "tools=  "} {
		_, err := parseIncludeSpecs(fsys.OSFS{}, t.TempDir(), []string{token}, nil)
		if err == nil || !strings.Contains(err.Error(), "no pack source") || !strings.Contains(err.Error(), `"tools"`) {
			t.Fatalf("parseIncludeSpecs(%q) error = %v, want a no-pack-source error naming the binding", token, err)
		}
	}
}

// TestResolveIncludeSpecsCanonicalizesSourcesInPlace proves a named include
// takes the same builtin/registry canonicalization as an unnamed one.
func TestResolveIncludeSpecsCanonicalizesSourcesInPlace(t *testing.T) {
	resolver := func(name string) (string, bool) {
		if name == "lighthouse" {
			return "https://packages.example/lighthouse.git", true
		}
		return "", false
	}
	specs, err := resolveIncludeSpecs(fsys.OSFS{}, t.TempDir(), []string{"lighthouse", "ops=lighthouse"}, nil, resolver)
	if err != nil {
		t.Fatalf("resolveIncludeSpecs: %v", err)
	}
	want := []includeSpec{
		{Token: "lighthouse", Source: "https://packages.example/lighthouse.git"},
		{Token: "ops=lighthouse", Binding: "ops", Source: "https://packages.example/lighthouse.git"},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("resolveIncludeSpecs = %+v, want %+v", specs, want)
	}
	if got := includeSpecSources(specs); !reflect.DeepEqual(got, []string{want[0].Source, want[1].Source}) {
		t.Fatalf("includeSpecSources = %v", got)
	}
}

func TestResolveIncludeSpecsRejectsUnresolvableNamedSource(t *testing.T) {
	_, err := resolveIncludeSpecs(fsys.OSFS{}, t.TempDir(), []string{"tools=missing-pack"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `--include "missing-pack" does not resolve to a pack`) {
		t.Fatalf("resolveIncludeSpecs error = %v, want the unresolvable-include error for the source", err)
	}
}

func TestBindIncludeSpecs(t *testing.T) {
	packs := map[string]config.PackSource{"ops": {Source: "https://example.com/ops-pack.git"}}
	unnamed := func(sources ...string) []includeSpec {
		out := make([]includeSpec, 0, len(sources))
		for _, s := range sources {
			out = append(out, includeSpec{Token: s, Source: s})
		}
		return out
	}

	t.Run("unnamed only matches the legacy converter", func(t *testing.T) {
		sources := []string{"https://example.com/a/tools.git", "https://example.com/b/tools.git", "https://example.com/a/tools.git"}
		got, err := bindIncludeSpecs(unnamed(sources...), packs)
		if err != nil {
			t.Fatal(err)
		}
		want := config.BoundImportsFromLegacySources(sources, packs)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bindIncludeSpecs = %+v, want %+v", got, want)
		}
		if len(got) != 2 || got[1].Binding != "tools-2" {
			t.Fatalf("want the -2 suffix case preserved, got %+v", got)
		}
	})

	t.Run("named spec overrides the binding", func(t *testing.T) {
		got, err := bindIncludeSpecs([]includeSpec{{Token: "ops=https://example.com/tools.git", Binding: "ops", Source: "https://example.com/tools.git"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []config.BoundImport{{Binding: "ops", Import: config.Import{Source: "https://example.com/tools.git"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bindIncludeSpecs = %+v, want %+v", got, want)
		}
	})

	t.Run("named packs key keeps its configured source", func(t *testing.T) {
		got, err := bindIncludeSpecs([]includeSpec{{Token: "tools=ops", Binding: "tools", Source: "ops"}}, packs)
		if err != nil {
			t.Fatal(err)
		}
		want := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: "https://example.com/ops-pack.git"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bindIncludeSpecs = %+v, want %+v", got, want)
		}
	})

	t.Run("duplicate named binding with different sources errors", func(t *testing.T) {
		_, err := bindIncludeSpecs([]includeSpec{
			{Token: "tools=https://example.com/a.git", Binding: "tools", Source: "https://example.com/a.git"},
			{Token: "tools=https://example.com/b.git", Binding: "tools", Source: "https://example.com/b.git"},
		}, nil)
		if err == nil {
			t.Fatal("want a collision error")
		}
		for _, want := range []string{`binding "tools"`, `"tools=https://example.com/a.git"`, `"tools=https://example.com/b.git"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %s", err, want)
			}
		}
	})

	t.Run("named binding equal to a derived binding with another source errors", func(t *testing.T) {
		specs := append(unnamed("https://example.com/tools.git"),
			includeSpec{Token: "tools=https://example.com/other.git", Binding: "tools", Source: "https://example.com/other.git"})
		_, err := bindIncludeSpecs(specs, nil)
		if err == nil {
			t.Fatal("want a collision error")
		}
		for _, want := range []string{`binding "tools"`, `"https://example.com/tools.git"`, `"tools=https://example.com/other.git"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %s", err, want)
			}
		}
	})

	t.Run("same binding and source collapses", func(t *testing.T) {
		specs := append(unnamed("https://example.com/tools.git"),
			includeSpec{Token: "tools=https://example.com/tools.git", Binding: "tools", Source: "https://example.com/tools.git"},
			includeSpec{Token: "tools=https://example.com/tools.git", Binding: "tools", Source: "https://example.com/tools.git"})
		got, err := bindIncludeSpecs(specs, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: "https://example.com/tools.git"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bindIncludeSpecs = %+v, want %+v", got, want)
		}
	})

	t.Run("same source under different bindings keeps both", func(t *testing.T) {
		specs := append(unnamed("https://example.com/tools.git"),
			includeSpec{Token: "ops=https://example.com/tools.git", Binding: "ops", Source: "https://example.com/tools.git"})
		got, err := bindIncludeSpecs(specs, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []config.BoundImport{
			{Binding: "ops", Import: config.Import{Source: "https://example.com/tools.git"}},
			{Binding: "tools", Import: config.Import{Source: "https://example.com/tools.git"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("bindIncludeSpecs = %+v, want %+v", got, want)
		}
	})
}

var errResolveIncludeSentinel = errors.New("resolve-include short-circuit")

func TestProvisionFreshAddRoutesIncludesThroughResolveIncludeImports(t *testing.T) {
	deps := stubDeps(t.TempDir())
	composeCalled := false
	deps.ComposePacks = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		composeCalled = true
		return nil, nil, nil
	}
	var got []config.BoundImport
	deps.ResolveIncludeImports = func(_ string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
		got = imports
		return nil, nil, errResolveIncludeSentinel
	}
	_, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     filepath.Join(t.TempDir(), "rig"),
		Includes: []string{"tools=https://example.com/p.git"},
	})
	if !errors.Is(err, errResolveIncludeSentinel) {
		t.Fatalf("Provision err = %v, want the ResolveIncludeImports sentinel", err)
	}
	if !strings.HasPrefix(err.Error(), "resolving rig imports: ") {
		t.Fatalf("Provision err = %q, want the resolving rig imports prefix", err)
	}
	want := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: "https://example.com/p.git"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveIncludeImports input = %+v, want %+v", got, want)
	}
	if composeCalled {
		t.Fatal("ComposePacks ran for a fresh add with explicit includes")
	}
}

func TestProvisionReAddKeepsComposePacksForIncludes(t *testing.T) {
	deps := depsWithComposeSentinel(t)
	rigPath := filepath.Join(t.TempDir(), "rig")
	deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "r", Path: rigPath}}}
	deps.ResolveIncludeImports = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		t.Error("ResolveIncludeImports ran on a re-add, which ignores --include")
		return nil, nil, nil
	}
	_, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     rigPath,
		Includes: []string{"tools=https://example.com/p.git"},
	})
	if !errors.Is(err, errComposeSentinel) {
		t.Fatalf("Provision err = %v, want the ComposePacks sentinel", err)
	}
}

func TestProvisionNilResolveIncludeImportsFallsBackToComposePacks(t *testing.T) {
	deps := depsWithComposeSentinel(t)
	var got []config.BoundImport
	deps.ComposePacks = func(_ string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
		got = imports
		return nil, nil, errComposeSentinel
	}
	_, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     filepath.Join(t.TempDir(), "rig"),
		Includes: []string{"https://example.com/tools.git"},
	})
	if !errors.Is(err, errComposeSentinel) {
		t.Fatalf("Provision err = %v, want the ComposePacks sentinel", err)
	}
	if !strings.HasPrefix(err.Error(), "installing bundled rig imports: ") {
		t.Fatalf("Provision err = %q, want the historical installing bundled rig imports prefix", err)
	}
	want := []config.BoundImport{{Binding: "tools", Import: config.Import{Source: "https://example.com/tools.git"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ComposePacks input = %+v, want %+v", got, want)
	}
}

// TestProvisionRigPathCollisionWinsOverIncludeResolution documents that
// re-add detection runs before include resolution: a rig name registered at
// another path fails without reaching the network-capable resolver.
func TestProvisionRigPathCollisionWinsOverIncludeResolution(t *testing.T) {
	deps := stubDeps(t.TempDir())
	deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "r", Path: filepath.Join(t.TempDir(), "elsewhere")}}}
	deps.ResolveIncludeImports = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		t.Error("ResolveIncludeImports ran before the rig path collision check")
		return nil, nil, nil
	}
	_, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     filepath.Join(t.TempDir(), "rig"),
		Includes: []string{"tools=https://example.com/p.git"},
	})
	if err == nil || !strings.Contains(err.Error(), `rig "r" already registered at`) {
		t.Fatalf("Provision err = %v, want the rig path collision error", err)
	}
}

// TestProvisionPrefixCollisionWinsOverIncludeResolution documents that the
// prefix collision check runs before include resolution: a fresh add whose
// prefix collides fails without reaching the network-capable resolver.
func TestProvisionPrefixCollisionWinsOverIncludeResolution(t *testing.T) {
	deps := stubDeps(t.TempDir())
	deps.Cfg = &config.City{Rigs: []config.Rig{{Name: "other", Path: filepath.Join(t.TempDir(), "other"), Prefix: "rp"}}}
	deps.ResolveIncludeImports = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		t.Error("ResolveIncludeImports ran before the prefix collision check")
		return nil, nil, nil
	}
	_, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     filepath.Join(t.TempDir(), "rig"),
		Prefix:   "rp",
		Includes: []string{"tools=https://example.com/p.git"},
	})
	if err == nil || !strings.Contains(err.Error(), `prefix "rp" collides with other`) {
		t.Fatalf("Provision err = %v, want the prefix collision error", err)
	}
}

// TestProvisionReAddIncludeWarningNamesTheTokens pins the re-add warning to
// the --include tokens as given, so a binding that differs from the existing
// rig's is visible next to the existing imports.
func TestProvisionReAddIncludeWarningNamesTheTokens(t *testing.T) {
	deps := stubDeps(t.TempDir())
	rigPath := t.TempDir()
	deps.Cfg = &config.City{Rigs: []config.Rig{{
		Name:    "r",
		Path:    rigPath,
		Imports: map[string]config.Import{"tools": {Source: "https://example.com/p.git"}},
	}}}
	deps.ComposePacks = func(_ string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
		return imports, nil, nil
	}
	var warnings []string
	deps.OnStep = func(step ProvisionStep) {
		if step.Name == "include-ignored" {
			warnings = append(warnings, step.Detail)
		}
	}
	if _, _, err := Provision(deps, ProvisionRequest{
		Name:     "r",
		Path:     rigPath,
		Includes: []string{" ops=https://example.com/p.git "},
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := "--include flags [ops=https://example.com/p.git] ignored (existing imports: tools=https://example.com/p.git)"
	if len(warnings) != 1 || !strings.Contains(warnings[0], want) {
		t.Fatalf("include-ignored warnings = %q, want one containing %q", warnings, want)
	}
}

func TestInheritExistingImportVersions(t *testing.T) {
	rig := &config.Rig{Imports: map[string]config.Import{
		"tools": {Source: "https://example.com/tools.git", Version: "^1.4"},
		"other": {Source: "https://example.com/other.git", Version: "^2.0"},
	}}
	in := []config.BoundImport{
		{Binding: "tools", Import: config.Import{Source: "https://example.com/tools.git"}},
		{Binding: "other", Import: config.Import{Source: "https://example.com/moved.git"}},
		{Binding: "pinned", Import: config.Import{Source: "https://example.com/p.git", Version: "^3.0"}},
		{Binding: "fresh", Import: config.Import{Source: "https://example.com/f.git"}},
	}
	got := inheritExistingImportVersions(in, rig)
	want := []config.BoundImport{
		{Binding: "tools", Import: config.Import{Source: "https://example.com/tools.git", Version: "^1.4"}},
		{Binding: "other", Import: config.Import{Source: "https://example.com/moved.git"}},
		{Binding: "pinned", Import: config.Import{Source: "https://example.com/p.git", Version: "^3.0"}},
		{Binding: "fresh", Import: config.Import{Source: "https://example.com/f.git"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inheritExistingImportVersions = %+v, want %+v", got, want)
	}
	if in[0].Import.Version != "" {
		t.Fatalf("input slice was mutated: %+v", in[0])
	}
	if got := inheritExistingImportVersions(in, nil); !reflect.DeepEqual(got, in) {
		t.Fatalf("nil rig changed imports: %+v", got)
	}
}
