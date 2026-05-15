package citylayout

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

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

// ControlDispatcherTraceDefaultPath returns the default control-dispatcher
// workflow trace file under the canonical runtime root.
func ControlDispatcherTraceDefaultPath(cityRoot string) string {
	return filepath.Join(RuntimeDataDir(cityRoot), "control-dispatcher-trace.log")
}

// ControlDispatcherTraceDefaultPathForRuntimeDir returns the default
// control-dispatcher workflow trace file for the provided runtime root. Runtime
// dirs inside the city but outside .gc/runtime are coerced back to the
// canonical hidden runtime root to avoid watcher-visible trace writes. Runtime
// dirs outside the city are preserved as explicit operator overrides.
func ControlDispatcherTraceDefaultPathForRuntimeDir(cityRoot, runtimeDir string) string {
	runtimeDir = normalizeRuntimeDir(cityRoot, runtimeDir)
	return filepath.Join(runtimeDir, "control-dispatcher-trace.log")
}

// ControlDispatcherTraceLogFileName returns the per-dispatcher trace log
// filename for a qualified agent name. The "/" → "--" mapping mirrors how
// rig-qualified agent names are encoded in tmux session aliases (e.g., the
// "app/control-dispatcher" dispatcher runs in the "app--control-dispatcher"
// tmux session). Closes #1650 when used as a per-dispatcher TRACE_DEFAULT.
func ControlDispatcherTraceLogFileName(qualifiedName string) string {
	safe := strings.ReplaceAll(qualifiedName, "/", "--")
	return safe + "-trace.log"
}

// ControlDispatcherTraceDefaultPathFor returns the per-dispatcher default
// trace file under cityRoot's canonical runtime root. Each dispatcher
// resolves to a distinct file so operators can attribute every trace line
// without correlating against process titles or session lists.
func ControlDispatcherTraceDefaultPathFor(cityRoot, qualifiedName string) string {
	return filepath.Join(RuntimeDataDir(cityRoot), ControlDispatcherTraceLogFileName(qualifiedName))
}

// ControlDispatcherTraceDefaultPathForRuntimeDirAndName returns the
// per-dispatcher default trace file for the provided runtime root. Same
// runtime-dir normalization rules as ControlDispatcherTraceDefaultPathForRuntimeDir.
func ControlDispatcherTraceDefaultPathForRuntimeDirAndName(cityRoot, runtimeDir, qualifiedName string) string {
	runtimeDir = normalizeRuntimeDir(cityRoot, runtimeDir)
	return filepath.Join(runtimeDir, ControlDispatcherTraceLogFileName(qualifiedName))
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

// CityRuntimeEnv returns city runtime env vars rooted at the canonical runtime
// directory for cityRoot.
func CityRuntimeEnv(cityRoot string) []string {
	return CityRuntimeEnvForRuntimeDir(cityRoot, "")
}

// CityRuntimeEnvForRuntimeDir returns city runtime env vars for cityRoot using
// runtimeDir when it is a trusted override.
func CityRuntimeEnvForRuntimeDir(cityRoot, runtimeDir string) []string {
	runtimeDir = strings.TrimSpace(runtimeDir)
	if runtimeDir == "" {
		runtimeDir = RuntimeDataDir(cityRoot)
	}
	return []string{
		"GC_CITY=" + cityRoot,
		"GC_CITY_PATH=" + cityRoot,
		"GC_CITY_RUNTIME_DIR=" + runtimeDir,
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT=" + ControlDispatcherTraceDefaultPathForRuntimeDir(cityRoot, runtimeDir),
	}
}

// CityRuntimeEnvMap returns city runtime env vars rooted at the canonical
// runtime directory for cityRoot.
func CityRuntimeEnvMap(cityRoot string) map[string]string {
	return CityRuntimeEnvMapForRuntimeDir(cityRoot, "")
}

// CityRuntimeEnvMapForRuntimeDir returns city runtime env vars for cityRoot
// using runtimeDir when it is a trusted override.
func CityRuntimeEnvMapForRuntimeDir(cityRoot, runtimeDir string) map[string]string {
	runtimeDir = strings.TrimSpace(runtimeDir)
	if runtimeDir == "" {
		runtimeDir = RuntimeDataDir(cityRoot)
	}
	return map[string]string{
		"GC_CITY":                             cityRoot,
		"GC_CITY_PATH":                        cityRoot,
		"GC_CITY_RUNTIME_DIR":                 runtimeDir,
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": ControlDispatcherTraceDefaultPathForRuntimeDir(cityRoot, runtimeDir),
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

// TrustedAmbientCityRuntimeDir returns GC_CITY_RUNTIME_DIR only when the
// ambient process env is already anchored to cityRoot via GC_CITY,
// GC_CITY_PATH, or GC_CITY_ROOT. Paths outside the city tree are preserved
// intentionally: they cannot wake the city watcher and let operators relocate
// runtime artifacts explicitly.
func TrustedAmbientCityRuntimeDir(cityRoot string) string {
	runtimeDir := strings.TrimSpace(os.Getenv("GC_CITY_RUNTIME_DIR"))
	if runtimeDir == "" {
		return ""
	}
	for _, key := range []string{"GC_CITY_PATH", "GC_CITY", "GC_CITY_ROOT"} {
		if pathutil.SamePath(strings.TrimSpace(os.Getenv(key)), cityRoot) {
			return pathutil.NormalizePathForCompare(runtimeDir)
		}
	}
	return ""
}

func normalizeRuntimeDir(cityRoot, runtimeDir string) string {
	canonicalRuntimeDir := RuntimeDataDir(cityRoot)
	hiddenRoot := filepath.Join(cityRoot, ".gc")
	if pathutil.PathWithin(cityRoot, runtimeDir) && !pathutil.PathWithin(hiddenRoot, runtimeDir) {
		runtimeDir = canonicalRuntimeDir
	}
	return runtimeDir
}

// PublicServiceMountPath returns the supervisor-routable public path for a
// workspace service: /v0/city/<cityName>/svc/<serviceName>. This is the
// path the supervisor's public listener actually mounts;
// internal/api/supervisor.go strips the /v0/city/<cityName> prefix before
// forwarding the remaining /svc/... segment to the per-city router.
//
// Use this when composing a URL that an external service or out-of-process
// adapter will hit inbound (e.g. as a registered CallbackURL). For paths
// inside the per-city router (where the /v0/city/<name> prefix is already
// stripped), use the per-city-relative form returned by
// config.Service.MountPathOrDefault instead.
func PublicServiceMountPath(cityName, serviceName string) string {
	return "/v0/city/" + cityName + "/svc/" + serviceName
}
