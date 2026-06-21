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
		//
		// Precedence note (intentional, differs from the positional arg and
		// --city flag): an env var is set deliberately and is ambient, so it
		// uses path-first / local-wins — validateCityPath above already returned
		// a same-named local city dir if one exists, and only an unshadowed name
		// reaches the registry lookup here. The interactive positional and
		// --city forms instead raise a loud ambiguity error when a local city
		// and a different registration collide (see resolveCityNameRef).
		//
		// Registry-error note (intentional): this uses the lenient bool
		// LookupCityByName, not LookupCityByNameE, so a corrupt/unreadable
		// registry collapses to a miss and this ambient env path falls through
		// to GC_DIR/cwd discovery rather than hard-erroring. The interactive
		// positional/--city paths use LookupCityByNameE to surface that error
		// because the user named a city explicitly there; the env path is
		// best-effort by design (pinned by
		// TestResolveExplicitCityPathEnvNameBestEffortOnCorruptRegistry).
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
