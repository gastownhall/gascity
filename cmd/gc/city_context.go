package main

import (
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func resolveExplicitCityPathEnv() (string, bool) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if cityPath, err := validateCityPath(raw); err == nil {
			return cityPath, true
		}
		// GC_CITY (the generic key) additionally accepts a registered city
		// NAME; GC_CITY_PATH / GC_CITY_ROOT are path-only by their names.
		if key == "GC_CITY" && supervisor.IsValidCityName(raw) {
			if entry, ok := supervisor.NewRegistry(supervisor.RegistryPath()).LookupCityByName(raw); ok {
				return entry.Path, true
			}
		}
	}
	return "", false
}

func resolveCityPathFromGCDir() (string, bool) {
	gcDir := strings.TrimSpace(os.Getenv("GC_DIR"))
	if gcDir == "" {
		return "", false
	}
	cityPath, err := findCity(gcDir)
	if err != nil {
		return "", false
	}
	return cityPath, true
}

func resolveCityPathFromCwd() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	cityPath, err := findCity(cwd)
	if err != nil {
		return "", false
	}
	return cityPath, true
}

func rigFromGCDirOrCwd(cityPath string) string {
	if gcDir := strings.TrimSpace(os.Getenv("GC_DIR")); gcDir != "" {
		if rigName := rigFromCwdDir(cityPath, gcDir); rigName != "" {
			return rigName
		}
	}
	return rigFromCwd(cityPath)
}
