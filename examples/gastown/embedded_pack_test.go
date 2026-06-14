package gastown_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gascitypacks "github.com/gastownhall/gascity-packs"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

// The gastown pack is no longer a checked-in copy: the gc binary embeds it
// from the gascity-packs Go module. These integration tests run against
// those exact embedded bytes so a runtime/pack mismatch fails here, in
// gascity CI, before it ships.

var (
	packRootOnce sync.Once
	packRootDir  string
	packRootErr  error
)

// packRoot materializes the module-embedded gastown pack into a shared
// temp root shaped like the historical example layout
// (<root>/packs/gastown/...), so pack-content tests keep their relative
// paths while exercising the embedded bytes. Files get the same modes the
// runtime materializer applies (scripts executable).
func packRoot() string {
	packRootOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gc-embedded-gastown-")
		if err != nil {
			packRootErr = err
			return
		}
		target := filepath.Join(dir, "packs", "gastown")
		packRootErr = fs.WalkDir(gascitypacks.Gastown(), ".", func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dst := filepath.Join(target, filepath.FromSlash(rel))
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			data, err := fs.ReadFile(gascitypacks.Gastown(), rel)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, builtinpacks.MaterializedFileMode(rel))
		})
		if packRootErr == nil {
			packRootDir = dir
		}
	})
	if packRootErr != nil {
		panic("materializing embedded gastown pack: " + packRootErr.Error())
	}
	return packRootDir
}

// primeBundledGastownCache hydrates a hermetic repo cache with the bundled
// synthetic repo at the pinned public release commit, so loading the
// example city's public gastown import resolves offline from the same
// bytes the binary embeds. The cache root follows the $HOME/.gc convention
// shared by packman installs and the config import resolver, so both HOME
// and GC_HOME point at the hermetic root.
func primeBundledGastownCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	coreSource, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	// One pin per repository the example's packs.lock references: the
	// public gastown release (gascity-packs) plus the bundled core/bd pin
	// (gascity.git — core and bd share the repo-shaped synthetic cache).
	for _, pin := range []struct{ source, version string }{
		{config.PublicGastownPackSource, config.PublicGastownPackVersion},
		{coreSource, config.BundledPackImportVersion},
	} {
		commit := strings.TrimPrefix(pin.version, "sha:")
		cachePath, err := packman.RepoCachePath(pin.source, commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%s): %v", pin.source, err)
		}
		if err := builtinpacks.MaterializeSyntheticRepo(cachePath, commit); err != nil {
			t.Fatalf("MaterializeSyntheticRepo(%s): %v", pin.source, err)
		}
	}
}

// gastownRel resolves a test-relative path: embedded gastown pack content
// ("packs/gastown/...") resolves against packRoot(); everything else (the
// example city files, sibling packs like ../bd/dolt) against the example
// directory.
func gastownRel(rel string) string {
	if strings.HasPrefix(rel, "packs/gastown") {
		return filepath.Join(packRoot(), filepath.FromSlash(rel))
	}
	return filepath.Join(exampleDir(), filepath.FromSlash(rel))
}
