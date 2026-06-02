package main

import (
	"path/filepath"

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
