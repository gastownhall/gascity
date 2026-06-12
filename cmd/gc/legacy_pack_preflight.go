package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// ensureLegacyNamedPacksCached preserves legacy [packs] compatibility.
// Schema-2 remote imports use gc import install and shared-cache resolution;
// legacy named packs still rely on the city-local cache populated by gc pack fetch.
func ensureLegacyNamedPacksCached(cityPath string) error {
	tomlPath := filepath.Join(cityPath, "city.toml")
	if quickCfg, qErr := config.Load(fsys.OSFS{}, tomlPath); qErr == nil && len(quickCfg.Packs) > 0 {
		if err := config.FetchPacks(quickCfg.Packs, cityPath); err != nil {
			return err
		}
	}
	return nil
}

// lockedBundledImport is a bundled pack source pinned at its canonical commit
// in packs.lock — the running binary can serve embedded content for it.
type lockedBundledImport struct {
	source string
	commit string
}

// lockedBundledCanonicalImports returns the bundled pack sources pinned at
// their canonical commit in packs.lock, sorted by source. A bundled source
// pinned at a non-canonical commit is an ordinary remote import (gc import
// install owns fetching it; the binary never serves embedded content for it)
// and is skipped. A lock entry missing its commit is a hard error.
func lockedBundledCanonicalImports(cityPath string) ([]lockedBundledImport, error) {
	lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, err
	}
	if len(lock.Packs) == 0 {
		return nil, nil
	}

	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		if builtinpacks.IsSource(source) {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)

	imports := make([]lockedBundledImport, 0, len(sources))
	for _, source := range sources {
		pack := lock.Packs[source]
		if strings.TrimSpace(pack.Commit) == "" {
			return nil, fmt.Errorf("lock entry %q is missing commit", source)
		}
		if !config.IsBundledSourceAtCanonicalPin(source, pack.Commit) {
			continue
		}
		imports = append(imports, lockedBundledImport{source: source, commit: pack.Commit})
	}
	return imports, nil
}

// ensureBundledLockedRemoteImportsCached hydrates the shared repo cache for
// every bundled pack source pinned in packs.lock so config load can resolve
// locked bundled imports without network access or a prior "gc import
// install". A cache that already validates is skipped lock-free; only on
// validation failure does the preflight take the write-locked
// packman.EnsureRepoInCache repair path, which revalidates under the lock
// (a concurrent repair between the two checks is therefore benign).
func ensureBundledLockedRemoteImportsCached(cityPath string) error {
	imports, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		return err
	}
	for _, imp := range imports {
		cachePath, err := packman.RepoCachePath(imp.source, imp.commit)
		if err != nil {
			return fmt.Errorf("resolving cache path for bundled import %q from packs.lock: %w", imp.source, err)
		}
		if builtinpacks.ValidateSyntheticRepo(cachePath, imp.commit) == nil {
			continue
		}
		if _, err := packman.EnsureRepoInCache(imp.source, imp.commit); err != nil {
			return fmt.Errorf("caching bundled import %q from packs.lock: %w", imp.source, err)
		}
	}
	return nil
}

// lockedBundledImportsUsable reports whether every bundled pack source pinned
// at its canonical commit in packs.lock has a valid synthetic cache. The ready
// fast path in EnsureBuiltinRuntimeAssets pairs it with
// requiredBuiltinSourcesUsable so an evicted or corrupted optional locked
// bundled cache (for example gastown or gascity) still forces re-hydration
// after the city was marked ready, instead of letting config load fail on the
// locked-but-missing synthetic cache. A lockfile that cannot be read or that
// has a malformed entry is reported unusable so the caller falls through to
// ensureBundledLockedRemoteImportsCached, which surfaces the underlying error.
func lockedBundledImportsUsable(cityPath string) bool {
	imports, err := lockedBundledCanonicalImports(cityPath)
	if err != nil {
		return false
	}
	for _, imp := range imports {
		cachePath, err := packman.RepoCachePath(imp.source, imp.commit)
		if err != nil {
			return false
		}
		if builtinpacks.ValidateSyntheticRepo(cachePath, imp.commit) != nil {
			return false
		}
	}
	return true
}

var ensureInitRemoteImportsInstalled = installInitRemoteImports

func installInitRemoteImports(cityPath string) error {
	allImports, err := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if err != nil {
		return err
	}
	if !hasRemoteImport(allImports) {
		return nil
	}
	lock, err := syncImports(cityPath, allImports, packman.InstallResolveIfNeeded)
	if err != nil {
		return err
	}
	if err := writeImportLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		return err
	}
	if _, err := installLockedImports(cityPath); err != nil {
		return err
	}
	return nil
}

func hasRemoteImport(imports map[string]config.Import) bool {
	for _, imp := range imports {
		if isRemoteImportSource(imp.Source) {
			return true
		}
	}
	return false
}
