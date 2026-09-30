package citylayout

import (
	"fmt"
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

// SessionDiagnosticsRoot is the subdirectory, relative to a city's runtime
// root, holding per-session diagnostic artifacts: start-stderr.log,
// nudge-unconfirmed.log, and startup-nudge-unconfirmed.log.
const SessionDiagnosticsRoot = "sessions"

// SessionDiagnosticsDir returns the directory holding per-session diagnostic
// artifacts for a city, given the city root.
//
// Both the writers (internal/runtime/tmux) and the readers (gc doctor's
// nudge-unconfirmed check) MUST resolve this directory through this function
// or through SessionDiagnosticsDirForRuntimeDir. They previously each joined
// their own literal onto a different helper -- the writers onto
// RuntimePath(city) (.gc), the doctor onto RuntimeDataDir(city) (.gc/runtime)
// -- so every successfully written diagnostic was invisible to the check that
// exists to surface it, and the check reported a clean city. The live
// ds-research city had 6,959 session directories under .gc/sessions and no
// .gc/runtime/sessions at all. TestSessionDiagnosticsDirMatchesRuntimeDirForm
// and cmd/gc's TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics bind
// the two forms together so they cannot drift apart again.
func SessionDiagnosticsDir(cityRoot string) string {
	return RuntimePath(cityRoot, SessionDiagnosticsRoot)
}

// SessionDiagnosticsDirForRuntimeDir returns the same directory for a caller
// that holds a runtime dir rather than a city root (the tmux provider is
// configured with one). An empty runtimeDir means diagnostic capture is
// disabled and yields an empty path rather than a bare "sessions" relative
// path, which would land in the process working directory.
func SessionDiagnosticsDirForRuntimeDir(runtimeDir string) string {
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, SessionDiagnosticsRoot)
}

// ACPTranscriptsRoot is the city-relative root holding ACP JSON-RPC capture
// transcripts, one directory per GC_SESSION_ID and one file per
// GC_CONTINUATION_EPOCH. It is city-rooted rather than under the provider
// state directory because that directory lives in the ephemeral supervisor
// runtime dir, and transcripts must outlive a supervisor restart.
const ACPTranscriptsRoot = ".gc/transcripts/acp"

// acpTranscriptExt is the file extension of an ACP capture transcript.
const acpTranscriptExt = ".jsonl"

// ACPTranscriptsDir returns the directory holding ACP capture transcripts for
// a city.
//
// The capture writer (internal/runtime/acp, configured with this directory)
// and every transcript reader MUST resolve paths through ACPTranscriptPath or
// ACPTranscriptPathForDir so writer and reader cannot drift apart (see
// SessionDiagnosticsDir for the failure this prevents).
func ACPTranscriptsDir(cityRoot string) string {
	return RuntimePath(cityRoot, "transcripts", "acp")
}

// ACPTranscriptPath returns the capture transcript path for one session
// continuation epoch: <city>/.gc/transcripts/acp/<sessionID>/<epoch>.jsonl.
// It rejects empty components, path separators, NUL bytes, and "." / "..",
// so a hostile or malformed identifier cannot escape the transcripts root.
func ACPTranscriptPath(cityRoot, sessionID, continuationEpoch string) (string, error) {
	return ACPTranscriptPathForDir(ACPTranscriptsDir(cityRoot), sessionID, continuationEpoch)
}

// ACPTranscriptPathForDir returns the same path as ACPTranscriptPath for a
// caller that holds the transcripts directory rather than the city root (the
// ACP provider is configured with the directory). An empty dir is an error
// rather than a path relative to the process working directory.
func ACPTranscriptPathForDir(transcriptsDir, sessionID, continuationEpoch string) (string, error) {
	if transcriptsDir == "" {
		return "", fmt.Errorf("acp transcript path: empty transcripts directory")
	}
	if err := validateACPTranscriptComponent("session id", sessionID); err != nil {
		return "", err
	}
	if err := validateACPTranscriptComponent("continuation epoch", continuationEpoch); err != nil {
		return "", err
	}
	return filepath.Join(transcriptsDir, sessionID, continuationEpoch+acpTranscriptExt), nil
}

func validateACPTranscriptComponent(label, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("acp transcript path: empty %s", label)
	case value == "." || value == "..":
		return fmt.Errorf("acp transcript path: %s %q is a relative path element", label, value)
	case strings.ContainsAny(value, "/\\\x00"):
		return fmt.Errorf("acp transcript path: %s %q contains a path separator or NUL", label, value)
	}
	return nil
}

// SessionNameLocksDir returns the canonical root for explicit session-name locks.
func SessionNameLocksDir(cityRoot string) string {
	return RuntimePath(cityRoot, "session-name-locks")
}

// GateSlotsDir returns the canonical root for the pre-push gate's
// cross-invocation concurrency slots (ga-owh20p). The bash mechanism in
// scripts/push-gate-lock-lib.sh owns the actual flock(1) semaphore; this
// constant exists so Go tooling can locate the same directory without
// duplicating the ".gc/gate-slots" literal.
func GateSlotsDir(cityRoot string) string {
	return RuntimePath(cityRoot, "gate-slots")
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

// SuspensionStateFile returns the path to the unified runtime
// suspension-state file. It holds the live city, rig, and (in a
// follow-up) agent suspension preferences that should not be
// committed to city.toml — each clone gets its own copy.
func SuspensionStateFile(cityRoot string) string {
	return RuntimePath(cityRoot, "runtime", "suspension-state.json")
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

// CityIdentityEnvMap returns the city identity anchors without dispatcher
// trace defaults. Empty city roots return nil so callers do not inject empty
// GC_CITY values or relative runtime paths on unanchored code paths.
func CityIdentityEnvMap(cityRoot string) map[string]string {
	cityRoot = strings.TrimSpace(cityRoot)
	if cityRoot == "" {
		return nil
	}
	full := CityRuntimeEnvMapForRuntimeDir(cityRoot, TrustedAmbientCityRuntimeDir(cityRoot))
	return map[string]string{
		"GC_CITY":             full["GC_CITY"],
		"GC_CITY_PATH":        full["GC_CITY_PATH"],
		"GC_CITY_RUNTIME_DIR": full["GC_CITY_RUNTIME_DIR"],
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
