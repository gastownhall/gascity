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

// ensureBundledLockedRemoteImportsCached hydrates the shared repo cache for
// every bundled pack source pinned in packs.lock so config load can resolve
// locked bundled imports without network access or a prior "gc import
// install". A cache that already validates is skipped lock-free; only on
// validation failure does the preflight take the write-locked
// packman.EnsureRepoInCache repair path, which revalidates under the lock
// (a concurrent repair between the two checks is therefore benign).
func ensureBundledLockedRemoteImportsCached(cityPath string) error {
	lock, err := readImportLockfile(fsys.OSFS{}, cityPath)
	if err != nil {
		return err
	}
	if len(lock.Packs) == 0 {
		return nil
	}

	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		if builtinpacks.IsSource(source) {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := lock.Packs[source]
		if strings.TrimSpace(pack.Commit) == "" {
			return fmt.Errorf("lock entry %q is missing commit", source)
		}
		cachePath, err := packman.RepoCachePath(source, pack.Commit)
		if err != nil {
			return fmt.Errorf("resolving cache path for bundled import %q from packs.lock: %w", source, err)
		}
		if builtinpacks.ValidateSyntheticRepo(cachePath, pack.Commit) == nil {
			continue
		}
		if _, err := packman.EnsureRepoInCache(source, pack.Commit); err != nil {
			return fmt.Errorf("caching bundled import %q from packs.lock: %w", source, err)
		}
	}
	return nil
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
