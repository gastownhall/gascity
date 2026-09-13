package main

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

func loadedCityName(cfg *config.City, cityPath string) string {
	fallback := ""
	if cityPath != "" {
		fallback = filepath.Base(filepath.Clean(cityPath))
	}
	return config.EffectiveCityName(cfg, fallback)
}

func applyRuntimeCityIdentity(cfg *config.City, cityName string) {
	if cfg == nil {
		return
	}
	if name := strings.TrimSpace(cityName); name != "" {
		cfg.ResolvedWorkspaceName = name
	}
}
