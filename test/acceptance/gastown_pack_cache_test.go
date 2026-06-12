//go:build acceptance_a

// Helpers for locating the gastown pack content that gc materializes into
// the user-global repo cache. The gastown example city composes the pack via
// a pinned public import (committed packs.lock); the gc binary self-heals
// the cache for bundled sources locked at their canonical pin from its
// embedded copy, so the cache
// — not a city-local packs/ directory — is where materialized pack
// artifacts live.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/remotesource"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// packsLockFile mirrors the on-disk packs.lock schema written by gc init.
type packsLockFile struct {
	Schema int                      `toml:"schema"`
	Packs  map[string]packsLockPack `toml:"packs"`
}

// packsLockPack is a single pinned source entry in packs.lock.
type packsLockPack struct {
	Version string `toml:"version"`
	Commit  string `toml:"commit"`
}

// cachePackDirByName resolves a builtin pack's content directory inside the
// user-global repo cache from the city's packs.lock pin. It fails the test
// when the city has no pin for that pack or when gc has not materialized
// the pinned source into the cache.
func cachePackDirByName(t *testing.T, c *helpers.City, name string) string {
	t.Helper()
	lockData := c.ReadFile("packs.lock")
	var lock packsLockFile
	if _, err := toml.Decode(lockData, &lock); err != nil {
		t.Fatalf("parsing packs.lock: %v", err)
	}
	for source, pin := range lock.Packs {
		if got, ok := builtinpacks.NameForSource(source); !ok || got != name {
			continue
		}
		commit := pin.Commit
		if commit == "" {
			commit = strings.TrimPrefix(pin.Version, "sha:")
		}
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatalf("resolving repo cache path for %s: %v", source, err)
		}
		packDir := filepath.Join(cachePath, filepath.FromSlash(remotesource.Parse(source).Subpath))
		if _, err := os.Stat(filepath.Join(packDir, "pack.toml")); err != nil {
			t.Fatalf("%s pack not materialized in repo cache at %s: %v", name, packDir, err)
		}
		return packDir
	}
	t.Fatalf("packs.lock has no %s entry:\n%s", name, lockData)
	return "" // unreachable
}

// gastownCachePackDir resolves the gastown pack content directory inside
// the user-global repo cache from the city's packs.lock pin.
func gastownCachePackDir(t *testing.T, c *helpers.City) string {
	t.Helper()
	return cachePackDirByName(t, c, "gastown")
}
