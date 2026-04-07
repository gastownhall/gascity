package main

import (
	"os"
	"strings"
)

func resolveExplicitCityPathEnv() (string, bool) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			if cityPath, err := validateCityPath(raw); err == nil {
				return cityPath, true
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
