package citylayout

import "path/filepath"

const (
	// RuntimeDataRoot is the canonical hidden runtime root for mutable city state.
	RuntimeDataRoot = ".gc/runtime"
	// RuntimePacksRoot is the canonical runtime root for pack-owned state.
	RuntimePacksRoot = ".gc/runtime/packs"
	// RuntimeServicesRoot is the canonical root for workspace-owned services.
	RuntimeServicesRoot = ".gc/services"
	// RuntimePublishedServicesRoot is the canonical root for published-service metadata.
	RuntimePublishedServicesRoot = ".gc/services/.published"
)

// RuntimeDataDir returns the canonical hidden runtime directory for a city.
func RuntimeDataDir(cityRoot string) string {
	return RuntimePath(cityRoot, "runtime")
}

// RuntimePacksDir returns the canonical root for pack-owned runtime state.
func RuntimePacksDir(cityRoot string) string {
	return RuntimePath(cityRoot, "runtime", "packs")
}

// RuntimeServicesDir returns the canonical root for workspace-owned services.
func RuntimeServicesDir(cityRoot string) string {
	return RuntimePath(cityRoot, "services")
}

// PublishedServicesDir returns the canonical root for published-service metadata.
func PublishedServicesDir(cityRoot string) string {
	return RuntimePath(cityRoot, "services", ".published")
}

// SessionNameLocksDir returns the canonical root for explicit session-name locks.
func SessionNameLocksDir(cityRoot string) string {
	return RuntimePath(cityRoot, "session-name-locks")
}

// ServiceStateDir returns the canonical runtime directory for a named service.
func ServiceStateDir(cityRoot, serviceName string) string {
	if serviceName == "" {
		return RuntimeServicesDir(cityRoot)
	}
	return filepath.Join(RuntimeServicesDir(cityRoot), serviceName)
}

// PackStateDir returns the canonical runtime directory for a named pack.
func PackStateDir(cityRoot, packName string) string {
	if packName == "" {
		return RuntimePacksDir(cityRoot)
	}
	return filepath.Join(RuntimePacksDir(cityRoot), packName)
}

// CityRuntimeEnv returns canonical city runtime env vars.
func CityRuntimeEnv(cityRoot string) []string {
	runtimeDir := RuntimeDataDir(cityRoot)
	return []string{
		"GC_CITY=" + cityRoot,
		"GC_CITY_PATH=" + cityRoot,
		"GC_CITY_RUNTIME_DIR=" + runtimeDir,
	}
}

// CityRuntimeEnvMap returns canonical city runtime env vars.
func CityRuntimeEnvMap(cityRoot string) map[string]string {
	runtimeDir := RuntimeDataDir(cityRoot)
	return map[string]string{
		"GC_CITY":             cityRoot,
		"GC_CITY_PATH":        cityRoot,
		"GC_CITY_RUNTIME_DIR": runtimeDir,
	}
}

// PackRuntimeEnv returns city runtime env vars plus the canonical pack state dir.
func PackRuntimeEnv(cityRoot, packName string) []string {
	env := CityRuntimeEnv(cityRoot)
	if packName != "" {
		env = append(env, "GC_PACK_STATE_DIR="+PackStateDir(cityRoot, packName))
	}
	return env
}

// PackRuntimeEnvMap returns city runtime env vars plus the canonical pack state dir.
func PackRuntimeEnvMap(cityRoot, packName string) map[string]string {
	env := CityRuntimeEnvMap(cityRoot)
	if packName != "" {
		env["GC_PACK_STATE_DIR"] = PackStateDir(cityRoot, packName)
	}
	return env
}
