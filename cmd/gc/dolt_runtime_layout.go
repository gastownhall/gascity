package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

type managedDoltRuntimeLayout struct {
	PackStateDir string
	DataDir      string
	LogFile      string
	StateFile    string
	PIDFile      string
	LockFile     string
	ConfigFile   string
}

// resolveManagedDoltRuntimeLayout resolves the city's managed dolt runtime
// layout, honoring the GC_DOLT_*/GC_PACK_STATE_DIR/GC_CITY_RUNTIME_DIR
// environment overrides. Use this for operating within a city's own runtime
// context (start/stop/probe), where the ambient env describes that city.
func resolveManagedDoltRuntimeLayout(cityPath string) (managedDoltRuntimeLayout, error) {
	return resolveManagedDoltRuntimeLayoutMode(cityPath, true)
}

// resolveManagedDoltRuntimeLayoutStrict resolves the layout purely from
// cityPath, IGNORING the GC_DOLT_*/GC_PACK_STATE_DIR/GC_CITY_RUNTIME_DIR
// overrides. It is for destructive, target-selecting callers (gc dolt cleanup)
// where an explicit --city must be authoritative: honoring ambient env there
// lets a foreign city's GC_DOLT_DATA_DIR/GC_DOLT_CONFIG_FILE redirect cleanup's
// process match onto another city's live server, and cleanup would then resolve
// and connect to the wrong DB.
func resolveManagedDoltRuntimeLayoutStrict(cityPath string) (managedDoltRuntimeLayout, error) {
	return resolveManagedDoltRuntimeLayoutMode(cityPath, false)
}

func resolveManagedDoltRuntimeLayoutMode(cityPath string, honorEnv bool) (managedDoltRuntimeLayout, error) {
	cityPath = filepath.Clean(strings.TrimSpace(cityPath))
	if cityPath == "" || cityPath == "." {
		return managedDoltRuntimeLayout{}, fmt.Errorf("missing --city")
	}
	cityPath = normalizePathForCompare(cityPath)

	// pathOf returns the env override when honorEnv is set, else the
	// cityPath-derived fallback (path-normalized for comparison either way).
	pathOf := func(key, fallback string) string {
		if honorEnv {
			return defaultEnvPath(key, fallback)
		}
		return normalizePathForCompare(fallback)
	}

	packStateDir := ""
	if honorEnv {
		packStateDir = strings.TrimSpace(os.Getenv("GC_PACK_STATE_DIR"))
		if packStateDir == "" {
			if runtimeDir := strings.TrimSpace(os.Getenv("GC_CITY_RUNTIME_DIR")); runtimeDir != "" {
				packStateDir = filepath.Join(runtimeDir, "packs", "dolt")
			}
		}
	}
	if packStateDir == "" {
		packStateDir = citylayout.PackStateDir(cityPath, "dolt")
	}

	return managedDoltRuntimeLayout{
		PackStateDir: packStateDir,
		DataDir:      pathOf("GC_DOLT_DATA_DIR", filepath.Join(cityPath, ".beads", "dolt")),
		LogFile:      pathOf("GC_DOLT_LOG_FILE", filepath.Join(packStateDir, "dolt.log")),
		StateFile:    pathOf("GC_DOLT_STATE_FILE", filepath.Join(packStateDir, "dolt-provider-state.json")),
		PIDFile:      pathOf("GC_DOLT_PID_FILE", filepath.Join(packStateDir, "dolt.pid")),
		LockFile:     pathOf("GC_DOLT_LOCK_FILE", filepath.Join(packStateDir, "dolt.lock")),
		ConfigFile:   pathOf("GC_DOLT_CONFIG_FILE", filepath.Join(packStateDir, "dolt-config.yaml")),
	}, nil
}

func defaultEnvPath(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return normalizePathForCompare(value)
	}
	return normalizePathForCompare(fallback)
}

func doltRuntimeLayoutFields(layout managedDoltRuntimeLayout) []string {
	return []string{
		"GC_PACK_STATE_DIR\t" + layout.PackStateDir,
		"GC_DOLT_DATA_DIR\t" + layout.DataDir,
		"GC_DOLT_LOG_FILE\t" + layout.LogFile,
		"GC_DOLT_STATE_FILE\t" + layout.StateFile,
		"GC_DOLT_PID_FILE\t" + layout.PIDFile,
		"GC_DOLT_LOCK_FILE\t" + layout.LockFile,
		"GC_DOLT_CONFIG_FILE\t" + layout.ConfigFile,
	}
}
