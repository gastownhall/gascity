package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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
