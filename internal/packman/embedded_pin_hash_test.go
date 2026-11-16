package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packregistry"
)

// The binary serves the public gastown and gascity pins offline from the pack
// content it embeds. Each pin must therefore name a registry release whose
// content hash is exactly the embedded tree; otherwise gc init writes a pin
// whose real bytes differ from what the city runs. The gascity pin need not be
// the embedded module commit (the module commit may carry a byte-identical
// gascity tree), so this compares content, not commits.
func TestEmbeddedPublicPacksHashToTheirPinnedRegistryReleases(t *testing.T) {
	data, err := os.ReadFile(realRegistryFixture)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := packregistry.ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		pin  string
	}{
		{"gastown", config.PublicGastownPackVersion},
		{"gascity", config.PublicGascityPackVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commit := strings.TrimPrefix(tc.pin, "sha:")
			want := ""
			for _, pack := range catalog.Packs {
				if pack.Name != tc.name {
					continue
				}
				for _, release := range pack.Releases {
					if release.Commit == commit {
						want = release.Hash
					}
				}
			}
			if want == "" {
				t.Fatalf("%s pin %s is not a %s release in %s; refresh the fixture from gascity-packs", tc.name, tc.pin, tc.name, realRegistryFixture)
			}

			dst := filepath.Join(t.TempDir(), "repo")
			if err := builtinpacks.MaterializeSyntheticRepo(dst, builtinpacks.PublicRepository, commit); err != nil {
				t.Fatalf("materialize embedded %s: %v", builtinpacks.PublicRepository, err)
			}
			got, err := packregistry.PackDirContentHash(filepath.Join(dst, tc.name))
			if err != nil {
				t.Fatalf("hash embedded %s: %v", tc.name, err)
			}
			if got != want {
				t.Fatalf("embedded %s content hash = %s, but its pin %s is registry release hash %s", tc.name, got, tc.pin, want)
			}
		})
	}
}
