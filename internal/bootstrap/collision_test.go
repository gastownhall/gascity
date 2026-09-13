package bootstrap

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCollidesWithBootstrapPack(t *testing.T) {
	tests := []struct {
		name           string
		userImports    map[string]config.Import
		bootstrapNames []string
		want           []string
	}{
		{
			name:           "no user imports returns nil",
			userImports:    nil,
			bootstrapNames: []string{"core", "import", "registry"},
			want:           nil,
		},
		{
			name:           "empty user imports returns nil",
			userImports:    map[string]config.Import{},
			bootstrapNames: []string{"core"},
			want:           nil,
		},
		{
			name:           "no bootstrap names returns nil",
			userImports:    map[string]config.Import{"core": {Source: "x"}},
			bootstrapNames: nil,
			want:           nil,
		},
		{
			name: "no collision when names disjoint",
			userImports: map[string]config.Import{
				"myteam": {Source: "github.com/me/myteam"},
			},
			bootstrapNames: []string{"core", "import", "registry"},
			want:           nil,
		},
		{
			name: "single collision on core",
			userImports: map[string]config.Import{
				"core":   {Source: "github.com/me/core"},
				"myteam": {Source: "github.com/me/myteam"},
			},
			bootstrapNames: []string{"core", "import", "registry"},
			want:           []string{"core"},
		},
		{
			name: "multi-collision sorted",
			userImports: map[string]config.Import{
				"registry": {Source: "github.com/me/registry"},
				"core":     {Source: "github.com/me/core"},
			},
			bootstrapNames: []string{"core", "import", "registry"},
			want:           []string{"core", "registry"},
		},
		{
			name: "duplicate bootstrap name de-duped in output",
			userImports: map[string]config.Import{
				"core": {Source: "github.com/me/core"},
			},
			bootstrapNames: []string{"core", "core", "core"},
			want:           []string{"core"},
		},
		{
			name: "empty bootstrap name ignored",
			userImports: map[string]config.Import{
				"": {Source: "github.com/me/empty"},
			},
			bootstrapNames: []string{""},
			want:           nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CollidesWithBootstrapPack(tc.userImports, tc.bootstrapNames)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CollidesWithBootstrapPack = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBootstrapPackNamesMatchesEntries(t *testing.T) {
	got := PackNames()
	if len(got) != len(BootstrapPacks) {
		t.Fatalf("BootstrapPackNames len = %d, want %d", len(got), len(BootstrapPacks))
	}
	want := make([]string, len(BootstrapPacks))
	for i, e := range BootstrapPacks {
		want[i] = e.Name
	}
	gotCopy := append([]string{}, got...)
	wantCopy := append([]string{}, want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("BootstrapPackNames = %v, want %v", got, want)
	}
}

func TestEnsureBootstrapForCityRefusesWriteOnCollision(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "core",
		Source:   "github.com/gastownhall/gc-core",
		Version:  "0.1.0",
		AssetDir: "packs/core",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()

	userImports := map[string]config.Import{
		"core": {Source: "github.com/me/my-core", Version: "1.0.0"},
	}

	err := EnsureBootstrapForCity(gcHome, userImports)
	if err == nil {
		t.Fatal("EnsureBootstrapForCity should error on collision, got nil")
	}
	if !strings.Contains(err.Error(), "cannot add implicit import") {
		t.Fatalf("error missing user-facing message: %v", err)
	}
	if !strings.Contains(err.Error(), `"core"`) {
		t.Fatalf("error should name the colliding bootstrap pack: %v", err)
	}

	// No entry should have been written: implicit-import.toml must not exist
	// (or must not contain the core entry).
	entries, readErr := readImplicitFile(gcHome + "/implicit-import.toml")
	if readErr != nil {
		t.Fatalf("readImplicitFile: %v", readErr)
	}
	if _, ok := entries["core"]; ok {
		t.Fatalf("core entry should not have been written after collision; entries=%v", entries)
	}
}

func TestEnsureBootstrapForCityNilImportsBehavesAsLegacy(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "core",
		Source:   "github.com/gastownhall/gc-core",
		Version:  "0.1.0",
		AssetDir: "packs/core",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := EnsureBootstrapForCity(gcHome, nil); err != nil {
		t.Fatalf("EnsureBootstrapForCity(nil): %v", err)
	}
	entries, err := readImplicitFile(gcHome + "/implicit-import.toml")
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	if _, ok := entries["core"]; !ok {
		t.Fatalf("core entry should be written when userImports is nil: %v", entries)
	}
}

func TestEnsureBootstrapForCityNonCollidingImportsAllowWrite(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "core",
		Source:   "github.com/gastownhall/gc-core",
		Version:  "0.1.0",
		AssetDir: "packs/core",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	userImports := map[string]config.Import{
		"myteam": {Source: "github.com/me/myteam"},
	}
	if err := EnsureBootstrapForCity(gcHome, userImports); err != nil {
		t.Fatalf("EnsureBootstrapForCity: %v", err)
	}
	entries, err := readImplicitFile(gcHome + "/implicit-import.toml")
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	if _, ok := entries["core"]; !ok {
		t.Fatalf("core entry should be written when user imports don't collide: %v", entries)
	}
}

func TestBootstrapPacksDefaultToEmpty(t *testing.T) {
	if got := PackNames(); len(got) != 0 {
		t.Fatalf("PackNames() = %v, want no default bootstrap implicit imports", got)
	}
}
