//go:build acceptance_a

// Helpers for locating the gastown pack content that gc materializes into
// the user-global repo cache. The gastown example city composes the pack via
// a pinned public import (committed packs.lock); the gc binary self-heals
// the cache for bundled locked sources from its embedded copy, so the cache
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

// gastownCachePackDir resolves the gastown pack content directory inside the
// user-global repo cache from the city's packs.lock pin. It fails the test
// when the city has no gastown pin or when gc has not materialized the
// pinned source into the cache.
func gastownCachePackDir(t *testing.T, c *helpers.City) string {
	t.Helper()
	lockData := c.ReadFile("packs.lock")
	var lock packsLockFile
	if _, err := toml.Decode(lockData, &lock); err != nil {
		t.Fatalf("parsing packs.lock: %v", err)
	}
	for source, pin := range lock.Packs {
		if name, ok := builtinpacks.NameForSource(source); !ok || name != "gastown" {
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
			t.Fatalf("gastown pack not materialized in repo cache at %s: %v", packDir, err)
		}
		return packDir
	}
	t.Fatalf("packs.lock has no gastown entry:\n%s", lockData)
	return "" // unreachable
}
