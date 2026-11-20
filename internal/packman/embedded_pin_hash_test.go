package packman

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packregistry"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// The binary serves the public gastown and gascity pins offline from the pack
// content it embeds. A published pin names a registry release whose
// content hash is exactly the embedded tree; otherwise gc init writes a pin
// whose real bytes differ from what the city runs. The gascity pin need not be
// the embedded module commit (the module commit may carry a byte-identical
// gascity tree), so it compares content. An unpublished compatibility pin
// must instead match the module commit exactly; an unrelated old pin fails.
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
				// A Gas City-only module update must not manufacture a Gastown
				// registry release. Its unchanged compatibility tree is proven
				// by the exact module commit instead; tags still require the
				// registry hash path because they contain no commit token.
				if tc.name == "gastown" && publicPinNamesEmbeddedModule(t, commit) {
					return
				}
				t.Fatalf("%s pin %s has neither registry release provenance nor exact embedded module provenance in %s", tc.name, tc.pin, realRegistryFixture)
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

// publicPinNamesEmbeddedModule verifies immutable module provenance without
// inventing a registry release for an unchanged compatibility pack.
func publicPinNamesEmbeddedModule(t *testing.T, commit string) bool {
	t.Helper()
	root := bazeltest.OverrideRoot()
	if root == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		root = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	}
	path := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range mod.Require {
		if req.Mod.Path != "github.com/gastownhall/gascity-packs" {
			continue
		}
		if !module.IsPseudoVersion(req.Mod.Version) {
			return false
		}
		revision, err := module.PseudoVersionRev(req.Mod.Version)
		if err != nil {
			t.Fatal(err)
		}
		return strings.HasPrefix(commit, revision)
	}
	t.Fatal("go.mod has no gascity-packs requirement")
	return false
}

func TestUnpublishedPublicPackPinRequiresExactModuleProvenance(t *testing.T) {
	if !publicPinNamesEmbeddedModule(t, strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")) {
		t.Fatal("compatibility pin does not name the embedded module")
	}
	if publicPinNamesEmbeddedModule(t, "124902097faa3d2e4de69e5ac5fe9e5109cad24f") {
		t.Fatal("previous module commit incorrectly accepted as current provenance")
	}
}
