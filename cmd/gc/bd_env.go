package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

const defaultManagedDoltHost = "127.0.0.1"

var postgresCredentialResolvedSeen sync.Map // map[string]struct{}

func postgresCredentialResolvedKey(cityPath string, payload pgauth.PostgresCredentialResolvedPayload) string {
	return strings.Join([]string{
		cityPath,
		payload.ScopeKind,
		payload.ScopeName,
		payload.Source,
		payload.Host,
		payload.Port,
		payload.User,
	}, "\x00")
}

// bdCommandRunnerForCity centralizes bd subprocess env construction so all
// GC-managed bd calls resolve Dolt against the same city-scoped runtime.
// Env is rebuilt on each call so GC_DOLT_PORT reflects the current managed
// dolt port (which can change across city restarts).
func bdCommandRunnerForCity(cityPath string) beads.CommandRunner {
	return bdCommandRunnerWithManagedRetryErr(cityPath, func(dir string) (map[string]string, error) {
		env, err := bdRuntimeEnvWithError(cityPath)
		env["BEADS_DIR"] = filepath.Join(dir, ".beads")
		return env, err
	})
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
	if cfg != nil && cfg.Beads.UsesBD105CLISemantics() {
		return []beads.BdStoreOption{beads.WithBdStoreListSkipLabels(true)}
	}
	return nil
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
// Dolt projection would resolve auth for the city scope: the scope
// backend is not postgres and the scope config resolves authoritative —
// the same ResolveScopeConfigState gate applyOrderExecCanonicalDoltEnv
// and its managed fallback apply before calling
// applyCanonicalDoltAuthEnv. Callers that feed ambient environments
// into the projection use this to strip untrusted password mirrors
// from the resolution input without breaking the strict no-op
// pass-through for non-authoritative scopes.
func canonicalScopeDoltProjectionAuthoritative(cityPath string) bool {
	if scopeBackendIsPostgres(cityPath, cityPath) {
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
	meta, _, metaErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if metaErr != nil {
		return true, metaErr
	}
	if resolved.State.EndpointOrigin == contract.EndpointOriginInheritedCity && meta.Backend == "" {
		if usedPostgres, err := applyCityPostgresBackendEnv(env, cityPath); err != nil {
			return true, err
		} else if usedPostgres {
			return true, nil
		}
	}
	if resolved.State.EndpointOrigin == contract.EndpointOriginInheritedCity &&
		(meta.Backend == "" || meta.Backend == "doltlite") &&
		cityUsesDoltliteBeadsBackend(cityPath) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	}
	switch meta.Backend {
	case "", "dolt":
		clearProjectedBeadsBackendEnv(env)
		clearProjectedPostgresEnv(env)
		target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
		if err != nil {
			return true, err
		}
		applyCanonicalDoltTargetEnv(env, target)
		applyCanonicalDoltAuthEnv(env, cityPath, scopeRoot, target)
		mirrorBeadsDoltEnv(env)
		return true, nil
	case "doltlite":
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return true, nil
	case "postgres":
		if err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta); err != nil {
			return true, err
		}
		return true, nil
	default:
		return true, fmt.Errorf("unsupported backend %q for scope %s", meta.Backend, scopeRoot)
	}
}

func applyCityPostgresBackendEnv(env map[string]string, cityPath string) (bool, error) {
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
	case "postgres":
		if err := applyResolvedScopePostgresEnv(env, cityPath, cityPath, meta); err != nil {
			return true, err
		}
		return true, nil
	case "", "dolt", "doltlite":
		return false, nil
	default:
		return true, fmt.Errorf("unsupported backend %q for scope %s", meta.Backend, cityPath)
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

// applyResolvedScopePostgresEnv projects PG credentials and connection
// info into env. Caller guarantees meta.Backend == "postgres". The
// resolver chain in internal/pgauth supplies the password; the host,
// port, user, and database come straight from MetadataState.
//
// On resolver exhaustion returns an error wrapping
// pgauth.ErrNoPasswordResolvable; callers can match with errors.Is.
//
// On success emits a pg.credential_resolved event identifying the scope
// and the resolution tier that supplied the value (best-effort; recorder
// failures do not propagate).
func applyResolvedScopePostgresEnv(env map[string]string, cityPath, scopeRoot string, meta contract.MetadataState) error {
	if env == nil {
		return nil
	}
	clearProjectedBeadsBackendEnv(env)
	clearProjectedDoltEnv(env)
	mirrorBeadsDoltEnv(env)
	clearProjectedPostgresEnv(env)
	endpoint := pgauth.Endpoint{
		Host: meta.PostgresHost,
		Port: meta.PostgresPort,
		User: meta.PostgresUser,
	}
	// Scope projection clears inherited PG keys first, so credential
	// resolution intentionally starts at process and file-backed sources.
	resolved, err := pgauth.ResolveFromEnv(nil, scopeRoot, endpoint)
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
	emitPostgresCredentialResolved(cityPath, scopeRoot, meta, resolved.Source)
	return nil
}

// emitPostgresCredentialResolved records a pg.credential_resolved event
// describing the scope and the resolution tier that supplied the password.
// Best-effort: recorder failures (file unreachable, JSONL write error) do
// not propagate to the caller. The payload deliberately omits the password
// value (asserted by TestPostgresEventOmitsPassword).
func emitPostgresCredentialResolved(cityPath, scopeRoot string, meta contract.MetadataState, source pgauth.Source) {
	scopeKind, scopeName := scopeKindAndName(cityPath, scopeRoot)
	subject := "city/" + scopeName
	if scopeKind == "rig" {
		subject = "rigs/" + scopeName
	}
	payload := pgauth.PostgresCredentialResolvedPayload{
		ScopeKind: scopeKind,
		ScopeName: scopeName,
		Source:    source.String(),
		Host:      meta.PostgresHost,
		Port:      meta.PostgresPort,
		User:      meta.PostgresUser,
	}
	if _, loaded := postgresCredentialResolvedSeen.LoadOrStore(postgresCredentialResolvedKey(cityPath, payload), struct{}{}); loaded {
		return
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Marshal of a flat-string struct should never fail; if it
		// does, there is no useful payload to record.
		return
	}
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return
	}
	defer rec.Close() //nolint:errcheck // best-effort: emission must not surface I/O errors
	rec.Record(events.Event{
		Type:    events.PostgresCredentialResolved,
		Actor:   eventActor(),
		Subject: subject,
		Payload: payloadBytes,
	})
}

// scopeKindAndName returns ("city", <city-name>) when scopeRoot is the
// city itself, or ("rig", <rig-name>) when scopeRoot is a rig under or
// outside the city. Path comparison is best-effort lexical equality after
// filepath.Clean; callers tolerate ambiguity (the recorded event remains
// useful even if a non-canonical path snaps to the rig branch).
func scopeKindAndName(cityPath, scopeRoot string) (string, string) {
	if filepath.Clean(scopeRoot) == filepath.Clean(cityPath) {
		return "city", filepath.Base(filepath.Clean(cityPath))
	}
	return "rig", filepath.Base(filepath.Clean(scopeRoot))
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

func postgresMetadataForScope(cityPath, scopeRoot string) (contract.MetadataState, bool, error) {
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil {
		return contract.MetadataState{}, false, fmt.Errorf("loading metadata for scope %s: %w", scopeRoot, err)
	}
	if ok {
		switch meta.Backend {
		case "postgres":
			return meta, true, nil
		case "dolt":
			return contract.MetadataState{}, false, nil
		}
	}
	if samePath(scopeRoot, cityPath) {
		return contract.MetadataState{}, false, nil
	}
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil {
		return contract.MetadataState{}, false, fmt.Errorf("resolving config for scope %s: %w", scopeRoot, err)
	}
	if resolved.Kind != contract.ScopeConfigAuthoritative || resolved.State.EndpointOrigin != contract.EndpointOriginInheritedCity {
		return contract.MetadataState{}, false, nil
	}
	cityMeta, cityOK, cityErr := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(cityPath))
	if cityErr != nil {
		return contract.MetadataState{}, false, fmt.Errorf("loading city metadata for inherited scope %s: %w", scopeRoot, cityErr)
	}
	if !cityOK || cityMeta.Backend != "postgres" {
		return contract.MetadataState{}, false, nil
	}
	return cityMeta, true, nil
}

// scopeBackendIsPostgres returns true when the scope's MetadataState
// has Backend == "postgres" or when the scope inherits a city-level
// Postgres backend. On any read error returns false for best-effort callers
// that do not need to make recovery decisions.
func scopeBackendIsPostgres(cityPath, scopeRoot string) bool {
	_, ok, err := postgresMetadataForScope(cityPath, scopeRoot)
	if err != nil {
		return false
	}
	return ok
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
	beadsExecCommandRunnerWithEnv             = beads.ExecCommandRunnerWithEnv
	processEnvSnapshotExcludingNativeDoltOpen = beads.ProcessEnvSnapshotExcludingNativeDoltOpen
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
	for _, key := range projectedDoltEnvKeys {
		delete(env, key)
	}
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

func clearProjectedPostgresEnv(env map[string]string) {
	for _, key := range projectedPostgresEnvKeys {
		delete(env, key)
	}
}

func ensureProjectedPostgresEnvExplicit(env map[string]string) {
	for _, key := range projectedPostgresEnvKeys {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
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
		return target, true, nil
	}

	if port := recoveredManagedDoltPort(); port != "" {
		return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
	}
	if allowRecovery {
		if err := healthBeadsProvider(cityPath); err == nil {
			resetRecoveryCache()
			if port := recoveredManagedDoltPort(); port != "" {
				return contract.DoltConnectionTarget{Host: defaultManagedDoltHost, Port: port}, true, nil
			}
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
)

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
		ensureProjectedPostgresEnvExplicit(env)
		runner := beadsExecCommandRunnerWithEnv(env)
		out, err := runner(dir, name, args...)
		if name != "bd" {
			return out, err
		}
		// PG-backed scopes never invoke managed-Dolt recovery. A transport
		// error gets wrapped with an operator-facing hint and surfaced; gc
		// does not manage external PG endpoints.
		if err != nil {
			meta, ok, classifyErr := postgresMetadataForScope(cityPath, dir)
			if classifyErr != nil {
				return out, fmt.Errorf("classifying scope backend (bd error: %w): %w", err, classifyErr)
			}
			if ok {
				return out, fmt.Errorf("postgres at %s:%s: gc does not manage external PG endpoints (no managed recovery attempted): %w", meta.PostgresHost, meta.PostgresPort, err)
			}
		}
		if err == nil && scopeBackendIsPostgres(cityPath, dir) {
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
		retryEnv, retryEnvErr := envFn(dir)
		if retryEnvErr != nil {
			return nil, retryEnvErr
		}
		ensureProjectedDoltEnvExplicit(retryEnv)
		ensureProjectedPostgresEnvExplicit(retryEnv)
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
		clearProjectedPostgresEnv(env)
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

func cityPostgresProjectionErrorCanBeBypassed(cityPath string, err error) bool {
	if err == nil || !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		return false
	}
	_, ok, metaErr := postgresMetadataForScope(cityPath, cityPath)
	return metaErr == nil && ok
}

func bdRuntimeEnvForRigWithError(cityPath string, cfg *config.City, rigPath string) (map[string]string, error) {
	env, cityErr := bdRuntimeEnvWithError(cityPath)
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
	rigDoltlite := scopeBackendIsDoltlite(cityPath, rigPath)
	cityDoltlite := scopeBackendIsDoltlite(cityPath, cityPath)
	if rigDoltlite || (cityDoltlite && !scopeOverridesCityBackend(cityPath, rigPath)) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if err := applyResolvedRigDoltEnv(env, cityPath, rigPath, explicitRig, true); err != nil {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		mirrorBeadsDoltEnv(env)
		if isRecoverableManagedDoltEnvError(err) {
			return env, nil
		}
		return env, err
	}
	if cityErr != nil && (!cityPostgresProjectionErrorCanBeBypassed(cityPath, cityErr) || !rigRuntimeEnvIndependentOfCityProjection(cityPath, rigPath, explicitRig)) {
		return env, cityErr
	}
	return env, nil
}

func nativeDoltOpenEnvForScope(cityPath string, cfg *config.City, scopeRoot string) (map[string]string, error) {
	scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
	if samePath(scopeRoot, cityPath) {
		return bdRuntimeEnvWithError(cityPath)
	}
	if cfg == nil {
		loaded, err := loadCityConfig(cityPath, io.Discard)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	}
	return bdRuntimeEnvForRigWithError(cityPath, cfg, scopeRoot)
}

func bdRuntimeEnvWithError(cityPath string) (map[string]string, error) {
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
	if scopeBackendIsDoltlite(cityPath, cityPath) {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
		mirrorBeadsDoltEnv(env)
		return env, nil
	}
	if usedPostgres, err := applyCityPostgresBackendEnv(env, cityPath); err != nil {
		clearProjectedDoltEnv(env)
		clearProjectedPostgresEnv(env)
		mirrorBeadsDoltEnv(env)
		return env, err
	} else if usedPostgres {
		return env, nil
	}
	if err := applyResolvedCityDoltEnv(env, cityPath, true); err != nil {
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
	if cityUsesBdStoreContract(cityPath) {
		source := map[string]string{"BEADS_DOLT_AUTO_START": "0"}
		applyBdContributorRoutingOptOut(source)
		applyBdCLIRemoteSyncOptOut(source)
		applyBdAutoBackupOptOut(source)
		if usedPostgres, err := applyCityPostgresBackendEnv(source, cityPath); err != nil {
			clearProjectedDoltEnv(source)
			clearProjectedPostgresEnv(source)
			mirrorBeadsDoltEnv(source)
			projectionErr = err
		} else if !usedPostgres {
			err := applyResolvedCityDoltEnv(source, cityPath, false)
			if err != nil {
				clearProjectedDoltEnv(source)
			}
		}
		keys := execProjectedBackendEnvKeys()
		keys = append(keys, "BEADS_DOLT_AUTO_START")
		for _, key := range keys {
			if value, ok := source[key]; ok {
				overrides[key] = value
			}
		}
	}
	return mergeRuntimeEnv(processEnvSnapshotExcludingNativeDoltOpen(), overrides), projectionErr
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
		"GC_POSTGRES_PASSWORD",
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
	"ORCHESTRATOR_KEY_FILE",
	"EIA_AUDIENCE",
	"EIA_SCOPES",
	"STS_MACHINE_URL",
	"STS_TOKEN_URL",
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
