package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

const defaultManagedDoltHost = "127.0.0.1"

// bdCommandRunnerForCity centralizes bd subprocess env construction so all
// GC-managed bd calls resolve Dolt against the same city-scoped runtime.
// Env is rebuilt on each call so GC_DOLT_PORT reflects the current managed
// dolt port (which can change across city restarts).
func bdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetry(cityPath, func(dir string) map[string]string {
		env := bdRuntimeEnv(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		return env
	})
}

func bdStoreForCity(dir, cityPath string) *beads.BdStore {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	return beads.NewBdStoreWithPrefix(dir, bdCommandRunnerForCity(cityPath), issuePrefixForScope(dir, cityPath, cfg))
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
	return beads.NewBdStoreWithPrefix(rigDir, bdCommandRunnerForRig(cityPath, cfg, rigDir), prefix)
}

func controlBdStoreForCity(dir, cityPath string, cfg *config.City) *beads.BdStore {
	return beads.NewBdStoreWithPrefix(dir, controlBdCommandRunnerForCity(cityPath), issuePrefixForScope(dir, cityPath, cfg))
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
	return beads.NewBdStoreWithPrefix(rigDir, controlBdCommandRunnerForRig(cityPath, cfg, rigDir), prefix)
}

func controlBdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetry(cityPath, func(dir string) map[string]string {
		env := bdRuntimeEnv(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		applyControllerBdEnv(env)
		return env
	})
}

func controlBdCommandRunnerForRig(cityPath string, cfg *config.City, rigDir string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		env := bdRuntimeEnvForRig(cityPath, cfg, rigDir)
		applyControllerBdEnv(env)
		return env
	})
}

func applyControlBdEnv(env map[string]string) {
	env["BD_EXPORT_AUTO"] = "false"
}

func applyControllerBdEnv(env map[string]string) {
	applyControlBdEnv(env)
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
	return bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return bdRuntimeEnvForRig(cityPath, cfg, rigDir)
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

func applyCanonicalDoltTargetEnv(env map[string]string, target contract.DoltConnectionTarget) {
	if env == nil {
		return
	}
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

// applyCanonicalScopeBackendEnv dispatches to the appropriate backend
// helper based on the scope's MetadataState.Backend. Renamed from
// applyCanonicalScopeDoltEnv; the Dolt body lives in the case "", "dolt"
// arm.
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
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if metaErr != nil {
		return true, metaErr
	}
	switch meta.Backend {
	case "", "dolt":
		target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
		if err != nil {
			return true, err
		}
		applyCanonicalDoltTargetEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		mirrorBeadsDoltEnv(env)
		return true, nil
	case "postgres":
		if err := applyResolvedScopePostgresEnv(env, scopeRoot, meta); err != nil {
			return true, err
		}
		return true, nil
	default:
		return true, fmt.Errorf("unsupported backend %q for scope %s", meta.Backend, scopeRoot)
	}
}

// scopeMetadataJSONPath returns the absolute path to a scope's
// .beads/metadata.json. Centralised so the dispatcher and the recovery
// hook helpers agree on the file location.
func scopeMetadataJSONPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", "metadata.json")
}

// applyResolvedScopePostgresEnv projects PG credentials and connection
// info into env. Caller guarantees meta.Backend == "postgres". The
// resolver chain in internal/pgauth supplies the password; the host,
// port, user, and database come straight from MetadataState.
//
// On resolver exhaustion returns an error wrapping
// pgauth.ErrNoPasswordResolvable; callers can match with errors.Is.
func applyResolvedScopePostgresEnv(env map[string]string, scopeRoot string, meta contract.MetadataState) error {
	if env == nil {
		return nil
	}
	endpoint := pgauth.Endpoint{
		Host: meta.PostgresHost,
		Port: meta.PostgresPort,
		User: meta.PostgresUser,
	}
	resolved, err := pgauth.ResolveFromEnv(env, scopeRoot, endpoint)
	if err != nil {
		return fmt.Errorf("resolving postgres credentials for %s: %w", scopeRoot, err)
	}
	env["GC_POSTGRES_PASSWORD"] = resolved.Password
	env["BEADS_POSTGRES_PASSWORD"] = resolved.Password
	env["BEADS_POSTGRES_HOST"] = meta.PostgresHost
	env["BEADS_POSTGRES_PORT"] = meta.PostgresPort
	env["BEADS_POSTGRES_USER"] = meta.PostgresUser
	env["BEADS_POSTGRES_DATABASE"] = meta.PostgresDatabase
	mirrorBeadsPostgresEnv(env)
	return nil
}

// mirrorBeadsPostgresEnv copies canonical (GC_*) PG env keys to their
// bd-expected (BEADS_POSTGRES_*) names. Today only the password has a
// canonical-vs-bd split; the function exists so future bd-side
// renames become a one-line change.
func mirrorBeadsPostgresEnv(env map[string]string) {
	if env == nil {
		return
	}
	if pass := env["GC_POSTGRES_PASSWORD"]; pass != "" {
		env["BEADS_POSTGRES_PASSWORD"] = pass
	} else {
		delete(env, "BEADS_POSTGRES_PASSWORD")
	}
	// HOST/PORT/USER/DATABASE: applyResolvedScopePostgresEnv populates
	// BEADS_POSTGRES_* directly from MetadataState; no GC_-side canonical
	// exists for those today. If a future refactor introduces GC_POSTGRES_HOST
	// etc., add the mirror copies here in the same shape as the password.
}

// projectedPostgresEnvKeys mirrors projectedDoltEnvKeys: the canonical
// list of env keys gc projects when a scope's backend is "postgres".
// The list MUST match exactly the PG-keys segment added to
// mergeRuntimeEnv's keys slice; TestProjectedKeysCoverage asserts the
// symmetry so a key added here without a matching strip-list entry
// fails CI.
var projectedPostgresEnvKeys = []string{
	"GC_POSTGRES_PASSWORD",
	"BEADS_POSTGRES_PASSWORD",
	"BEADS_POSTGRES_HOST",
	"BEADS_POSTGRES_PORT",
	"BEADS_POSTGRES_USER",
	"BEADS_POSTGRES_DATABASE",
}

// scopeBackendIsPostgres returns true when the scope's MetadataState
// has Backend == "postgres". On any read error returns false and the
// caller behaves as if the scope were Dolt-backed; the bd call's
// own error remains the operator's primary signal.
func scopeBackendIsPostgres(cityPath, scopeRoot string) bool {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil || !ok {
		return false
	}
	return meta.Backend == "postgres"
}

// scopePostgresEndpoint returns the host and port from the scope's
// MetadataState. Returns ("", "") on any read error; the caller
// embeds the empty values into the operator-facing error message
// rather than failing — the underlying bd transport error is what
// the operator most needs to debug.
func scopePostgresEndpoint(cityPath, scopeRoot string) (string, string) {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil || !ok {
		return "", ""
	}
	return meta.PostgresHost, meta.PostgresPort
}

// pgTransportError returns true when the wrapped error matches a
// known Postgres transport-layer failure marker. Used by the bd
// command runner to wrap the error with an operator-facing hint
// instead of attempting recovery — gc does not manage external PG
// endpoints.
func pgTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"dial tcp",
		"no such host",
		"i/o timeout",
		"password authentication failed",
		"pq:",
		"pgx:",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func applyCanonicalConfigStateDoltEnv(env map[string]string, cityPath, scopeRoot string, state contract.ConfigState) {
	target := contract.DoltConnectionTarget{
		Host:           strings.TrimSpace(state.DoltHost),
		Port:           strings.TrimSpace(state.DoltPort),
		User:           strings.TrimSpace(state.DoltUser),
		EndpointOrigin: state.EndpointOrigin,
		EndpointStatus: state.EndpointStatus,
		External:       true,
	}
	applyCanonicalDoltTargetEnv(env, target)
	applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
	mirrorBeadsDoltEnv(env)
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
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_PASSWORD",
}

var beadsExecCommandRunnerWithEnv = beads.ExecCommandRunnerWithEnv

var recoverManagedBDCommand = func(cityPath string) error {
	script := gcBeadsBdScriptPath(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	setProjectedDoltEnvEmpty(overrides)
	environ := mergeRuntimeEnv(os.Environ(), overrides)
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
	for _, key := range projectedDoltEnvKeys {
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

func resolvedRuntimeCityDoltTarget(cityPath string, allowRecovery bool) (contract.DoltConnectionTarget, bool, error) {
	if target, ok, err := canonicalScopeDoltTarget(cityPath, cityPath); err != nil {
		if !allowRecovery || !contract.IsManagedRuntimeUnavailable(err) {
			return contract.DoltConnectionTarget{}, false, err
		}
	} else if ok {
		return target, true, nil
	}
	if host, port, ok, invalid := resolveConfiguredCityDoltTarget(cityPath); invalid {
		return contract.DoltConnectionTarget{}, false, fmt.Errorf("invalid canonical city endpoint state")
	} else if ok {
		return contract.DoltConnectionTarget{Host: host, Port: port, External: true}, true, nil
	}

	hostOverride := strings.TrimSpace(os.Getenv("GC_DOLT_HOST"))
	if hostOverride != "" && !managedLocalDoltHost(hostOverride) {
		return contract.DoltConnectionTarget{
			Host:     hostOverride,
			Port:     strings.TrimSpace(os.Getenv("GC_DOLT_PORT")),
			External: true,
		}, true, nil
	}

	if port := currentManagedDoltPort(cityPath); port != "" {
		return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
	}
	if allowRecovery {
		if err := healthBeadsProvider(cityPath); err == nil {
			if port := currentManagedDoltPort(cityPath); port != "" {
				return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
			}
		}
	}
	return contract.DoltConnectionTarget{}, false, nil
}

func managedLocalDoltEnv(env map[string]string) bool {
	return managedLocalDoltHost(env["GC_DOLT_HOST"])
}

func managedBDRecoveryAllowed(cityPath, scopeRoot string, env map[string]string) bool {
	if scopeRoot == "" {
		scopeRoot = cityPath
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
		"auto-importing",
		"into empty database",
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
		"auto-importing",
		"into empty database",
	})
}

func bdCommandRunnerWithManagedRetry(cityPath string, envFn func(dir string) map[string]string) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		env := envFn(dir)
		ensureProjectedDoltEnvExplicit(env)
		runner := beadsExecCommandRunnerWithEnv(env)
		out, err := runner(dir, name, args...)
		if name != "bd" {
			return out, err
		}
		// PG-backed scopes never invoke managed-Dolt recovery. A transport
		// error gets wrapped with an operator-facing hint and surfaced; gc
		// does not manage external PG endpoints.
		if scopeBackendIsPostgres(cityPath, dir) {
			if err != nil && pgTransportError(err) {
				host, port := scopePostgresEndpoint(cityPath, dir)
				return out, fmt.Errorf("postgres at %s:%s is unreachable; gc does not manage external PG endpoints: %w", host, port, err)
			}
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
		retryEnv := envFn(dir)
		ensureProjectedDoltEnvExplicit(retryEnv)
		retryRunner := beadsExecCommandRunnerWithEnv(retryEnv)
		return retryRunner(dir, name, args...)
	}
}

func applyResolvedCityDoltEnv(env map[string]string, cityPath string, allowRecovery bool) error {
	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, allowRecovery)
	if err != nil {
		return err
	}
	fallbackUser := ""
	if ok {
		applyCanonicalDoltTargetEnv(env, target)
		fallbackUser = strings.TrimSpace(target.User)
	}
	applyResolvedDoltAuthEnv(env, cityPath, fallbackUser)
	mirrorBeadsDoltEnv(env)
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
	if usedCanonical, err := applyCanonicalScopeBackendEnv(env, cityPath, rigPath); err != nil {
		var invalid *contract.InvalidCanonicalConfigError
		if errors.As(err, &invalid) {
			fallback, fallbackErr := contract.AllowsInvalidInheritedCityFallback(fsys.OSFS{}, cityPath, rigPath)
			if fallbackErr == nil && fallback {
				return applyResolvedCityDoltEnv(env, cityPath, allowRecovery)
			}
		}
		if rigAllowsResolvedCityTargetFallback(cityPath, rigPath) {
			return applyResolvedCityDoltEnv(env, cityPath, allowRecovery)
		}
		if allowRecovery && contract.IsManagedRuntimeUnavailable(err) && rigAllowsManagedCityRuntimeRecovery(cityPath, rigPath) {
			return applyResolvedCityDoltEnv(env, cityPath, true)
		}
		return err
	} else if usedCanonical {
		return nil
	}
	if explicitRig != nil && (explicitRig.DoltHost != "" || explicitRig.DoltPort != "") {
		applyLegacyRigExternalTarget(env, *explicitRig)
		clearProjectedDoltPasswordEnv(env)
		applyResolvedDoltAuthEnv(env, rigPath, "")
		mirrorBeadsDoltEnv(env)
		return nil
	}
	// Rigs without local endpoint authority inherit the resolved city target.
	// A minimal local .beads/config.yaml must not suppress valid city compat fallback.
	return applyResolvedCityDoltEnv(env, cityPath, allowRecovery)
}

func applyLegacyRigExternalTarget(env map[string]string, rig config.Rig) {
	host, port := configuredExternalDoltTargetForRig(rig)
	if host != "" {
		env["GC_DOLT_HOST"] = host
	}
	if port != "" {
		env["GC_DOLT_PORT"] = port
	}
}

// bdRuntimeEnvForRig returns the bd runtime environment for a rig directory.
// If the rig has custom DoltHost/DoltPort in city.toml, those override the
// city-level Dolt config. Otherwise falls back to bdRuntimeEnv(cityPath).
func bdRuntimeEnvForRig(cityPath string, cfg *config.City, rigPath string) map[string]string {
	env := bdRuntimeEnv(cityPath)
	rigPath = filepath.Clean(rigPath)
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
	if err := applyResolvedRigDoltEnv(env, cityPath, rigPath, explicitRig, true); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
	}
	return env
}

func bdRuntimeEnv(cityPath string) map[string]string {
	env := cityRuntimeEnvMapForCity(cityPath)
	env["BEADS_DIR"] = filepath.Join(cityPath, ".beads")
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	// Suppress bd's built-in Dolt auto-start. The gc controller manages the
	// Dolt server lifecycle via gc-beads-bd; bd's CLI auto-start ignores the
	// dolt.auto-start:false config (beads resolveAutoStart priority bug) and
	// starts rogue servers from the agent's cwd with the wrong data_dir.
	env["BEADS_DOLT_AUTO_START"] = "0"
	if !cityUsesBdStoreContract(cityPath) {
		return env
	}
	if err := applyResolvedCityDoltEnv(env, cityPath, true); err != nil {
		clearProjectedDoltEnv(env)
		mirrorBeadsDoltEnv(env)
	}
	return env
}

func cityRuntimeEnvMapForCity(cityPath string) map[string]string {
	return citylayout.CityRuntimeEnvMapForRuntimeDir(cityPath, citylayout.TrustedAmbientCityRuntimeDir(cityPath))
}

func cityRuntimeProcessEnv(cityPath string) []string {
	cityPath = normalizePathForCompare(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	if cityUsesBdStoreContract(cityPath) {
		source := map[string]string{"BEADS_DOLT_AUTO_START": "0"}
		if err := applyResolvedCityDoltEnv(source, cityPath, false); err != nil {
			clearProjectedDoltEnv(source)
		}
		for _, key := range []string{
			"GC_DOLT_HOST",
			"GC_DOLT_PORT",
			"GC_DOLT_USER",
			"GC_DOLT_PASSWORD",
			"BEADS_CREDENTIALS_FILE",
			"BEADS_DOLT_SERVER_HOST",
			"BEADS_DOLT_SERVER_PORT",
			"BEADS_DOLT_SERVER_USER",
			"BEADS_DOLT_PASSWORD",
			"BEADS_DOLT_AUTO_START",
		} {
			if value, ok := source[key]; ok {
				overrides[key] = value
			}
		}
	}
	return mergeRuntimeEnv(os.Environ(), overrides)
}

func mirrorBeadsDoltEnv(env map[string]string) {
	if env == nil {
		return
	}
	if host := strings.TrimSpace(env["GC_DOLT_HOST"]); host != "" {
		env["BEADS_DOLT_SERVER_HOST"] = host
	} else {
		delete(env, "BEADS_DOLT_SERVER_HOST")
	}
	if port := strings.TrimSpace(env["GC_DOLT_PORT"]); port != "" {
		env["BEADS_DOLT_SERVER_PORT"] = port
	} else {
		// Keep the key present so child bd processes cannot inherit a stale
		// BEADS_DOLT_SERVER_PORT from an ambient parent environment.
		env["BEADS_DOLT_SERVER_PORT"] = ""
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
}

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
	return out
}

func mergeRuntimeEnv(environ []string, overrides map[string]string) []string {
	keys := []string{
		"BEADS_CREDENTIALS_FILE",
		"BEADS_DIR",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_POSTGRES_DATABASE",
		"BEADS_POSTGRES_HOST",
		"BEADS_POSTGRES_PASSWORD",
		"BEADS_POSTGRES_PORT",
		"BEADS_POSTGRES_USER",
		"GC_CITY",
		"GC_CITY_ROOT", // kept for stripping: no code emits this anymore, but inherited values must be cleaned
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
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
		"GC_POSTGRES_PASSWORD",
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
