package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDetectLegacyV1Surfaces(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *City
		want     int
		contains []string // each warning must contain the corresponding substring (in order)
	}{
		{
			name: "empty config produces no warnings",
			cfg:  &City{},
			want: 0,
		},
		{
			name: "nil config produces no warnings",
			cfg:  nil,
			want: 0,
		},
		{
			name: "agent only",
			cfg: &City{
				Agents: []Agent{{Name: "a"}},
			},
			want:     1,
			contains: []string{"[[agent]] tables are deprecated"},
		},
		{
			name: "packs only",
			cfg: &City{
				Packs: map[string]PackSource{"p": {}},
			},
			want:     1,
			contains: []string{"[packs] is deprecated"},
		},
		{
			name: "workspace.includes only",
			cfg: &City{
				Workspace: Workspace{Includes: []string{"./pack-a"}},
			},
			want:     1,
			contains: []string{"workspace.includes is deprecated"},
		},
		{
			name: "workspace.default_rig_includes only",
			cfg: &City{
				Workspace: Workspace{DefaultRigIncludes: []string{"./pack-b"}},
			},
			want:     1,
			contains: []string{"workspace.default_rig_includes is deprecated"},
		},
		{
			name: "all four in stable order",
			cfg: &City{
				Agents: []Agent{{Name: "a"}},
				Packs:  map[string]PackSource{"p": {}},
				Workspace: Workspace{
					Includes:           []string{"./inc"},
					DefaultRigIncludes: []string{"./drig"},
				},
			},
			want: 4,
			contains: []string{
				"[[agent]] tables are deprecated",
				"[packs] is deprecated",
				"workspace.includes is deprecated",
				"workspace.default_rig_includes is deprecated",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectLegacyV1Surfaces(tc.cfg, "city.toml")
			if len(got) != tc.want {
				t.Fatalf("got %d warnings, want %d: %v", len(got), tc.want, got)
			}
			for i, sub := range tc.contains {
				if !strings.Contains(got[i], sub) {
					t.Errorf("warning %d = %q, expected to contain %q", i, got[i], sub)
				}
				if !strings.HasPrefix(got[i], "city.toml: ") {
					t.Errorf("warning %d = %q, expected source prefix %q", i, got[i], "city.toml: ")
				}
			}
		})
	}
}

// warningsExcludingV1Surfaces filters out warnings produced by
// DetectLegacyV1Surfaces. It exists so tests that exercise unrelated
// composition behavior can ignore the loud v1-surface warnings emitted
// by fixtures that still use [[agent]] / [packs] / workspace.includes
// without rewriting the fixtures.
func warningsExcludingV1Surfaces(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, w := range in {
		if IsLegacyV1SurfaceWarning(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// TestComposeCleanV2CityNoV1SurfaceWarnings verifies that a clean v2
// city.toml that uses only [imports] does NOT trigger v1-surface
// warnings, even when the imported pack internally uses [[agent]].
// This guards the invariant that DetectLegacyV1Surfaces runs against
// the as-parsed city.toml, before pack expansion merges pack-defined
// agents into root.Agents.
func TestComposeCleanV2CityNoV1SurfaceWarnings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "clean-v2"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "clean-v2"
schema = 2

[[agent]]
name = "mayor"
scope = "city"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	for _, w := range prov.Warnings {
		if IsLegacyV1SurfaceWarning(w) {
			t.Errorf("clean v2 city.toml emitted v1-surface warning: %q", w)
		}
	}
}

func TestLoadWithIncludesSkipsLegacyV1SurfaceWarningsWithoutSchema2Pack(t *testing.T) {
	cases := []struct {
		name     string
		packTOML string
	}{
		{name: "no pack toml"},
		{name: "schema 1 pack", packTOML: `
[pack]
name = "legacy-city"
schema = 1
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Dirs["/city/legacy-pack"] = true
			fs.Files["/city/legacy-pack/pack.toml"] = []byte(`
[pack]
name = "legacy-pack"
schema = 1
`)
			fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "legacy-city"
includes = ["legacy-pack"]
default_rig_includes = ["default-pack"]

[[agent]]
name = "worker"

[packs.legacy]
source = "legacy-pack"
`)
			if tc.packTOML != "" {
				fs.Files["/city/pack.toml"] = []byte(tc.packTOML)
			}

			_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			for _, w := range prov.Warnings {
				if IsLegacyV1SurfaceWarning(w) {
					t.Fatalf("schema-1 city emitted v1-surface warning: %q", w)
				}
			}
		})
	}
}

func TestLoadWithIncludesWarnsOnLegacyV1SurfacesInSchema2Fragments(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragments/legacy.toml"]
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "schema2-city"
schema = 2
`)
	fs.Files["/city/fragments/legacy.toml"] = []byte(`
[[agent]]
name = "fragment-worker"
`)

	_, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := warningsExcludingV1Surfaces(prov.Warnings); len(got) != 0 {
		t.Fatalf("unexpected unrelated warnings: %v", got)
	}
	var found bool
	for _, warning := range prov.Warnings {
		if strings.Contains(warning, "/city/fragments/legacy.toml: [[agent]] tables are deprecated") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want schema-2 fragment legacy surface guidance", prov.Warnings)
	}
}

func TestLoadWithIncludesRejectsLegacyV1SurfacesInSchema2City(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "schema2-city"
includes = ["legacy-pack"]
default_rig_includes = ["default-pack"]

[[agent]]
name = "worker"

[packs.legacy]
source = "legacy-pack"
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "schema2-city"
schema = 2
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes succeeded, want hard error for schema-2 city legacy surfaces")
	}
	for _, want := range []string{
		"unsupported PackV1 [[agent]] tables",
		"unsupported PackV1 [packs] entries",
		"unsupported PackV1 workspace.includes",
		"unsupported PackV1 workspace.default_rig_includes",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
}

func TestIsLegacyV1SurfaceWarning(t *testing.T) {
	hits := DetectLegacyV1Surfaces(&City{
		Agents: []Agent{{Name: "a"}},
		Packs:  map[string]PackSource{"p": {}},
		Workspace: Workspace{
			Includes:           []string{"./inc"},
			DefaultRigIncludes: []string{"./drig"},
		},
	}, "city.toml")
	if len(hits) != 4 {
		t.Fatalf("len(hits) = %d, want 4", len(hits))
	}
	for _, w := range hits {
		if !IsLegacyV1SurfaceWarning(w) {
			t.Errorf("IsLegacyV1SurfaceWarning(%q) = false, want true", w)
		}
	}
	if IsLegacyV1SurfaceWarning("some unrelated warning") {
		t.Error("IsLegacyV1SurfaceWarning matched unrelated text")
	}
}

func TestDetectLegacyV1Surfaces_PointsAtDoctorWithoutPromisingInPlaceUpgrade(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{Name: "a"}},
		Packs:  map[string]PackSource{"p": {}},
		Workspace: Workspace{
			Includes:           []string{"./inc"},
			DefaultRigIncludes: []string{"./drig"},
		},
	}
	got := DetectLegacyV1Surfaces(cfg, "city.toml")
	wantSurfaces := []string{
		"[[agent]] tables are deprecated",
		"[packs] is deprecated",
		"workspace.includes is deprecated",
		"workspace.default_rig_includes is deprecated",
	}
	for i, w := range got {
		if !strings.Contains(w, wantSurfaces[i]) {
			t.Errorf("warning %d = %q, want surface %q", i, w, wantSurfaces[i])
		}
		if strings.Contains(w, "[packs] is deprecated") {
			if !strings.Contains(w, "Run `gc doctor` to inspect; `gc doctor --fix` migrates entries referenced by legacy workspace include lists, then migrate or remove any remaining [packs] entries manually.") {
				t.Errorf("warning %d = %q, expected [packs] cleanup guidance", i, w)
			}
		} else if !strings.Contains(w, "Run `gc doctor` to inspect; `gc doctor --fix` handles the safe mechanical rewrites available in this wave.") {
			t.Errorf("warning %d = %q, expected gc doctor guidance", i, w)
		}
		if strings.Contains(w, "gc import migrate") {
			t.Errorf("warning %d = %q, should not recommend gc import migrate", i, w)
		}
	}
}

func TestLegacyV1SurfaceErrorAggregatesViolations(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{Name: "a"}},
		Packs:  map[string]PackSource{"p": {}},
		Workspace: Workspace{
			Includes:           []string{"./inc"},
			DefaultRigIncludes: []string{"./drig"},
		},
	}

	err := LegacyV1SurfaceError(cfg, "city.toml")
	if err == nil {
		t.Fatal("LegacyV1SurfaceError returned nil, want aggregated error")
	}
	for _, want := range []string{
		"PackV1 config surfaces are no longer supported",
		"city.toml: unsupported PackV1 [[agent]] tables",
		"city.toml: unsupported PackV1 [packs] entries",
		"city.toml: unsupported PackV1 workspace.includes",
		"city.toml: unsupported PackV1 workspace.default_rig_includes",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
}
