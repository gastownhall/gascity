package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// bdCommandRunnerForCity centralizes bd subprocess env construction so all
// GC-managed bd calls resolve Dolt against the same city-scoped runtime.
// Env is rebuilt on each call so GC_DOLT_PORT reflects the current managed
// dolt port (which can change across city restarts).
func bdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env := bdRuntimeEnv(cityPath)
		runner := beads.ExecCommandRunnerWithEnv(env)
		return runner(dir, name, args...)
	}
}

func bdStoreForCity(dir, cityPath string) *beads.BdStore {
	return beads.NewBdStore(dir, bdCommandRunnerForCity(cityPath))
}

func bdStoreForDir(dir string) *beads.BdStore {
	return bdStoreForCity(dir, cityForStoreDir(dir))
}

func bdRuntimeEnv(cityPath string) map[string]string {
	env := citylayout.CityRuntimeEnvMap(cityPath)
	env["BEADS_DIR"] = filepath.Join(cityPath, ".beads")
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	if rawBeadsProvider(cityPath) != "bd" {
		return env
	}
	// Propagate Dolt host/port from per-city config (registered by
	// startBeadsLifecycle) or user env vars. Per-city config avoids
	// process-global env pollution that breaks supervisor multi-tenancy.
	if host := doltHostForCity(cityPath); host != "" {
		env["GC_DOLT_HOST"] = host
	}
	// External host: use the port from config or user env.
	if isExternalDolt(cityPath) {
		if port := doltPortForCity(cityPath); port != "" {
			env["GC_DOLT_PORT"] = port
		}
		return env
	}
	if port := currentDoltPort(cityPath); port != "" {
		env["GC_DOLT_PORT"] = port
		return env
	}
	// Best-effort recovery for managed cities: if state is stale or missing,
	// ask the provider to repair itself before bd falls back to auto-start.
	if err := healthBeadsProvider(cityPath); err == nil {
		if port := currentDoltPort(cityPath); port != "" {
			env["GC_DOLT_PORT"] = port
		}
	}
	return env
}

func cityRuntimeProcessEnv(cityPath string) []string {
	overrides := citylayout.CityRuntimeEnvMap(cityPath)
	if rawBeadsProvider(cityPath) == "bd" {
		if host := doltHostForCity(cityPath); host != "" {
			overrides["GC_DOLT_HOST"] = host
		}
		if isExternalDolt(cityPath) {
			if port := doltPortForCity(cityPath); port != "" {
				overrides["GC_DOLT_PORT"] = port
			}
		} else if port := currentDoltPort(cityPath); port != "" {
			overrides["GC_DOLT_PORT"] = port
		}
	}
	return mergeRuntimeEnv(os.Environ(), overrides)
}

func cityForStoreDir(dir string) string {
	if gcCity := os.Getenv("GC_CITY"); gcCity != "" {
		if p, err := findCity(gcCity); err == nil {
			return p
		}
	}
	if p, err := findCity(dir); err == nil {
		return p
	}
	return dir
}

func mergeRuntimeEnv(environ []string, overrides map[string]string) []string {
	keys := []string{
		"BEADS_DIR",
		"GC_CITY",
		"GC_CITY_ROOT",
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_PACK_STATE_DIR",
		"GC_RIG",
		"GC_RIG_ROOT",
	}
	if len(overrides) > 0 {
		for key := range overrides {
			if !containsString(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	out := append([]string(nil), environ...)
	for _, key := range keys {
		out = removeEnvKey(out, key)
	}
	overrideKeys := make([]string, 0, len(overrides))
	for key := range overrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		out = append(out, key+"="+overrides[key])
	}
	return out
}

func removeEnvKey(environ []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
