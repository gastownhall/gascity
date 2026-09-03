package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
)

const defaultManagedDoltHost = "127.0.0.1"

// bdCommandRunnerForCity centralizes bd subprocess env construction so all
// GC-managed bd calls resolve Dolt against the same city-scoped runtime.
// Env is rebuilt on each call so GC_DOLT_PORT reflects the current managed
// dolt port (which can change across city restarts).
func bdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	completeBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
	if err != nil {
		return func(_, _ string, _ ...string) ([]byte, error) { return nil, err }
	}
	if completeBinding {
		return bdContextCommandRunnerForCity(cityPath)
	}
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		env, err := bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		return env, err
	})
}

// bdContextCommandRunnerForCity delegates complete external bindings to the
// workspace-pinned bd without projecting or recovering a managed backend.
func bdContextCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env := cityRuntimeEnvMapForCity(cityPath)
		bdBin, err := workspacePinnedBdBinary(cityPath)
		if err != nil {
			return nil, err
		}
		env["BD_BIN"] = bdBin
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		env["GC_RIG"] = ""
		env["GC_RIG_ROOT"] = ""
		env["BEADS_DOLT_AUTO_START"] = "0"
		env["BD_EXPORT_AUTO"] = "false"
		hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
		if err != nil {
			return nil, err
		}
		credentialsFile := strings.TrimSpace(env["BEADS_CREDENTIALS_FILE"])
		if credentialsFile == "" && !hosted {
			credentialsFile = strings.TrimSpace(ambientNativeDoltOpenEnv("BEADS_CREDENTIALS_FILE"))
		}
		setExecProjectedBackendEnvEmpty(env)
		if credentialsFile != "" {
			env["BEADS_CREDENTIALS_FILE"] = credentialsFile
		}
		if err := applyHostedBeadsCredentialEnv(env, cityPath); err != nil {
			return nil, err
		}
		runner, err := beadsCommandRunnerForHostedCity(cityPath, env)
		if err != nil {
			return nil, err
		}
		return runner(dir, name, args...)
	}
}

// workspacePinnedBdBinary resolves bd only from an explicitly configured
// workspace PATH. An unconfigured workspace retains the ambient executable
// lookup performed by the caller.
func workspacePinnedBdBinary(cityPath string) (string, error) {
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return "", err
	}
	_, configured := cfg.Workspace.Env["PATH"]
	if !configured {
		return "", nil
	}
	for _, dir := range filepath.SplitList(expandEnvMap(cfg.Workspace.Env)["PATH"]) {
		dir = strings.TrimSpace(dir)
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate, err := exec.LookPath(filepath.Join(dir, "bd"))
		if err == nil && filepath.IsAbs(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("workspace.env PATH is configured but contains no executable bd at an absolute path")
}

// errBdNotOnPath reports that neither the workspace pin nor the ambient
// lookup produced a bd executable. Callers phrase their own remediation.
var errBdNotOnPath = errors.New("bd not found in PATH")

// resolveBdBinaryForScope resolves the bd executable a scope's commands run.
// A scope bound to a complete storage binding runs the binary its workspace
// PATH pins, because only that build speaks the bound backend; every other
// scope keeps the ambient lookup. An ambient miss is errBdNotOnPath so
// callers can phrase their own remediation; a pin that is configured but
// unresolvable for a scope that needs it is returned verbatim rather than
// masked as a missing binary.
func resolveBdBinaryForScope(cityPath, scopeRoot string) (string, error) {
	bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot)
	if err != nil {
		return "", err
	}
	if bound {
		pinned, err := workspacePinnedBdBinary(cityPath)
		if err != nil {
			return "", err
		}
		if pinned = strings.TrimSpace(pinned); filepath.IsAbs(pinned) {
			return pinned, nil
		}
	}
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return "", errBdNotOnPath
	}
	return bdPath, nil
}

// scopeStoreIsExternallyBound reports whether a scope's bead store is served by
// a storage binding gc does not manage: the scope carries a complete binding of
// its own, or it inherits the city's the way applyCanonicalScopeBackendEnv
// does. A scope that overrides the city backend never reads the city binding,
// so a fault in it is not that scope's fault and answers false rather than
// taking the scope offline. Only the scope's own binding surfaces an error.
//
// This is the single predicate for "gc does not own this store": which bd
// binary to run, whether to project a Dolt environment, whether to manage or
// recover a Dolt runtime, and whether the scope needs a local Dolt identity are
// all the same question asked from different places. A site that answers it
// some other way is how the store gc does not serve acquires a Dolt server.
func scopeStoreIsExternallyBound(cityPath, scopeRoot string) (bool, error) {
	completeBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(scopeRoot))
	if err != nil || completeBinding {
		return completeBinding, err
	}
	if samePath(cityPath, scopeRoot) || scopeOverridesCityBackend(cityPath, scopeRoot) {
		return false, nil
	}
	inherited, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
	return err == nil && inherited, nil
}

// scopeStoreIsExternallyBoundBestEffort answers scopeStoreIsExternallyBound for
// callers that have no way to report a read failure. A scope whose binding
// cannot be read is treated as gc's own, which is the conservative answer: the
// paths that would then run all fail loudly on their own metadata read.
func scopeStoreIsExternallyBoundBestEffort(cityPath, scopeRoot string) bool {
	bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot)
	return err == nil && bound
}

func bdStoreForCity(dir, cityPath string) *beads.BdStore {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	reapStaleBdExportJSONL(dir)
	return beads.NewBdStoreWithPrefix(
		dir,
		bdCommandRunnerForCity(cityPath),
		issuePrefixForScope(dir, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
}

// bdStoreForRig opens a bead store at rigDir using rig-level Dolt config
// when available, falling back to city-level config. Use this when the rig
// may have its own Dolt server (e.g., shared from another city).
func bdStoreForRig(rigDir, cityPath string, cfg *config.City, knownPrefix ...string) *beads.BdStore {
	prefix := issuePrefixForScope(rigDir, cityPath, cfg)
	if prefix == "" {
		for _, candidate := range knownPrefix {
			if strings.TrimSpace(candidate) != "" {
				prefix = candidate
				break
			}
		}
	}
	reapStaleBdExportJSONL(rigDir)
	return beads.NewBdStoreWithPrefix(
		rigDir,
		bdCommandRunnerForRig(cityPath, cfg, rigDir),
		prefix,
		bdStoreOptionsForConfig(cfg)...,
	)
}

func bdStoreOptionsForConfig(cfg *config.City) []beads.BdStoreOption {
	var opts []beads.BdStoreOption
	if cfg != nil && cfg.Beads.UsesBD105CLISemantics() {
		opts = append(opts, beads.WithBdStoreListSkipLabels(true))
	}
	// Every bd-backed store this binary opens is a work ledger, so the classes
	// a split city serves elsewhere are exactly the classes its SQL reads must
	// refuse. Nil on a city that relocates nothing, which leaves the option
	// list byte-identical to what it was.
	if relocated := relocatedBeadClasses(cfg); len(relocated) > 0 {
		opts = append(opts, beads.WithBdStoreRelocatedClasses(relocated...))
	}
	return opts
}

// reapStaleBdExportJSONL removes .beads/issues.jsonl best-effort when the
// scope is gc-managed. The file is a stale export from when bd's auto-export
// was on (the upstream default); keeping it on disk causes bd 1.x to detect
// a "fresh clone" / "empty database" on the next write and stall bd create /
// gc mail send for the full 2m subprocess timeout while it re-imports the
// JSONL (sa-41j3kp).
//
// Cleanup conditions (any of which proves the scope is gc-managed and the
// JSONL is therefore stale):
//
//   - config.yaml explicitly sets export.auto:false (PR 1965 canonical state)
//   - config.yaml's gc.endpoint_origin is one of the managed origins
//
// Best-effort: any error is ignored because the env-var BD_EXPORT_AUTO=false
// in bdRuntimeEnv is a second line of defense, and a concurrent reader of the
// file (e.g., a bd-aware viewer) shouldn't fail the caller's operation. Reads
// use os.Stat/os.Remove (not fsys.OSFS) so the helper stays callable from
// store constructors that don't carry an fs seam.
func reapStaleBdExportJSONL(scopeRoot string) {
	scopeRoot = strings.TrimSpace(scopeRoot)
	if scopeRoot == "" {
		return
	}
	jsonlPath := filepath.Join(scopeRoot, ".beads", "issues.jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		// Fast path: no file → nothing to do. This is the steady state
		// once the cleanup has run once, so the rest of the helper is
		// only reached during the one-shot transition.
		return
	}
	if !scopeIsGCManaged(scopeRoot) {
		// Unmanaged scope: leave the file alone. Removing it under those
		// conditions could race with a legitimate auto-exporter (e.g., a
		// rig that opted out of managed canonicalization).
		return
	}
	_ = os.Remove(jsonlPath)
}

// scopeIsGCManaged reports whether a scope's .beads/config.yaml proves the
// scope is gc-managed under the canonical (non-explicit) shape. Either of
// two signals counts as proof:
//   - export.auto is explicitly false (PR 1965 wrote it; the user did not
//     opt back into auto-export afterward)
//   - gc.endpoint_origin is one of the canonical managed origins (the scope
//     was initialized by gc, even if the export.auto key has not yet been
//     normalized into the config on disk)
//
// Either signal alone is sufficient: the first covers post-normalization
// state, the second covers the transitional state where samtown-style
// long-lived cities still have a pre-PR-1965 config but were always
// gc-managed.
//
// EndpointOriginExplicit is intentionally excluded: per PR 1965, that is
// the deliberate opt-out path for rigs that want to keep JSONL-based
// sharing, so issues.jsonl there is load-bearing, not stale. The
// endpoint-origin check runs first so that an opt-out rig that *also*
// has export.auto:false (e.g. left over from a prior canonicalization,
// or hand-set) is still treated as unmanaged and never reaped.
func scopeIsGCManaged(scopeRoot string) bool {
	configPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	state, stateOK, stateErr := contract.ReadConfigState(fsys.OSFS{}, configPath)
	if stateErr == nil && stateOK {
		switch state.EndpointOrigin {
		case contract.EndpointOriginExplicit:
			// Deliberate opt-out — JSONL is load-bearing, leave alone
			// regardless of any other signal in the config.
			return false
		case contract.EndpointOriginManagedCity,
			contract.EndpointOriginCityCanonical,
			contract.EndpointOriginInheritedCity:
			return true
		}
	}
	if autoExport, ok, err := contract.ReadExportAuto(fsys.OSFS{}, configPath); err == nil && ok && !autoExport {
		return true
	}
	return false
}

func controlBdStoreForCity(dir, cityPath string, cfg *config.City) *beads.BdStore {
	reapStaleBdExportJSONL(dir)
	return beads.NewBdStoreWithPrefix(
		dir,
		controlBdCommandRunnerForCity(cityPath),
		issuePrefixForScope(dir, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
}

func controlBdStoreForRig(rigDir, cityPath string, cfg *config.City, knownPrefix ...string) *beads.BdStore {
	prefix := issuePrefixForScope(rigDir, cityPath, cfg)
	if prefix == "" {
		for _, candidate := range knownPrefix {
			if strings.TrimSpace(candidate) != "" {
				prefix = candidate
				break
			}
		}
	}
	reapStaleBdExportJSONL(rigDir)
	return beads.NewBdStoreWithPrefix(
		rigDir,
		controlBdCommandRunnerForRig(cityPath, cfg, rigDir),
		prefix,
		bdStoreOptionsForConfig(cfg)...,
	)
}

func controlBdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		env, err := bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		applyControllerBdEnv(env)
		return env, err
	})
}

func controlBdCommandRunnerForRig(cityPath string, cfg *config.City, rigDir string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(_ string) (map[string]string, error) {
		env, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
		applyControllerBdEnv(env)
		return env, err
	})
}

func applyExportSuppressionEnv(env map[string]string) {
	env["BD_EXPORT_AUTO"] = "false"
}

func applyControllerBdEnv(env map[string]string) {
	applyExportSuppressionEnv(env)
	if strings.TrimSpace(os.Getenv("BEADS_ACTOR")) == "" {
		env["BEADS_ACTOR"] = "controller"
	}
}

func issuePrefixForScope(scopeRoot, cityPath string, cfg *config.City) string {
	if prefix := readScopeIssuePrefix(scopeRoot); prefix != "" {
		return prefix
	}
	if cfg == nil {
		return ""
	}
	scopeRoot = filepath.Clean(scopeRoot)
	if filepath.Clean(cityPath) == scopeRoot {
		return config.EffectiveHQPrefix(cfg)
	}
	for i := range cfg.Rigs {
		rigPath := resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path)
		if filepath.Clean(rigPath) == scopeRoot {
			return cfg.Rigs[i].EffectivePrefix()
		}
	}
	return ""
}

func readScopeIssuePrefix(scopeRoot string) string {
	prefix, ok, err := contract.ReadIssuePrefix(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ""
	}
	return prefix
}

func bdCommandRunnerForRig(cityPath string, cfg *config.City, rigDir string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(_ string) (map[string]string, error) {
		return bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	})
}

func canonicalScopeDoltTarget(cityPath, scopeRoot string) (contract.DoltConnectionTarget, bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return contract.DoltConnectionTarget{}, false, err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return contract.DoltConnectionTarget{}, false, nil
	}
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return contract.DoltConnectionTarget{}, true, err
	}
	return target, true, nil
}

// canonicalScopeDoltProjectionAuthoritative reports whether canonical
// Dolt projection would resolve auth for the city scope: the city's store
// is gc's to project, and the scope config resolves authoritative — the
// same ResolveScopeConfigState gate applyOrderExecCanonicalDoltEnv and its
// managed fallback apply before calling applyCanonicalDoltAuthEnv.
// Callers that feed ambient environments into the projection use this
// to strip untrusted password mirrors from the resolution input without
// breaking the strict no-op pass-through for non-authoritative scopes.
// A city served by a storage binding gets no projection at all, so
// stripping its operator-set password mirrors would remove auth nothing
// downstream restores.
func canonicalScopeDoltProjectionAuthoritative(cityPath string) bool {
	if scopeStoreIsExternallyBoundBestEffort(cityPath, cityPath) {
		return false
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		return false
	}
	return resolved.Kind == contract.ScopeConfigAuthoritative
}

func applyCanonicalDoltTargetEnv(env map[string]string, target contract.DoltConnectionTarget) {
	if env == nil {
		return
	}
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		delete(env, "GC_DOLT_HOST")
		delete(env, "GC_DOLT_PORT")
		delete(env, "BEADS_DOLT_SERVER_HOST")
		delete(env, "BEADS_DOLT_SERVER_PORT")
		env["BEADS_DOLT_SERVER_SOCKET"] = socket
		return
	}
	delete(env, "BEADS_DOLT_SERVER_SOCKET")
	// GC-owned projections must use the resolved target, not ambient parent
	// shell host/port. Stale GC_DOLT_HOST/PORT was causing gc bd and projected
	// session flows to drift away from the canonical external endpoint.
	if shouldProjectResolvedDoltHost(target) {
		env["GC_DOLT_HOST"] = strings.TrimSpace(target.Host)
	} else {
		delete(env, "GC_DOLT_HOST")
	}
	if strings.TrimSpace(target.Port) != "" {
		env["GC_DOLT_PORT"] = target.Port
	} else {
		delete(env, "GC_DOLT_PORT")
	}
}

func shouldProjectResolvedDoltHost(target contract.DoltConnectionTarget) bool {
	host := strings.TrimSpace(target.Host)
	if host == "" {
		return false
	}
	if target.External {
		return true
	}
	return !managedLocalDoltHost(host)
}

func applyCanonicalDoltAuthEnv(env map[string]string, cityPath, scopeRoot string, target contract.DoltConnectionTarget) {
	if env == nil {
		return
	}
	authScopeRoot := doltauth.AuthScopeRoot(cityPath, scopeRoot, target)
	if !samePath(authScopeRoot, cityPath) {
		clearProjectedDoltPasswordEnv(env)
	}
	applyResolvedDoltAuthEnv(env, authScopeRoot, strings.TrimSpace(target.User))
}

// applyCompleteNonDoltStorageBindingEnv is the whole of gc's support for a
// backend it does not implement, and the reason no other support is needed.
//
// A scope carrying a complete storage binding is served by the linked beads
// library reading the workspace's own configuration. So gc withholds the
// entire projected backend namespace rather than a list of keys — the set of
// variables that library reads is the library's to grow — sets only the bd
// binary to run, and preserves a credentials file the operator pointed at. The
// library's credential ladder does the rest. Nothing here branches on which
// backend the binding names, which is why no name has to be registered, no
// connection shape parsed, and no credential resolved on this side.
func applyCompleteNonDoltStorageBindingEnv(env map[string]string, cityPath, scopeRoot string) (bool, error) {
	completeBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(scopeRoot))
	if err != nil || !completeBinding {
		return completeBinding, err
	}
	hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return true, err
	}
	credentialsFile := strings.TrimSpace(env["BEADS_CREDENTIALS_FILE"])
	if credentialsFile == "" && !hosted {
		credentialsFile = strings.TrimSpace(ambientNativeDoltOpenEnv("BEADS_CREDENTIALS_FILE"))
	}
	setExecProjectedBackendEnvEmpty(env)
	if credentialsFile != "" {
		env["BEADS_CREDENTIALS_FILE"] = credentialsFile
	}
	if err := applyHostedBeadsCredentialEnv(env, cityPath); err != nil {
		return true, err
	}
	bdBin, err := workspacePinnedBdBinary(cityPath)
	if err != nil {
		return true, err
	}
	env["BD_BIN"] = bdBin
	return true, nil
}

// applyHostedBeadsCredentialEnv selects the credential command for bd
// subprocesses. Existing legacy helpers always win. Otherwise, and only for
// the exact hosted beads-workspace storage selector, gc installs its fixed
// bridge to the credential-provider protocol.
func applyHostedBeadsCredentialEnv(env map[string]string, cityPath string) error {
	if env == nil {
		return nil
	}
	selected, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return err
	}
	if selected {
		// Exact hosted bindings must not inherit any part of the BEADS_*
		// namespace. Explicit compatibility values below are projected into the
		// map before the command runner applies it; ambient BEADS_* values are
		// never treated as authorization for this binding.
		withholdAmbientHostedBeadsEnv(env)
	}
	if command := strings.TrimSpace(env["GC_DOLT_CRED_CMD"]); command != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = command
		return nil
	}
	if strings.TrimSpace(env["BEADS_DOLT_CREDENTIAL_COMMAND"]) != "" {
		return nil
	}
	if command := strings.TrimSpace(os.Getenv("GC_DOLT_CRED_CMD")); command != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = command
		return nil
	}
	// The ambient BEADS_* namespace is untrusted for the exact hosted
	// workspace selector. In particular, an inherited credential command must
	// not replace the bridge selected below. Non-hosted/legacy paths retain the
	// historical ambient fallback for compatibility.
	if !selected {
		if command := strings.TrimSpace(os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")); command != "" {
			env["BEADS_DOLT_CREDENTIAL_COMMAND"] = command
			return nil
		}
		return nil
	}
	command, err := hostedBeadsCredentialCommand()
	if err != nil {
		return err
	}
	projectCredentialProviderEnv(env)
	env["BEADS_DOLT_CREDENTIAL_COMMAND"] = command
	return nil
}

// beadsCommandRunnerForHostedCity chooses the hermetic runner only for the
// exact hosted Beads workspace binding. Explicit values in env remain valid
// overrides; only the inherited BEADS_* namespace is withheld by the variant.
func beadsCommandRunnerForHostedCity(cityPath string, env map[string]string) (beads.CommandRunner, error) {
	selected, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return nil, err
	}
	if selected {
		return beadsExecCommandRunnerWithEnvWithoutAmbientBeads(env), nil
	}
	return beadsExecCommandRunnerWithEnv(env), nil
}

// withholdAmbientHostedBeadsEnv pins every ambient BEADS_* key absent from an
// explicit projection to an empty value. Runtime adapters interpret empty
// values as removal, so unknown variables cannot flow into an agent session.
func withholdAmbientHostedBeadsEnv(env map[string]string) {
	if env == nil {
		return
	}
	for _, entry := range processEnvSnapshotExcludingNativeDoltOpen() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, "BEADS_") {
			continue
		}
		if _, explicit := env[key]; !explicit {
			env[key] = ""
		}
	}
}

// projectCredentialProviderEnv carries the provider argv configuration into
// subprocess and session maps. LookupEnv is intentional: an explicitly empty
// override is invalid configuration and must reach the bridge so invocation
// fails closed instead of silently using the default provider.
func projectCredentialProviderEnv(env map[string]string) {
	if env == nil {
		return
	}
	if _, exists := env[registryCredentialProviderEnv]; exists {
		return
	}
	if raw, configured := os.LookupEnv(registryCredentialProviderEnv); configured {
		env[registryCredentialProviderEnv] = raw
	}
}

func citySelectsHostedBeadsCredentialProvider(cityPath string) (bool, error) {
	cityConfigPath := filepath.Join(cityPath, "city.toml")
	if _, err := os.Stat(cityConfigPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read hosted Beads credential configuration: %w", err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, cityConfigPath)
	if err != nil {
		return false, fmt.Errorf("load hosted Beads credential configuration: %w", err)
	}
	return configSelectsHostedBeadsCredentialProvider(cfg), nil
}

func configSelectsHostedBeadsCredentialProvider(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	storage := cfg.EffectiveStorage()
	shape, bindingName := storageSplitShapeOf(storage)
	if shape != storageSplitWhole {
		return false
	}
	binding, ok := storage.Bindings[bindingName]
	return ok &&
		binding.Provider == config.StorageProviderBeadsWorkspace &&
		strings.TrimSpace(binding.URL) != "" &&
		binding.Auth == config.StorageAuthCredentialProvider
}

// applyCanonicalScopeBackendEnv dispatches to the appropriate backend
// helper based on the scope's MetadataState.Backend.
//
// Returns (true, nil) when the scope is authoritative and the backend
// projection succeeded. Returns (false, nil) when the scope is
// non-authoritative — caller falls through to inherited-city. Returns
// (true, err) on a known backend that failed to project; caller MUST
// surface this error rather than retrying.
func applyCanonicalScopeBackendEnv(env map[string]string, cityPath, scopeRoot string) (bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return false, err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return false, nil
	}
	if completeBinding, err := applyCompleteNonDoltStorageBindingEnv(env, cityPath, scopeRoot); err != nil {
		return true, err
	} else if completeBinding {
		return true, nil
	}
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if metaErr != nil {
		return true, metaErr
	}
	// A mode marker is interpreted only in the namespace of the effective
	// backend. Metadata is scope-local authority; when it has no backend marker,
	// use the configured city backend (empty means the legacy Dolt backend).
	// This keeps stale dolt.mode values on DoltLite scopes inert even before
	// metadata is emitted.
	effectiveBackend := strings.TrimSpace(meta.Backend)
	if effectiveBackend == "" {
		effectiveBackend = strings.TrimSpace(beadsBackend(cityPath))
	}
	if resolved.State.EndpointOrigin == contract.EndpointOriginInheritedCity && meta.Backend == "" {
		if inherited, err := applyCityStorageBindingEnv(env, cityPath); err != nil {
			return true, err
		} else if inherited {
			return true, nil
		}
	}
	if resolved.State.EndpointOrigin == contract.EndpointOriginInheritedCity &&
		(meta.Backend == "" || meta.Backend == "doltlite") &&
		cityUsesDoltliteBeadsBackend(cityPath) {
		clearProjectedDoltEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	}
	// A mode marker is meaningful only for the Dolt backend.  In particular,
	// stale dolt_mode metadata on a doltlite scope must not switch the child to
	// Dolt's proxied environment.
	resolvedProxied := contract.IsProxiedDoltMode(effectiveBackend, resolved.State.DoltMode)
	metadataProxied := contract.IsProxiedDoltMode(effectiveBackend, meta.DoltMode)
	if resolvedProxied || metadataProxied {
		applyProxiedDoltEnv(env)
		return true, nil
	}
	switch strings.ToLower(strings.TrimSpace(effectiveBackend)) {
	case "", "dolt":
		clearProjectedBeadsBackendEnv(env)
		target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
		if err != nil {
			return true, err
		}
		applyCanonicalDoltTargetEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		mirrorBeadsDoltScopeEnv(env, target)
		return true, nil
	case "doltlite":
		clearProjectedDoltEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	default:
		return true, unprojectableBackendError(effectiveBackend, scopeRoot)
	}
}

// unprojectableBackendError refuses a backend this projector has no arm for.
//
// It reports the same four facts the metadata loader reports — the name, where
// it was found, what this build registers, and that nothing was opened —
// because an operator who meets one backend refusal must not have to learn a
// second vocabulary to read the next one. This is the last guard before a bd
// subprocess would inherit the ambient environment, so it never returns nil.
//
// A registered backend arriving here is a composition defect rather than bad
// metadata (a registrar added a name without an env-projection arm), and the
// message says so instead of telling the operator their metadata is wrong.
func unprojectableBackendError(backend, scopeRoot string) error {
	if err := contract.RecognizeBackend(backend); err != nil {
		return fmt.Errorf("%w (scope %s)", err, scopeRoot)
	}
	supported, err := contract.RegisteredBackends()
	if err != nil {
		return fmt.Errorf("backend %q for scope %s cannot be projected: %w; %s", backend, scopeRoot, err, contract.BackendNotOpenedGuarantee)
	}
	return fmt.Errorf("backend %q for scope %s is registered by this build but has no environment projection (registered: %s); %s",
		backend, scopeRoot, strings.Join(supported, ", "), contract.BackendNotOpenedGuarantee)
}

// applyCityStorageBindingEnv covers the two city-scope shapes the caller's
// Dolt projection does not: a complete storage binding, which withholds the
// whole projected namespace, and a backend this build cannot project, which is
// refused by name before any bd subprocess could inherit an ambient
// environment. Returns (false, nil) when the city names a backend the caller's
// Dolt path handles.
func applyCityStorageBindingEnv(env map[string]string, cityPath string) (bool, error) {
	if completeBinding, err := applyCompleteNonDoltStorageBindingEnv(env, cityPath, cityPath); err != nil {
		return true, err
	} else if completeBinding {
		return true, nil
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		return false, err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return false, nil
	}
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if metaErr != nil {
		return true, metaErr
	}
	switch meta.Backend {
	case "", "dolt", "doltlite":
		return false, nil
	default:
		return true, unprojectableBackendError(meta.Backend, cityPath)
	}
}

func scopeBackendIsDoltlite(cityPath, scopeRoot string) bool {
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err == nil && ok && meta.Backend != "" {
		return meta.Backend == "doltlite"
	}
	if samePath(cityPath, scopeRoot) {
		return cityUsesDoltliteBeadsBackend(cityPath)
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind != contract.ScopeConfigAuthoritative {
		return false
	}
	return resolved.State.EndpointOrigin == contract.EndpointOriginInheritedCity &&
		cityUsesDoltliteBeadsBackend(cityPath)
}

func scopeOverridesCityBackend(cityPath, scopeRoot string) bool {
	if samePath(cityPath, scopeRoot) {
		return false
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err == nil && ok && strings.TrimSpace(meta.Backend) != "" {
		return true
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind != contract.ScopeConfigAuthoritative {
		return false
	}
	return resolved.State.EndpointOrigin != contract.EndpointOriginInheritedCity
}

// scopeMetadataJSONPath returns the absolute path to a scope's
// .beads/metadata.json. Centralized so the dispatcher and the recovery
// hook helpers agree on the file location.
func scopeMetadataJSONPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "metadata.json")
}

func applyCanonicalConfigStateDoltEnv(env map[string]string, cityPath, scopeRoot string, state contract.ConfigState) {
	target := contract.DoltConnectionTarget{
		Host:           strings.TrimSpace(state.DoltHost),
		Port:           strings.TrimSpace(state.DoltPort),
		Socket:         strings.TrimSpace(state.DoltSocket),
		User:           strings.TrimSpace(state.DoltUser),
		EndpointOrigin: state.EndpointOrigin,
		EndpointStatus: state.EndpointStatus,
		External:       true,
	}
	applyCanonicalDoltTargetEnv(env, target)
	applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
	mirrorBeadsDoltScopeEnv(env, target)
}

func applyCanonicalScopeInitDoltEnv(env map[string]string, cityPath, scopeRoot string) error {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return err
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative {
		return nil
	}
	switch resolved.State.EndpointOrigin {
	case contract.EndpointOriginManagedCity:
		return nil
	case contract.EndpointOriginCityCanonical, contract.EndpointOriginExplicit:
		applyCanonicalConfigStateDoltEnv(env, cityPath, scopeRoot, resolved.State)
		return nil
	case contract.EndpointOriginInheritedCity:
		cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
		if err != nil {
			return err
		}
		if cityResolved.Kind == contract.ScopeConfigAuthoritative && cityResolved.State.EndpointOrigin == contract.EndpointOriginCityCanonical {
			applyCanonicalConfigStateDoltEnv(env, cityPath, scopeRoot, resolved.State)
		}
		return nil
	default:
		return nil
	}
}

var projectedDoltEnvKeys = []string{
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_SOCKET",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_PASSWORD",
	// BEADS_DOLT_SERVER_TLS is intentionally NOT a projected key: it is an
	// ambient hosted-gateway credential passthrough (see
	// hostedBeadsCredentialPassthroughKeys) with no per-scope source, so
	// mergeRuntimeEnv must not strip it. mirrorBeadsDoltEnv clears it (non-external
	// scopes) and mirrorBeadsDoltScopeEnv carries it (external endpoints) into the
	// native-open env instead.
}

var bdCLIRemoteSyncOptOutEnvKeys = [...]string{
	// BD_DOLT_SYNC_CLI_REMOTES is the key bd's BD-prefixed Viper env
	// binding consumes today; keep BEADS_DOLT_SYNC_CLI_REMOTES as a
	// compatibility alias only.
	"BD_DOLT_SYNC_CLI_REMOTES",
	"BEADS_DOLT_SYNC_CLI_REMOTES",
}

func appendBdCLIRemoteSyncOptOutEnvKeys(keys []string) []string {
	for _, key := range bdCLIRemoteSyncOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

// bdAutoBackupOptOutEnvKeys disables bd's PersistentPostRun auto-backup —
// the hardcoded "backup_export" Dolt remote bd syncs on (almost) every
// invocation. A stuck-looping backup_export sync was the root cause of the
// 2026-06-08 town-wide Dolt wedge (ga-0eq): it saturated the commit path
// while oscillating not-found/already-exists. gc never relies on this path
// (managed backups run through mol-dog-backup), so it is pure downside here.
// BD_BACKUP_ENABLED is the key bd's BD-prefixed Viper env binding consumes
// today; keep BEADS_BACKUP_ENABLED as a compatibility alias only.
var bdAutoBackupOptOutEnvKeys = [...]string{
	"BD_BACKUP_ENABLED",
	"BEADS_BACKUP_ENABLED",
}

func appendBdAutoBackupOptOutEnvKeys(keys []string) []string {
	for _, key := range bdAutoBackupOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

// bdContributorRoutingOptOutEnvKeys disables bd's fork/contributor
// auto-routing for gc-managed bd invocations. When a gcy-style store has
// routing.mode=auto and routing.contributor=~/.beads-planning persisted in
// its .beads config, upstream bd silently routes `create`/`list`/`update`
// to that out-of-band "planning" store while `show` (prefix-routed) and gc's
// in-process dispatch (sling/scale-check/hook pickup, which open the scope
// store directly via openCityStoreAt) keep reading the scope store. The
// result is a three-way split brain: a bead that `bd list --rig` shows is
// invisible to `bd show` and unresolvable by `gc sling`. gc owns scope→store
// resolution itself (BEADS_DIR + the rig registry), so contributor routing is
// pure downside here. Forcing routing.mode=off via the env override (which
// beadslib's getRoutingConfigValue / resolveRoutingConfigValue honor ahead of
// the persisted DB value) makes every gc-managed bd subcommand operate on the
// scope's own store — the same store sling and show already use.
//
// BD_ROUTING_MODE is the key bd's BD-prefixed Viper env binding consumes;
// BEADS_ROUTING_MODE is kept as a compatibility alias only.
var bdContributorRoutingOptOutEnvKeys = [...]string{
	"BD_ROUTING_MODE",
	"BEADS_ROUTING_MODE",
}

func appendBdContributorRoutingOptOutEnvKeys(keys []string) []string {
	for _, key := range bdContributorRoutingOptOutEnvKeys {
		keys = append(keys, key)
	}
	return keys
}

var (
	beadsExecCommandRunnerWithEnv                    = beads.ExecCommandRunnerWithEnv
	beadsExecCommandRunnerWithEnvWithoutAmbientBeads = beads.ExecCommandRunnerWithEnvWithoutAmbientBeads
	processEnvSnapshotExcludingNativeDoltOpen        = beads.ProcessEnvSnapshotExcludingNativeDoltOpen
	ambientNativeDoltOpenEnv                         = beads.AmbientNativeDoltOpenEnv
)

var recoverManagedBDCommand = func(cityPath string) error {
	script := gcBeadsBdScriptPath(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	setProjectedDoltEnvEmpty(overrides)
	applyBdCLIRemoteSyncOptOut(overrides)
	applyBdAutoBackupOptOut(overrides)
	applyBdContributorRoutingOptOut(overrides)
	environ := mergeRuntimeEnv(processEnvSnapshotExcludingNativeDoltOpen(), overrides)
	environ = append(environ, providerLifecycleDoltPathEnv(cityPath)...)
	if gcBin := resolveProviderLifecycleGCBinary(); gcBin != "" {
		environ = removeEnvKey(environ, "GC_BIN")
		environ = append(environ, "GC_BIN="+gcBin)
	}
	return runProviderOpWithEnv(script, environ, "recover")
}

func setProjectedDoltEnvEmpty(env map[string]string) {
	for _, key := range projectedDoltEnvKeys {
		env[key] = ""
	}
}

func ensureProjectedDoltEnvExplicit(env map[string]string) {
	for _, key := range projectedDoltEnvKeys {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
	}
}

func clearProjectedDoltEnv(env map[string]string) {
	delete(env, "BEADS_DOLT_PROXIED_SERVER")
	for _, key := range projectedDoltEnvKeys {
		delete(env, key)
	}
}

// clearManagedDoltLifecycleEnv removes Gas City's direct sql-server control
// plane from an environment. Proxied-server mode is owned entirely by beads;
// leaking these variables lets an RC bd child rediscover the obsolete direct
// lifecycle or connect to a stale server.
func clearManagedDoltLifecycleEnv(env map[string]string) {
	for _, key := range []string{
		"GC_PACK_STATE_DIR", "GC_DOLT_DATA_DIR", "GC_DOLT_LOG_FILE",
		"GC_DOLT_STATE_FILE", "GC_DOLT_PID_FILE", "GC_DOLT_LOCK_FILE",
		"GC_DOLT_CONFIG_FILE", "GC_DOLT_ARCHIVE_LEVEL", "GC_DOLT_AUTO_GC_ENABLED",
		"GC_DOLT_MAX_CONNECTIONS", "GC_DOLT_READ_TIMEOUT_MILLIS",
		"GC_DOLT_WRITE_TIMEOUT_MILLIS", "GC_DOLT_LOCK_RELEASE_TIMEOUT_MS",
		"GC_DOLT_WAIT_TIMEOUT", "GC_DOLT_CONCURRENT_START_READY_TIMEOUT_MS", "BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_SOCKET", "BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_MODE",
	} {
		delete(env, key)
	}
}

func applyProxiedDoltEnv(env map[string]string) {
	clearProjectedDoltEnv(env)
	clearManagedDoltLifecycleEnv(env)
	env["GC_BEADS_BACKEND"] = "dolt"
	env["BEADS_BACKEND"] = "dolt"
	env["BEADS_DOLT_PROXIED_SERVER"] = "1"
}

var projectedBeadsBackendEnvKeys = []string{
	"GC_BEADS_BACKEND",
	"BEADS_BACKEND",
}

func clearProjectedBeadsBackendEnv(env map[string]string) {
	for _, key := range projectedBeadsBackendEnvKeys {
		delete(env, key)
	}
}

func clearProjectedDoltPasswordEnv(env map[string]string) {
	delete(env, "GC_DOLT_PASSWORD")
	delete(env, "BEADS_DOLT_PASSWORD")
}

func managedLocalDoltHost(host string) bool {
	return contract.DoltHostIsLocal(host)
}

func externalDoltEnvOverrideTarget() (contract.DoltConnectionTarget, bool) {
	if socket := strings.TrimSpace(os.Getenv("BEADS_DOLT_SERVER_SOCKET")); socket != "" {
		if !filepath.IsAbs(socket) || strings.ContainsRune(socket, '\x00') || strings.TrimSpace(os.Getenv("GC_DOLT_HOST")) != "" || strings.TrimSpace(os.Getenv("GC_DOLT_PORT")) != "" {
			return contract.DoltConnectionTarget{}, false
		}
		return contract.DoltConnectionTarget{Socket: socket, External: true}, true
	}
	hostOverride := strings.TrimSpace(os.Getenv("GC_DOLT_HOST"))
	if hostOverride == "" || managedLocalDoltHost(hostOverride) {
		return contract.DoltConnectionTarget{}, false
	}
	// Tests and runbooks use the reserved .invalid TLD as a stale ambient
	// endpoint sentinel. Never promote that sentinel into a child bd process.
	if strings.HasSuffix(strings.Trim(hostOverride, "[]"), ".invalid") {
		return contract.DoltConnectionTarget{}, false
	}
	return contract.DoltConnectionTarget{
		Host:     hostOverride,
		Port:     strings.TrimSpace(os.Getenv("GC_DOLT_PORT")),
		External: true,
	}, true
}

// currentResolvableManagedDoltPort returns a live managed Dolt port from the
// published runtime state, or from provider state when publication has not
// caught up yet. Provider fallback uses validDoltRuntimeState instead of the
// contract package's lighter published-state validation because callers may
// mirror or publish this value into user-visible runtime files.
func currentResolvableManagedDoltPort(cityPath string) string {
	// Proxied-server mode has no Gas City-managed listener. Ignore stale
	// provider/runtime state so callers never publish or stop a direct server
	// for a scope whose proxy owns the child lifecycle.
	if scopeUsesProxiedDoltMode(cityPath, cityPath) {
		return ""
	}
	if port := currentManagedDoltPort(cityPath); port != "" {
		return port
	}
	state, ok := readValidProviderManagedDoltState(cityPath)
	if !ok {
		return ""
	}
	return strconv.Itoa(state.Port)
}

func readValidProviderManagedDoltState(cityPath string) (doltRuntimeState, bool) {
	state, err := readDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath))
	if err != nil {
		return doltRuntimeState{}, false
	}
	if !validDoltRuntimeState(state, cityPath) {
		return doltRuntimeState{}, false
	}
	return state, true
}

func currentPublishedOrRecoveredManagedDoltPort(cityPath string, allowRecovery bool) (string, error) {
	if scopeUsesProxiedDoltMode(cityPath, cityPath) {
		return "", nil
	}
	if port := currentManagedDoltPort(cityPath); port != "" {
		return port, nil
	}
	if !allowRecovery {
		return "", nil
	}
	state, ok := readValidProviderManagedDoltState(cityPath)
	if !ok {
		return "", nil
	}
	published, err := publishManagedDoltRuntimeStateIfOwnedResultFromState(cityPath, state)
	if err != nil {
		return "", fmt.Errorf("publish managed dolt runtime state from provider state: %w", err)
	}
	port := currentManagedDoltPort(cityPath)
	if port == "" {
		if !published {
			return "", fmt.Errorf("publish managed dolt runtime state from provider state: managed dolt lifecycle is not owned and published state is absent")
		}
		return "", fmt.Errorf("publish managed dolt runtime state from provider state: published state is not valid")
	}
	return port, nil
}

func resolvedRuntimeCityDoltTarget(cityPath string, allowRecovery bool) (contract.DoltConnectionTarget, bool, error) {
	return resolvedRuntimeCityDoltTargetContext(context.Background(), cityPath, allowRecovery)
}

func resolvedRuntimeCityDoltTargetContext(ctx context.Context, cityPath string, allowRecovery bool) (contract.DoltConnectionTarget, bool, error) {
	if err := ctx.Err(); err != nil {
		return contract.DoltConnectionTarget{}, false, err
	}
	var managedRuntimeErr error
	var recoveryErr error
	recoveryChecked := false
	recoveryPort := ""
	recoveredManagedDoltPort := func() string {
		if recoveryChecked {
			return recoveryPort
		}
		recoveryChecked = true
		port, err := currentPublishedOrRecoveredManagedDoltPort(cityPath, allowRecovery)
		if err != nil {
			recoveryErr = err
			return ""
		}
		recoveryPort = port
		return port
	}
	resetRecoveryCache := func() {
		recoveryChecked = false
		recoveryPort = ""
	}
	if target, ok, err := canonicalScopeDoltTarget(cityPath, cityPath); err != nil {
		if !allowRecovery || !contract.IsManagedRuntimeUnavailable(err) {
			return contract.DoltConnectionTarget{}, false, err
		}
		if port := recoveredManagedDoltPort(); port != "" {
			return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
		}
		managedRuntimeErr = err
	} else if ok {
		return target, true, nil
	}
	if host, port, ok, invalid := resolveConfiguredCityDoltTarget(cityPath); invalid {
		return contract.DoltConnectionTarget{}, false, fmt.Errorf("invalid canonical city endpoint state")
	} else if ok {
		return contract.DoltConnectionTarget{Host: host, Port: port, External: true}, true, nil
	}

	if target, ok := externalDoltEnvOverrideTarget(); ok {
		if target.Socket != "" {
			return target, true, nil
		}
		return target, true, nil
	}

	if port := recoveredManagedDoltPort(); port != "" {
		return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
	}
	if allowRecovery {
		if err := healthBeadsProviderContext(ctx, cityPath, false); err == nil {
			resetRecoveryCache()
			if port := recoveredManagedDoltPort(); port != "" {
				return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
			}
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return contract.DoltConnectionTarget{}, false, ctxErr
		}
	}
	// Last-resort: when all other recovery paths have been exhausted but the
	// managed Dolt lifecycle is owned, attempt to read the port directly from
	// provider state using the symlink-aware validation path. This handles the
	// case where currentPublishedOrRecoveredManagedDoltPort encounters a publish
	// failure (e.g., write permission error, post-publish re-validation failure)
	// while the server is still accessible.
	if allowRecovery {
		if owned, _ := managedDoltLifecycleOwned(cityPath); owned {
			if port := currentResolvableManagedDoltPort(cityPath); port != "" {
				return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
			}
		}
	}
	if recoveryErr != nil {
		return contract.DoltConnectionTarget{}, false, recoveryErr
	}
	if managedRuntimeErr != nil {
		return contract.DoltConnectionTarget{}, false, managedRuntimeErr
	}
	return contract.DoltConnectionTarget{}, false, nil
}

func managedLocalDoltEnv(env map[string]string) bool {
	return managedLocalDoltHost(env["GC_DOLT_HOST"])
}

// managedBDRecoveryAllowed is the one place that answers "may gc recover this
// scope's store?" — both the retry and the recovery classifier ask it.
//
// A scope served by a storage binding is answered first and unconditionally.
// The projection for such a scope withholds the whole backend namespace, so
// GC_DOLT_HOST arrives empty and every question below it reads a withheld
// projection as managed-local Dolt: canonicalScopeDoltTarget reports no managed
// runtime, and managedLocalDoltEnv agrees with an empty host. Recovering on
// that answer would start a managed Dolt server for a store gc does not serve.
// A binding that cannot be read is answered the same way, because a scope whose
// ownership is unknown is not a scope to start servers for.
func managedBDRecoveryAllowed(cityPath, scopeRoot string, env map[string]string) bool {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot); err != nil || bound {
		return false
	}
	if target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot); err != nil {
		return contract.IsManagedRuntimeUnavailable(err) && managedLocalDoltEnv(env)
	} else if ok {
		return !target.External && managedLocalDoltHost(target.Host)
	}
	return managedLocalDoltEnv(env)
}

func bdTransportErrorMatches(cityPath, scopeRoot string, env map[string]string, err error, markers []string) bool {
	if err == nil || !providerUsesBdStoreContract(rawBeadsProviderForScope(scopeRoot, cityPath)) || !managedBDRecoveryAllowed(cityPath, scopeRoot, env) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// bdSilentFallbackMarkerImport and bdSilentFallbackMarkerEmptyDB are the
// substring pair bd emits when it loses the managed Dolt server and silently
// falls back to opening the on-disk store with a JSONL auto-import. They are
// load-bearing in three places — the two transport-error classifiers below
// and bdOutputIndicatesSilentFallback — so they live here as the single
// source of truth. If bd's banner wording ever changes, this is the only
// edit site. (The root cause is fixed upstream in beads post-#3691; these
// markers remain the symptom detector for deployments still on stable bd.)
const (
	bdSilentFallbackMarkerImport  = "auto-importing"
	bdSilentFallbackMarkerEmptyDB = "into empty database"

	bdCommandRetryBaseDelay = 500 * time.Millisecond
)

var bdCommandRetrySleep = time.Sleep

func bdTransportRetryableError(cityPath, scopeRoot string, env map[string]string, err error) bool {
	return bdTransportErrorMatches(cityPath, scopeRoot, env, err, []string{
		"server unreachable",
		"dial tcp",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"bad connection",
		"use of closed network connection",
		// bd silently falls back to opening the on-disk store when it cannot
		// reach the managed Dolt server. On an empty .beads/dolt/ that fallback
		// triggers a JSONL auto-import, which presents as a 2m command timeout
		// rather than a network error. Treat the auto-import marker as a
		// transport failure so the managed-retry path republishes the correct
		// port and retries against the live server. See gastownhall/gascity#1930.
		bdSilentFallbackMarkerImport,
		bdSilentFallbackMarkerEmptyDB,
	})
}

func bdTransportRecoverableError(cityPath, scopeRoot string, env map[string]string, err error) bool {
	return bdTransportErrorMatches(cityPath, scopeRoot, env, err, []string{
		"server unreachable",
		"dial tcp",
		"connection refused",
		// When bd auto-imports into an empty on-disk store it has lost the
		// managed Dolt server; republishing the port via the recovery path
		// is what unsticks the next attempt. See gastownhall/gascity#1930.
		bdSilentFallbackMarkerImport,
		bdSilentFallbackMarkerEmptyDB,
	})
}

// bdOutputIndicatesSilentFallback reports whether the given bd output
// (typically captured stderr) contains the marker pair that bd emits
// when it loses the managed Dolt server and silently falls back to
// opening the on-disk store with a JSONL auto-import. Operators of
// `gc bd ...` rely on this to convert the silent fallback into a loud,
// non-zero-exit error — in managed mode (BD_EXPORT_AUTO=false) the
// fallback drops writes. See gastownhall/gascity#2080 (bd update path)
// and gastownhall/gascity#2079 (bd close path) — both subcommands flow
// through the shared doBd handoff, so a single detection site covers
// the bd-write-persistence quad.
func bdOutputIndicatesSilentFallback(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, bdSilentFallbackMarkerImport) &&
		strings.Contains(lower, bdSilentFallbackMarkerEmptyDB)
}

// bdDoltStartSuggestionMarkerAutoStart and bdDoltStartSuggestionMarkerCommand
// are the substring pair bd emits when the managed Dolt server is
// unreachable and dolt.auto-start is disabled: bd suggests running
// `bd dolt start` to recover. In a gc-managed city (gc always sets
// dolt.auto-start: false) that suggestion is actively harmful — it starts a
// second, unmanaged Dolt server that fights gc's own server for the same
// data directory (gastownhall/gascity#1374). Load-bearing only in
// bdOutputSuggestsConflictingDoltStart; if bd's banner wording ever
// changes, this is the only edit site.
const (
	bdDoltStartSuggestionMarkerAutoStart = "auto-start is disabled"
	bdDoltStartSuggestionMarkerCommand   = "bd dolt start"
)

// bdOutputSuggestsConflictingDoltStart reports whether the given bd output
// (typically captured stderr) contains the marker pair bd emits when it
// tells the operator to run `bd dolt start` because managed auto-start is
// disabled. Requiring both markers (rather than the command substring
// alone) avoids a false positive on unrelated mentions of "bd dolt start",
// e.g. in bd's own --help text.
func bdOutputSuggestsConflictingDoltStart(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, bdDoltStartSuggestionMarkerAutoStart) &&
		strings.Contains(lower, bdDoltStartSuggestionMarkerCommand)
}

// bdScopeDoltIsGcManaged reports whether the corrective `gc start` /
// `gc dolt restart` hint applies to this scope. gc owns the Dolt
// lifecycle only for a managed, local endpoint: an externally-bound
// store, or an explicit/city-canonical endpoint (which resolves
// External even on 127.0.0.1), is not gc's to restart, and pointing the
// operator at gc lifecycle commands there sends them at the wrong
// remedy. Mirrors the ownership predicate managedBDRecoveryAllowed
// applies. Fails closed — no hint — when ownership cannot be resolved.
func bdScopeDoltIsGcManaged(cityPath, scopeRoot string) bool {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot); err != nil || bound {
		return false
	}
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil || !ok {
		return false
	}
	return !target.External && managedLocalDoltHost(target.Host)
}

func bdCommandRunnerWithManagedRetry(cityPath string, envFn func(dir string) map[string]string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		return envFn(dir), nil
	})
}

func bdCommandRunnerWithManagedRetryErr(cityPath string, envFn func(dir string) (map[string]string, error)) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env, envErr := envFn(dir)
		if envErr != nil {
			return nil, envErr
		}
		if env == nil {
			env = map[string]string{}
		}
		ensureProjectedDoltEnvExplicit(env)
		runner, runnerErr := beadsCommandRunnerForHostedCity(cityPath, env)
		if runnerErr != nil {
			return nil, runnerErr
		}
		out, err := runner(dir, name, args...)
		if name != "bd" {
			return out, err
		}
		if !bdTransportRetryableError(cityPath, dir, env, err) {
			return out, err
		}
		if bdTransportRecoverableError(cityPath, dir, env, err) {
			if recErr := recoverManagedBDCommand(cityPath); recErr != nil {
				return out, err
			}
		}
		bdCommandRetrySleep(bdCommandRetryBaseDelay)
		retryEnv, retryEnvErr := envFn(dir)
		if retryEnvErr != nil {
			return nil, retryEnvErr
		}
		ensureProjectedDoltEnvExplicit(retryEnv)
		retryRunner, runnerErr := beadsCommandRunnerForHostedCity(cityPath, retryEnv)
		if runnerErr != nil {
			return nil, runnerErr
		}
		return retryRunner(dir, name, args...)
	}
}

func applyResolvedCityDoltEnv(env map[string]string, cityPath string, allowRecovery bool) error {
	return applyResolvedCityDoltEnvContext(context.Background(), env, cityPath, allowRecovery)
}

func applyResolvedCityDoltEnvContext(ctx context.Context, env map[string]string, cityPath string, allowRecovery bool) error {
	target, ok, err := resolvedRuntimeCityDoltTargetContext(ctx, cityPath, allowRecovery)
	if err != nil {
		return err
	}
	fallbackUser := ""
	if ok {
		applyCanonicalDoltTargetEnv(env, target)
		fallbackUser = strings.TrimSpace(target.User)
	}
	applyResolvedDoltAuthEnv(env, cityPath, fallbackUser)
	mirrorBeadsDoltScopeEnv(env, target)
	return nil
}

func rigConfigForScopeRoot(cityPath, rigPath string, rigs []config.Rig) *config.Rig {
	rigPath = filepath.Clean(rigPath)
	for i := range rigs {
		rp := rigs[i].Path
		if !filepath.IsAbs(rp) {
			rp = filepath.Join(cityPath, rp)
		}
		if samePath(rp, rigPath) {
			return &rigs[i]
		}
	}
	return nil
}

func rigAllowsManagedCityRuntimeRecovery(cityPath, rigPath string) bool {
	rigResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rigPath, "")
	if err != nil || rigResolved.Kind != contract.ScopeConfigAuthoritative || rigResolved.State.EndpointOrigin != contract.EndpointOriginInheritedCity {
		return false
	}
	cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		return false
	}
	return cityResolved.Kind == contract.ScopeConfigAuthoritative && cityResolved.State.EndpointOrigin == contract.EndpointOriginManagedCity
}

func rigAllowsResolvedCityTargetFallback(cityPath, rigPath string) bool {
	rigResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rigPath, "")
	if err != nil || rigResolved.Kind != contract.ScopeConfigAuthoritative || rigResolved.State.EndpointOrigin != contract.EndpointOriginInheritedCity {
		return false
	}
	cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		return false
	}
	return cityResolved.Kind != contract.ScopeConfigAuthoritative
}

func applyResolvedRigDoltEnv(env map[string]string, cityPath, rigPath string, explicitRig *config.Rig, allowRecovery bool) error {
	return applyResolvedRigDoltEnvContext(context.Background(), env, cityPath, rigPath, explicitRig, allowRecovery)
}

func applyResolvedRigDoltEnvContext(ctx context.Context, env map[string]string, cityPath, rigPath string, explicitRig *config.Rig, allowRecovery bool) error {
	if usedCanonical, err := applyCanonicalScopeBackendEnv(env, cityPath, rigPath); err != nil {
		var invalid *contract.InvalidCanonicalConfigError
		if errors.As(err, &invalid) {
			fallback, fallbackErr := contract.AllowsInvalidInheritedCityFallback(fsys.OSFS{}, cityPath, rigPath)
			if fallbackErr == nil && fallback {
				return applyResolvedCityDoltEnvContext(ctx, env, cityPath, allowRecovery)
			}
		}
		if rigAllowsResolvedCityTargetFallback(cityPath, rigPath) {
			return applyResolvedCityDoltEnvContext(ctx, env, cityPath, allowRecovery)
		}
		if allowRecovery && contract.IsManagedRuntimeUnavailable(err) && rigAllowsManagedCityRuntimeRecovery(cityPath, rigPath) {
			return applyResolvedCityDoltEnvContext(ctx, env, cityPath, true)
		}
		return err
	} else if usedCanonical {
		return nil
	}
	if explicitRig != nil && (explicitRig.DoltHost != "" || explicitRig.DoltPort != "") {
		target := applyLegacyRigExternalTarget(env, *explicitRig)
		clearProjectedDoltPasswordEnv(env)
		applyResolvedDoltAuthEnv(env, rigPath, "")
		mirrorBeadsDoltScopeEnv(env, target)
		return nil
	}
	// Rigs without local endpoint authority inherit the resolved city target.
	// A minimal local .beads/config.yaml must not suppress valid city compat fallback.
	return applyResolvedCityDoltEnvContext(ctx, env, cityPath, allowRecovery)
}

// applyLegacyRigExternalTarget projects a legacy config.Rig{DoltHost,DoltPort}
// external endpoint onto env and returns the resolved external
// DoltConnectionTarget. A rig that sets an explicit Dolt host/port is an
// external endpoint by construction — the same way applyCanonicalConfigStateDoltEnv
// treats an explicit canonical endpoint as External — so callers mirror the
// returned target through mirrorBeadsDoltScopeEnv. That helper carries the
// hosted-gateway BEADS_DOLT_SERVER_TLS requirement into the native-open env only
// for a non-local endpoint (targetCarriesHostedGatewayTLS): a legacy rig that
// points at a hosted gateway connects with TLS, while an explicit or port-only
// 127.0.0.1 rig stays plaintext instead of inheriting a controller's ambient
// TLS=1. Using the non-scoped mirrorBeadsDoltEnv here would clear TLS for the
// gateway case too and force a TLS-required gateway rig configured through this
// compatibility path to attempt plaintext.
func applyLegacyRigExternalTarget(env map[string]string, rig config.Rig) contract.DoltConnectionTarget {
	host, port := configuredExternalDoltTargetForRig(rig)
	if host != "" {
		env["GC_DOLT_HOST"] = host
	}
	if port != "" {
		env["GC_DOLT_PORT"] = port
	}
	return contract.DoltConnectionTarget{Host: host, Port: port, External: true}
}

func rigRuntimeEnvIndependentOfCityProjection(cityPath, rigPath string, explicitRig *config.Rig) bool {
	if explicitRig != nil && (explicitRig.DoltHost != "" || explicitRig.DoltPort != "") {
		return true
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rigPath, "")
	if err != nil {
		return false
	}
	return resolved.Kind == contract.ScopeConfigAuthoritative && resolved.State.EndpointOrigin != contract.EndpointOriginInheritedCity
}

func bdRuntimeEnvForRigWithError(cityPath string, cfg *config.City, rigPath string) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecovery(cityPath, cfg, rigPath, true)
}

// bdRuntimeEnvForRigWithErrorNoRecovery is bdRuntimeEnvForRigWithError
// without the managed-dolt recovery side effects; see
// bdRuntimeEnvWithErrorNoRecovery for why (gascity ga-cdmx6x).
func bdRuntimeEnvForRigWithErrorNoRecovery(cityPath string, cfg *config.City, rigPath string) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecovery(cityPath, cfg, rigPath, false)
}

func bdRuntimeEnvForRigWithErrorRecovery(cityPath string, cfg *config.City, rigPath string, allowRecovery bool) (map[string]string, error) {
	return bdRuntimeEnvForRigWithErrorRecoveryContext(context.Background(), cityPath, cfg, rigPath, allowRecovery)
}

func bdRuntimeEnvForRigWithErrorRecoveryContext(ctx context.Context, cityPath string, cfg *config.City, rigPath string, allowRecovery bool) (map[string]string, error) {
	env, cityErr := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, allowRecovery)
	rigPath = normalizePathForCompare(rigPath)
	// Pin the rig store explicitly. The gc-beads-bd provider derives its Dolt
	// data root from GC_CITY_PATH unless BEADS_DIR is set, so cwd-based
	// discovery is not sufficient for rig-scoped operations.
	env["BEADS_DIR"] = filepath.Join(rigPath, ".beads")
	env["GC_RIG_ROOT"] = rigPath
	var explicitRig *config.Rig
	if cfg != nil {
		explicitRig = rigConfigForScopeRoot(cityPath, rigPath, cfg.Rigs)
		if explicitRig != nil {
			env["GC_RIG"] = explicitRig.Name
		}
	}
	if scopeUsesProxiedDoltMode(cityPath, rigPath) || (!scopeOverridesCityBackend(cityPath, rigPath) && scopeUsesProxiedDoltMode(cityPath, cityPath)) {
		applyProxiedDoltEnv(env)
		return env, nil
	}
	rigDoltlite := scopeBackendIsDoltlite(cityPath, rigPath)
	cityDoltlite := scopeBackendIsDoltlite(cityPath, cityPath)
	if rigDoltlite || (cityDoltlite && !scopeOverridesCityBackend(cityPath, rigPath)) {
		clearProjectedDoltEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if err := applyResolvedRigDoltEnvContext(ctx, env, cityPath, rigPath, explicitRig, allowRecovery); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
		if isRecoverableManagedDoltEnvError(err) {
			return env, nil
		}
		return env, err
	}
	if cityErr != nil {
		return env, cityErr
	}
	return env, nil
}

func nativeDoltOpenEnvForScope(cityPath string, cfg *config.City, scopeRoot string) (map[string]string, error) {
	return nativeDoltOpenEnvForScopeContext(context.Background(), cityPath, cfg, scopeRoot)
}

func nativeDoltOpenEnvForScopeContext(ctx context.Context, cityPath string, cfg *config.City, scopeRoot string) (map[string]string, error) {
	scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
	if samePath(scopeRoot, cityPath) {
		return bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, true)
	}
	if cfg == nil {
		loaded, err := loadCityConfig(cityPath, io.Discard)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}
	return bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, scopeRoot, true)
}

func bdRuntimeEnvWithError(cityPath string) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecovery(cityPath, true)
}

// bdRuntimeEnvWithErrorNoRecovery is bdRuntimeEnvWithError without the
// managed-dolt recovery/health-check/autostart side effects: it reads
// existing published or configured connection state only, and fails fast
// (env still gets the non-Dolt opt-out vars set) when no managed server is
// currently reachable. Recovering a managed dolt server is legitimate
// work, but doing it from every concurrent, short-budget scoped-store
// construction would multiply exactly the load a read-storm mitigation
// exists to bound (gascity ga-cdmx6x) — those callers use this instead of
// bdRuntimeEnvWithError.
func bdRuntimeEnvWithErrorNoRecovery(cityPath string) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecovery(cityPath, false)
}

func bdRuntimeEnvWithErrorRecovery(cityPath string, allowRecovery bool) (map[string]string, error) {
	return bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, allowRecovery)
}

func bdRuntimeEnvWithErrorRecoveryContext(ctx context.Context, cityPath string, allowRecovery bool) (map[string]string, error) {
	env := cityRuntimeEnvMapForCity(cityPath)
	env["BEADS_DIR"] = filepath.Join(cityPath, ".beads")
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	// Suppress bd's built-in Dolt auto-start. The gc controller manages the
	// Dolt server lifecycle via gc-beads-bd; bd's CLI auto-start ignores the
	// dolt.auto-start:false config (beads resolveAutoStart priority bug) and
	// starts rogue servers from the agent's cwd with the wrong data_dir.
	env["BEADS_DOLT_AUTO_START"] = "0"
	// Suppress bd's auto-export of issues.jsonl on every write. The canonical
	// config also persists export.auto:false (see internal/beads/contract/files.go),
	// but the env var is the bulletproof per-invocation guard: it covers fresh
	// scopes whose config has not yet been canonicalized, and it short-circuits
	// the export → next-write-auto-import stall cycle (sa-41j3kp) even when an
	// out-of-band caller has left a stale .beads/issues.jsonl on disk. Without
	// this, bd's "auto-importing N bytes ... into empty database" path can
	// stall bd create / gc mail send for the full 2m subprocess timeout on
	// large datasets.
	env["BD_EXPORT_AUTO"] = "false"
	// Disable bd's fork/contributor auto-routing. Without this, a store with
	// routing.mode=auto + routing.contributor (gcy's ~/.beads-planning) sends
	// bd create/list/update to that out-of-band store while gc's in-process
	// dispatch (sling) and bd show read the scope store — a three-way split
	// brain. See bdContributorRoutingOptOutEnvKeys.
	applyBdContributorRoutingOptOut(env)
	applyBdCLIRemoteSyncOptOut(env)
	// Suppress bd's PersistentPostRun auto-backup (the "backup_export" Dolt
	// remote). Like BD_EXPORT_AUTO above, the env var is the bulletproof
	// per-invocation guard: it covers fresh rig scopes whose config has not
	// been canonicalized and overrides any drifted backup.enabled:true. A
	// stuck-looping backup_export sync wedged the whole town on 2026-06-08
	// (ga-0eq); managed backups run through mol-dog-backup, not this path.
	applyBdAutoBackupOptOut(env)
	if !cityUsesBdStoreContract(cityPath) {
		return env, nil
	}
	if scopeUsesProxiedDoltMode(cityPath, cityPath) {
		applyProxiedDoltEnv(env)
		return env, nil
	}
	if err := applyHostedBeadsCredentialEnv(env, cityPath); err != nil {
		return env, err
	}
	if scopeBackendIsDoltlite(cityPath, cityPath) {
		clearProjectedDoltEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if bound, err := applyCityStorageBindingEnv(env, cityPath); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
		return env, err
	} else if bound {
		return env, nil
	}
	if err := applyResolvedCityDoltEnvContext(ctx, env, cityPath, allowRecovery); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
		if isRecoverableManagedDoltEnvError(err) {
			return env, nil
		}
		return env, err
	}
	return env, nil
}

func isRecoverableManagedDoltEnvError(err error) bool {
	if err == nil {
		return false
	}
	return contract.IsManagedRuntimeUnavailable(err)
}

func cityRuntimeEnvMapForCity(cityPath string) map[string]string {
	return citylayout.CityRuntimeEnvMapForRuntimeDir(cityPath, citylayout.TrustedAmbientCityRuntimeDir(cityPath))
}

// cityIdentityAnchorsForCity returns only the three identity anchors
// (GC_CITY, GC_CITY_PATH, GC_CITY_RUNTIME_DIR) for cityPath. The shared
// projection lives in internal/citylayout so CLI and API session resolvers
// keep the identity-only contract in sync.
func cityIdentityAnchorsForCity(cityPath string) map[string]string {
	return citylayout.CityIdentityEnvMap(cityPath)
}

func cityRuntimeProcessEnvWithError(cityPath string) ([]string, error) {
	cityPath = normalizePathForCompare(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	var projectionErr error
	var hostedBeads bool
	if cityUsesBdStoreContract(cityPath) {
		var err error
		hostedBeads, err = citySelectsHostedBeadsCredentialProvider(cityPath)
		if err != nil {
			projectionErr = err
		}
		source := map[string]string{"BEADS_DOLT_AUTO_START": "0"}
		applyBdContributorRoutingOptOut(source)
		applyBdCLIRemoteSyncOptOut(source)
		applyBdAutoBackupOptOut(source)
		if err := applyHostedBeadsCredentialEnv(source, cityPath); err != nil {
			projectionErr = err
		}
		if bound, err := applyCityStorageBindingEnv(source, cityPath); err != nil {
			clearProjectedDoltEnv(source)
			mirrorBeadsDoltEnv(source)
			projectionErr = err
		} else if !bound {
			err := applyResolvedCityDoltEnv(source, cityPath, false)
			if err != nil {
				// Mirror the storage-binding error branch: clearing the projected Dolt
				// keys alone leaves BEADS_DOLT_SERVER_TLS unset in source, so it
				// never reaches overrides and preserveHostedBeadsCredentialEnv
				// re-injects the ambient hosted-gateway TLS=1 onto this
				// local/plaintext fallback. mirrorBeadsDoltEnv stamps the
				// non-external TLS="" clear so the fallback stays plaintext.
				clearProjectedDoltEnv(source)
				mirrorBeadsDoltEnv(source)
			}
		}
		keys := execProjectedBackendCopyKeys()
		// BEADS_DOLT_AUTO_START and BEADS_DOLT_SERVER_TLS are carried explicitly:
		// neither is in execProjectedBackendCopyKeys. TLS is deliberately kept out
		// of projectedDoltEnvKeys because it is a hosted-gateway credential
		// passthrough (preserveHostedBeadsCredentialEnv must be able to keep an
		// ambient value), but the scope mirror sets source[BEADS_DOLT_SERVER_TLS]=""
		// for a non-external/local city. That cleared value has to reach overrides —
		// present including empty — or preserveHostedBeadsCredentialEnv re-injects
		// the ambient hosted-gateway TLS=1 and forces TLS against a plaintext local
		// Dolt server.
		keys = append(keys, "BEADS_DOLT_AUTO_START", "BEADS_DOLT_SERVER_TLS")
		for _, key := range keys {
			if value, ok := source[key]; ok {
				overrides[key] = value
			}
		}
	}
	environ := processEnvSnapshotExcludingNativeDoltOpen()
	if hostedBeads {
		environ = removeEnvKeyPrefix(environ, "BEADS_")
	}
	return mergeRuntimeEnv(environ, overrides), projectionErr
}

func applyBdCLIRemoteSyncOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdCLIRemoteSyncOptOutEnvKeys {
		env[key] = "false"
	}
}

// applyBdAutoBackupOptOut forces bd's PersistentPostRun auto-backup off for
// gc-managed bd invocations. It overrides any ambient or per-scope config
// value so a fresh or drifted rig store cannot re-enable the destructive
// backup_export sync (ga-0eq). See bdAutoBackupOptOutEnvKeys.
func applyBdAutoBackupOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdAutoBackupOptOutEnvKeys {
		env[key] = "false"
	}
}

// applyBdContributorRoutingOptOut forces bd's fork/contributor auto-routing
// off for gc-managed bd invocations. It overrides any ambient or per-scope
// routing.mode=auto config so a gcy-style store cannot siphon create/list/
// update to ~/.beads-planning while sling/show read the scope store — the
// three-way split brain documented on bdContributorRoutingOptOutEnvKeys.
func applyBdContributorRoutingOptOut(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range bdContributorRoutingOptOutEnvKeys {
		env[key] = "off"
	}
}

// mirrorBeadsDoltEnv projects the GC_DOLT_* connection values onto the
// BEADS_DOLT_SERVER_* names beadslib's in-process native store reads, and clears
// the native-open TLS requirement. Clearing TLS is the safe default for every
// non-external scope (doltlite, managed-local dolt, cleared/error
// fallbacks): such a scope must never negotiate TLS, including a requirement
// inherited from a hosted-gateway city env this scope's map was cloned from (rig
// runtime env is built on top of the city env). A scope that resolves to an
// external endpoint uses mirrorBeadsDoltScopeEnv instead, which carries TLS only
// for a non-local hosted-gateway endpoint.
func mirrorBeadsDoltEnv(env map[string]string) {
	mirrorBeadsDoltServerEnv(env, false)
}

// mirrorBeadsDoltScopeEnv is mirrorBeadsDoltEnv keyed on a resolved Dolt target:
// it carries the hosted-gateway BEADS_DOLT_SERVER_TLS requirement into the
// native-open env only for a target that actually speaks hosted-gateway TLS — an
// external endpoint with a non-local host (targetCarriesHostedGatewayTLS).
// Callers that already hold the resolved DoltConnectionTarget use this so ambient
// TLS reaches only genuine hosted gateways and never bleeds into a local/plaintext
// scope, including an explicit or port-only 127.0.0.1 endpoint that resolves
// External by topology but connects in the clear.
func mirrorBeadsDoltScopeEnv(env map[string]string, target contract.DoltConnectionTarget) {
	mirrorBeadsDoltServerEnv(env, targetCarriesHostedGatewayTLS(target))
}

// targetCarriesHostedGatewayTLS reports whether ambient BEADS_DOLT_SERVER_TLS
// should be carried into target's native-open env. TLS is transport policy, not
// endpoint topology: DoltConnectionTarget.External marks every non-managed
// explicit/city-canonical endpoint — including a plaintext 127.0.0.1 or a
// port-only legacy rig that populateExternalTarget/canonicalExternalHost default
// to loopback — so gating the carry on External alone forces TLS onto plaintext
// local endpoints and breaks them under a controller running with ambient
// BEADS_DOLT_SERVER_TLS=1 (PR #4008 review finding). A hosted beads-gateway, the
// only endpoint that terminates client TLS, is remote by construction, so the
// carry is gated on an external endpoint with a non-local host. This is
// deliberately narrower than the identity-deferral gate (which stays keyed on
// External): deferral only Pass-marks and relies on beadslib's open-time identity
// check, so it is safe for a local endpoint the plaintext probe can already
// authenticate, whereas forcing TLS onto that same endpoint is not.
func targetCarriesHostedGatewayTLS(target contract.DoltConnectionTarget) bool {
	return target.External && !contract.DoltHostIsLocal(target.Host)
}

func mirrorBeadsDoltServerEnv(env map[string]string, carryAmbientTLS bool) {
	if env == nil {
		return
	}
	socket := strings.TrimSpace(env["BEADS_DOLT_SERVER_SOCKET"])
	if socket != "" {
		delete(env, "BEADS_DOLT_SERVER_HOST")
		env["BEADS_DOLT_SERVER_PORT"] = ""
		env["BEADS_DOLT_SERVER_SOCKET"] = socket
	} else {
		delete(env, "BEADS_DOLT_SERVER_SOCKET")
		if host := strings.TrimSpace(env["GC_DOLT_HOST"]); host != "" {
			env["BEADS_DOLT_SERVER_HOST"] = host
		} else {
			delete(env, "BEADS_DOLT_SERVER_HOST")
		}
		if port := strings.TrimSpace(env["GC_DOLT_PORT"]); port != "" {
			env["BEADS_DOLT_SERVER_PORT"] = port
		} else {
			env["BEADS_DOLT_SERVER_PORT"] = ""
		}
	}
	if user := strings.TrimSpace(env["GC_DOLT_USER"]); user != "" {
		env["BEADS_DOLT_SERVER_USER"] = user
	} else {
		delete(env, "BEADS_DOLT_SERVER_USER")
	}
	// Note: beads v1.0.0 reads BEADS_DOLT_PASSWORD (no _SERVER_ infix).
	// The asymmetry with BEADS_DOLT_SERVER_USER is intentional per beads
	// upstream convention.
	if pass := env["GC_DOLT_PASSWORD"]; pass != "" {
		env["BEADS_DOLT_PASSWORD"] = pass
	} else {
		delete(env, "BEADS_DOLT_PASSWORD")
	}
	// Carry the hosted beads-gateway credential command into the projected env.
	// bd authenticates to the gateway by running the helper named in
	// BEADS_DOLT_CREDENTIAL_COMMAND; without it bd falls back to the static/root
	// user and the gateway rejects the connection (MySQL Error 1045). That key
	// contains "CREDENTIAL", so execenv.FilterInherited strips it from every
	// gc-spawned bd subprocess and agent session. preserveHostedBeadsCredentialEnv
	// re-adds it on the slice-merge paths (overlayEnvEntries / mergeRuntimeEnv),
	// but only when it is already present in the pre-filter environ and only on
	// those paths — the agent session env is built from this projected map, which
	// does not carry the ambient value, and a controller that exports the helper
	// under only the non-sensitive GC_DOLT_CRED_CMD (which survives filtering) has
	// nothing for that pass to preserve. Mirror GC_DOLT_CRED_CMD into
	// BEADS_DOLT_CREDENTIAL_COMMAND here (map value wins, else the ambient value
	// of either key) so bd authenticates the same way the in-process native store
	// does.
	//
	// Two intentional asymmetries with the sibling BEADS_DOLT_* branches above,
	// kept deliberately (do not "normalize" them into the map->map convention):
	//  1. Ambient fallback via a bare os.Getenv: the external-endpoint TLS branch
	//     (mirrorNativeDoltTLSEnv) also reads ambient env, but through the
	//     ambientNativeDoltOpenEnv guard; this credential branch is the only one
	//     that reads the ambient process env with a bare os.Getenv, because a
	//     controller commonly exports only the helper and never seeds it into the
	//     projected map.
	//  2. Preserve-not-clear: when no source exists this branch leaves any
	//     existing target value untouched instead of deleting/emptying it. The
	//     siblings clear their target to defeat stale tmux inheritance; the
	//     credential key is instead preserved from ambient by
	//     preserveHostedBeadsCredentialEnv on the slice-merge paths, so clearing
	//     it here would fight that pass.
	if cred := strings.TrimSpace(env["GC_DOLT_CRED_CMD"]); cred != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = cred
	} else if cred := strings.TrimSpace(env["BEADS_DOLT_CREDENTIAL_COMMAND"]); cred != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = cred
	} else if ambient := strings.TrimSpace(os.Getenv("GC_DOLT_CRED_CMD")); ambient != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = ambient
	} else if ambient := strings.TrimSpace(os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")); ambient != "" {
		env["BEADS_DOLT_CREDENTIAL_COMMAND"] = ambient
	}
	mirrorNativeDoltTLSEnv(env, carryAmbientTLS)
}

// mirrorNativeDoltTLSEnv scopes the BEADS_DOLT_SERVER_TLS native-open requirement
// to the resolved target, keeping the GC_DOLT_*->BEADS_DOLT_* projection in
// mirrorBeadsDoltServerEnv flat and the TLS gate readable as one unit. Only a
// hosted-gateway target (carryAmbientTLS, computed by targetCarriesHostedGatewayTLS)
// negotiates TLS; every non-carry scope — non-external, and an external-but-local
// plaintext endpoint — clears it.
func mirrorNativeDoltTLSEnv(env map[string]string, carryAmbientTLS bool) {
	if !carryAmbientTLS {
		// Non-carry scope (non-external, or an external endpoint with a local host
		// such as an explicit or port-only 127.0.0.1 rig): clear any TLS
		// requirement — including one inherited from a hosted-gateway city env this
		// map was cloned from, or a stale ambient value — so the native store, and
		// the bd fallback built from the same map, connect with the scope's real
		// (plaintext) transport instead of forcing TLS against a non-TLS server.
		// Key kept present but empty (like the PORT projection in
		// mirrorBeadsDoltServerEnv) so a child bd or reused map cannot resurrect it.
		// TLS is not a DoltConnectionTarget field, so there is no GC_DOLT_* source
		// to project.
		env["BEADS_DOLT_SERVER_TLS"] = ""
		return
	}
	// Hosted-gateway endpoint: carry the TLS requirement to the in-process native
	// store. A hosted beads-gateway terminates client TLS and rejects plaintext
	// ("TLS required"); the shell-out bd inherits BEADS_DOLT_SERVER_TLS from the
	// ambient controller env, but the native store opens beadslib against this
	// projected map, which CityRuntimeEnvMapForRuntimeDir builds fresh without the
	// ambient value. An explicit scoped value wins; otherwise mirror it from the
	// ambient process env — the same signal bd reads. Read the ambient value
	// through the native-open env guard, not a bare os.Getenv: withNativeDoltOpenEnv
	// mutates BEADS_DOLT_SERVER_TLS under nativeDoltOpenEnvMu, so an unguarded read
	// here could observe a concurrent cross-scope open's transient TLS rather than
	// the true ambient value.
	if tls := strings.TrimSpace(env["BEADS_DOLT_SERVER_TLS"]); tls != "" {
		env["BEADS_DOLT_SERVER_TLS"] = tls
	} else if ambient := strings.TrimSpace(ambientNativeDoltOpenEnv("BEADS_DOLT_SERVER_TLS")); ambient != "" {
		env["BEADS_DOLT_SERVER_TLS"] = ambient
	} else {
		env["BEADS_DOLT_SERVER_TLS"] = ""
	}
}

// cityForStoreDir resolves ambient store contexts. GC_CITY intentionally wins
// over filesystem discovery here; callers with an authoritative city path or
// hook-projected store root must pass that city directly.
func cityForStoreDir(dir string) string {
	if cityPath, ok := resolveExplicitCityPathEnv(); ok {
		return cityPath
	}
	if p, err := findCity(dir); err == nil {
		return p
	}
	return dir
}

func overlayEnvEntries(environ []string, overrides map[string]string) []string {
	out := execenv.FilterInherited(environ)
	if len(overrides) == 0 {
		return out
	}
	overrideKeys := make([]string, 0, len(overrides))
	for key := range overrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		out = removeEnvKey(out, key)
		out = append(out, key+"="+overrides[key])
	}
	out = preserveHostedBeadsCredentialEnv(out, environ, overrides)
	return out
}

func mergeRuntimeEnv(environ []string, overrides map[string]string) []string {
	keys := []string{
		"BEADS_CREDENTIALS_FILE",
		"BEADS_BACKEND",
		"BEADS_DIR",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_PROXIED_SERVER",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_SOCKET",
		"BEADS_DOLT_SERVER_USER",
		"GC_CITY",
		"GC_CITY_ROOT", // kept for stripping: no code emits this anymore, but inherited values must be cleaned
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
		"GC_BEADS_BACKEND",
		"GC_DOLT",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_HOST",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_MANAGED_LOCAL",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_PORT",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_USER",
		"GC_PACK_STATE_DIR",
		"GC_RIG",
		"GC_RIG_ROOT",
	}
	keys = appendBdCLIRemoteSyncOptOutEnvKeys(keys)
	keys = appendBdAutoBackupOptOutEnvKeys(keys)
	keys = appendBdContributorRoutingOptOutEnvKeys(keys)
	if len(overrides) > 0 {
		for key := range overrides {
			if !containsString(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	out := execenv.FilterInherited(environ)
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
	out = preserveHostedBeadsCredentialEnv(out, environ, overrides)
	return out
}

// hostedBeadsCredentialPassthroughKeys are env vars the bd provider needs to
// authenticate to a hosted beads-gateway: the credential command and the inputs
// its helper (e.g. eia-helper) reads. Several contain execenv.IsSensitiveKey
// markers (CREDENTIAL / TOKEN) and would otherwise be stripped by
// FilterInherited, but they carry command/URL/path references — not secret
// values (the orchestrator key itself stays in a file mount) — so they are
// preserved explicitly for gc-spawned bd subprocesses.
var hostedBeadsCredentialPassthroughKeys = []string{
	"BEADS_DOLT_CREDENTIAL_COMMAND",
	"BEADS_DOLT_SERVER_TLS",
	registryCredentialProviderEnv,
	"ORCHESTRATOR_KEY_FILE",
	"EIA_AUDIENCE",
	"EIA_SCOPES",
	"STS_MACHINE_URL",
	"STS_TOKEN_URL",
}

// githubTokenExecEnvKeys are the GitHub CLI auth env vars an exec order needs
// to run `gh`. Merge orders (and other PR housekeeping) shell out to `gh`,
// which authenticates from GH_TOKEN (preferred) or GITHUB_TOKEN. Both keys
// contain the substring TOKEN, so execenv.IsSensitiveKey reports them sensitive
// and the curated order-exec env — built from a map, then merged through
// FilterInherited — never carries the controller's ambient token into the child
// process. Every `gh` call the order runs then fails auth even though the
// controller holds a valid token. GH_TOKEN wins over GITHUB_TOKEN in `gh`'s own
// precedence, but both are projected independently when present.
var githubTokenExecEnvKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
}

// projectGitHubTokenExecEnv copies the controller's ambient GitHub CLI auth
// tokens into an exec-order env map so shelled-out `gh` invocations
// authenticate. It mirrors the ambient value the same way mirrorBeadsDoltEnv
// carries the hosted-gateway credential command, rather than weakening
// execenv.IsSensitiveKey: keeping these keys sensitive means execenv.RedactText
// still masks their values in captured exec output and logs. A value already in
// the map (an explicit [order.env] entry) is left untouched so an order can
// scope its own credential, and only non-empty ambient values are projected.
func projectGitHubTokenExecEnv(env map[string]string) {
	if env == nil {
		return
	}
	for _, key := range githubTokenExecEnvKeys {
		if strings.TrimSpace(env[key]) != "" {
			continue
		}
		if ambient := strings.TrimSpace(os.Getenv(key)); ambient != "" {
			env[key] = ambient
		}
	}
}

// preserveHostedBeadsCredentialEnv re-adds the hosted-gateway credential env
// from the original (pre-filter) environ, unless an override already set the
// key. Without this, FilterInherited drops the credential command (and the
// STS token URL) and gc-spawned bd cannot reach a hosted beads-gateway.
func preserveHostedBeadsCredentialEnv(out, environ []string, overrides map[string]string) []string {
	for _, key := range hostedBeadsCredentialPassthroughKeys {
		if _, ok := overrides[key]; ok {
			continue
		}
		prefix := key + "="
		for _, entry := range environ {
			if strings.HasPrefix(entry, prefix) {
				out = removeEnvKey(out, key)
				out = append(out, entry)
				break
			}
		}
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
