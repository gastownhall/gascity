package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Observation callers are verify-only (engdocs/architecture/beads.md,
// invariant 18). A command that merely looks at a city's store inspects the
// published runtime state and the process table, and reads a store that is
// ALREADY running. It never runs the provider health -> recover ladder, never
// retries through recoverManagedBDCommand, never lets bd autostart a stopped
// proxied store, and never starts, stops or republishes a managed Dolt.
//
// This file is the one seam those commands open stores through: a classifier
// that answers "is the store running" without a lifecycle operation, a typed
// unavailable error, and the verify-only storeOpenPolicy. Callers choose
// observation or mutation at the call site by which opener they call, never by
// helper identity; the transitive guard in observation_recovery_census_test.go
// keys on those call sites.

// exitStoreUnavailable is the exit status of an observation command whose store
// is not available (EX_UNAVAILABLE). It is distinct from 1 (no work, no mail)
// and 124 (timeout).
const exitStoreUnavailable = 69

// storeUnavailableState is the stable token naming why a store is unavailable.
// "available" has no unavailable rendering.
type storeUnavailableState string

const (
	// storeStateStopped: a gc-managed scope with no publication or running:false,
	// or a proxied scope whose proxy is not running.
	storeStateStopped storeUnavailableState = "stopped"
	// storeStateStalePublication: the publication says running but the published
	// pid is not alive, is not a process gc owns, or names another data dir.
	storeStateStalePublication storeUnavailableState = "stale-publication"
	// storeStateUnreachable: the endpoint is claimed live but a bounded read
	// failed or timed out.
	storeStateUnreachable storeUnavailableState = "unreachable"
)

// storeUnavailableError reports that an observation found its store not
// available. It is matched with errors.As; Error carries the stable stderr
// substring `store unavailable: <state> scope=<label>`.
type storeUnavailableError struct {
	State storeUnavailableState
	// Scope is "city" or the rig name.
	Scope string
	// PID is the published pid of a stale publication, or zero.
	PID int
	// Detail follows the stable substring.
	Detail string
	// Cause is the read failure behind an unreachable state.
	Cause error
}

// Error renders the stable `store unavailable: <state> scope=<label>` form.
func (e *storeUnavailableError) Error() string {
	msg := fmt.Sprintf("store unavailable: %s scope=%s", e.State, e.Scope)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// namedSessionStatus is the gc status named-session value for a store that was
// not read: a store that is not running is a fact, an unreachable one is not.
func (e *storeUnavailableError) namedSessionStatus() string {
	if e.State == storeStateUnreachable {
		return "store unreachable"
	}
	return "store not running"
}

// Unwrap exposes the read failure behind an unreachable state.
func (e *storeUnavailableError) Unwrap() error { return e.Cause }

// asStoreUnavailable reports whether err is, or wraps, a storeUnavailableError.
func asStoreUnavailable(err error) (*storeUnavailableError, bool) {
	var unavailable *storeUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable, true
	}
	return nil, false
}

// observationReadTimeout is the read deadline of an observation command that
// has no outer deadline of its own. It reuses the managed hook's budget: a read
// that exceeds it is unreachable.
const observationReadTimeout = defaultHookRunTimeout

// observationScopeLabel names a scope for the unavailable diagnostic: "city"
// for the city store, otherwise the rig that owns scopeRoot.
func observationScopeLabel(cityPath, scopeRoot string, cfg *config.City) string {
	if samePath(scopeRoot, cityPath) {
		return "city"
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			if samePath(rigPath, scopeRoot) {
				return rig.Name
			}
		}
	}
	return filepath.Base(scopeRoot)
}

// classifyObservationScope answers, with no lifecycle operation, whether the
// store at scopeRoot is running. It returns nil for an available store and a
// *storeUnavailableError for a stopped or stale one. A read of an available
// store can still fail later; observationReadFailure turns that into
// unreachable.
//
// Dispatch is by ownership first:
//   - a proxied (provider-owned) scope is stopped when its proxy supervisor is
//     not running. This runs before ANY bd call, because bd autostarts a proxied
//     store's server on a read and BEADS_DOLT_AUTO_START=0 is inert there. The
//     verdict proves the supervisor, not its Dolt child (a stated residual).
//   - an external or explicit endpoint is available: gc has no lifecycle for it.
//   - a gc-managed scope is checked against the runtime publication, read-only.
//     This never calls the resolver that republishes a recovered port.
//   - GC_DOLT=skip, file and exec scopes open no gc-managed Dolt and are
//     available.
func classifyObservationScope(cityPath, scopeRoot, label string) error {
	if doctor.ProxiedStoreNotRunning(scopeRoot) {
		return &storeUnavailableError{State: storeStateStopped, Scope: label, Detail: "the proxied store's proxy is not running"}
	}
	if scopeStoreIsExternallyBoundBestEffort(cityPath, scopeRoot) {
		return nil
	}
	// GC_DOLT=skip declares that gc owns no Dolt here, so there is no
	// publication to verify.
	if gcDoltSkip() {
		return nil
	}
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		return fmt.Errorf("classify store %s: %w", label, err)
	}
	if !owned {
		return nil
	}
	return managedPublicationUnavailable(cityPath, label)
}

// managedPublicationUnavailable reads the managed Dolt publication and returns
// the unavailable verdict it implies, or nil when the published server is
// alive and ours. It checks the same facts as currentManagedDoltPort (identity,
// pid alive, process owned) but not port reachability: a pid that lives behind
// a refusing port is claimed live, and only the bounded read reports it
// unreachable. It never rewrites the publication.
func managedPublicationUnavailable(cityPath, label string) error {
	state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if errors.Is(err, fs.ErrNotExist) {
		return &storeUnavailableError{State: storeStateStopped, Scope: label, Detail: "no managed dolt runtime publication"}
	}
	if err != nil {
		return &storeUnavailableError{State: storeStateStalePublication, Scope: label, Detail: "managed dolt runtime publication is unreadable", Cause: err}
	}
	if !state.Running {
		return &storeUnavailableError{State: storeStateStopped, Scope: label, Detail: "managed dolt is not running"}
	}
	layout, ok := validDoltRuntimeStateIdentity(state, cityPath)
	if !ok {
		return &storeUnavailableError{State: storeStateStalePublication, Scope: label, PID: state.PID, Detail: fmt.Sprintf("published runtime state (pid %d) does not match this city's managed dolt", state.PID)}
	}
	if !pidAlive(state.PID) {
		return &storeUnavailableError{State: storeStateStalePublication, Scope: label, PID: state.PID, Detail: fmt.Sprintf("published pid %d is not running", state.PID)}
	}
	if !managedDoltRuntimeProcessOwned(state, layout) {
		return &storeUnavailableError{State: storeStateStalePublication, Scope: label, PID: state.PID, Detail: fmt.Sprintf("published pid %d is not this city's managed dolt process", state.PID)}
	}
	return nil
}

// observationReadFailure maps a failed read of a store that classified as
// available to the typed unreachable state when the failure is outage-shaped
// (an unreachable server or an expired deadline). Any other error is returned
// unchanged: a missing bead is not an outage.
func observationReadFailure(err error, scope string) error {
	if err == nil {
		return nil
	}
	if _, ok := asStoreUnavailable(err); ok {
		return err
	}
	if isBeadStoreUnreachable(err) || errors.Is(err, context.DeadlineExceeded) {
		return &storeUnavailableError{State: storeStateUnreachable, Scope: scope, Detail: err.Error(), Cause: err}
	}
	return err
}

// observationContext bounds an observation read that has no outer deadline.
func observationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), observationReadTimeout)
}

// openObservationStoreAt opens the store at storePath for a verify-only
// caller. A stopped or stale scope returns a *storeUnavailableError before any
// backend is touched; an available scope opens under the observation policy.
// cfg is the caller's already-loaded city config, or nil to load it.
func openObservationStoreAt(ctx context.Context, storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	result, err := openObservationStoreResultAt(ctx, storePath, cityPath, cfg)
	if err != nil {
		return nil, err
	}
	return result.Store, nil
}

// openObservationStoreResultAt is openObservationStoreAt returning the full
// open result.
func openObservationStoreResultAt(ctx context.Context, storePath, cityPath string, cfg *config.City) (beads.StoreOpenResult, error) {
	runtimeCityPath := cityPath
	if runtimeCityPath == "" {
		runtimeCityPath = cityForStoreDir(storePath)
	}
	scopeRoot := resolveStoreScopeRoot(runtimeCityPath, storePath)
	if err := classifyObservationScope(runtimeCityPath, scopeRoot, observationScopeLabel(runtimeCityPath, scopeRoot, cfg)); err != nil {
		return beads.StoreOpenResult{}, err
	}
	result, err := openStoreResultWithPolicy(observationStoreOpenPolicy(observationOpenContext(ctx)), storePath, cityPath, cfg, gate.ModeUnset, false, false, false, true, nil)
	if err != nil {
		return beads.StoreOpenResult{}, observationReadFailure(err, observationScopeLabel(runtimeCityPath, scopeRoot, cfg))
	}
	result.Store = splitStrictWorkView(result.Store)
	return result, nil
}

// openObservationInfraSource opens the city's work store for the storage
// gate's born-split check, under the observation policy. The store outlives
// this call (the check closes it), so it carries no deadline of its own.
// Overridden by tests.
var openObservationInfraSource = func(cityPath string) (beads.Store, error) {
	return openObservationStoreAt(context.Background(), cityPath, cityPath, nil)
}

// observationStoreOpenPolicy is the verify-only storeOpenPolicy. Every env it
// builds forbids recovery, its bd runner has no managed-retry wrapper, and its
// bd subprocesses are bound to ctx.
func observationStoreOpenPolicy(ctx context.Context) storeOpenPolicy {
	return storeOpenPolicy{
		observation: true,
		preflight: func(cityPath, provider string, cfg *config.City) contract.PreflightChecker {
			return newBeadsPreflightCheckerWithContext(cityPath, provider, cfg, observationPreflightBDContextReader(ctx, cityPath, cfg))
		},
		openBd: func(storePath, cityPath string, cfg *config.City, _ bool, native beads.NativeTransportMode) (beads.Store, error) {
			return openObservationBdStore(ctx, storePath, cityPath, cfg, native)
		},
		openExec: func(provider, scopeRoot, cityPath string, cfg *config.City) (beads.Store, error) {
			return openExecStoreWithProjectedEnv(provider, scopeRoot, cityPath, cfg, bdRuntimeEnvForRigWithErrorNoRecovery, bdRuntimeEnvWithErrorNoRecovery)
		},
		nativeEnv: observationNativeOpenEnv,
	}
}

// observationNativeOpenEnv builds the env of a direct native open without the
// recovery ladder, for the open and for its reconnect hook.
func observationNativeOpenEnv(ctx context.Context, cityPath string, cfg *config.City, scopeRoot string, longLived bool) (map[string]string, error) {
	scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
	var env map[string]string
	var err error
	if samePath(scopeRoot, cityPath) {
		env, err = bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	} else {
		if cfg == nil {
			loaded, loadErr := loadCityConfig(cityPath, io.Discard)
			if loadErr != nil {
				return nil, loadErr
			}
			cfg = loaded
		}
		env, err = bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, scopeRoot, false)
	}
	if err != nil {
		return env, err
	}
	applyNativeOpenSchemaDecision(env, cityPath, cfg)
	if longLived {
		return env, nil
	}
	return nativeDoltCliPoolCap(env), nil
}

// observationBdRunner returns the verify-only bd command runner for a scope: a
// ctx-bound exec runner over a no-recovery env, with no managed-retry wrapper.
// A complete external storage binding delegates to the workspace-pinned bd,
// which projects and recovers nothing.
func observationBdRunner(ctx context.Context, cityPath, scopeRoot string, cfg *config.City) (beads.CommandRunner, error) {
	var env map[string]string
	var err error
	if samePath(scopeRoot, cityPath) {
		complete, bindErr := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
		if bindErr != nil {
			return nil, bindErr
		}
		if complete {
			return bdContextCommandRunnerForCity(cityPath), nil
		}
		env, err = bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	} else {
		if cfg == nil {
			loaded, loadErr := loadCityConfig(cityPath, io.Discard)
			if loadErr != nil {
				return nil, loadErr
			}
			cfg = loaded
		}
		env, err = bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, scopeRoot, false)
	}
	if err != nil {
		return nil, err
	}
	env["BEADS_DIR"] = filepath.Join(scopeRoot, ".beads")
	// Belt and braces only: bd ignores it on a proxied scope, which the
	// classifier gate covers before any bd call.
	env["BEADS_DOLT_AUTO_START"] = "0"
	return beadsCommandRunnerWithContextForHostedCity(ctx, cityPath, env)
}

// openObservationBdStore opens the bd-backed store for a verify-only caller.
func openObservationBdStore(ctx context.Context, storePath, cityPath string, cfg *config.City, native beads.NativeTransportMode) (beads.Store, error) {
	if cfg == nil {
		loaded, err := loadCityConfig(cityPath, io.Discard)
		if err == nil {
			cfg = loaded
		}
	}
	runner, err := observationBdRunner(ctx, cityPath, storePath, cfg)
	if err != nil {
		return nil, err
	}
	store := beads.NewBdStoreWithPrefix(
		storePath,
		withBdReadMemo(cityPath, runner),
		issuePrefixForScope(storePath, cityPath, cfg),
		bdStoreOptionsForConfig(cfg)...,
	)
	return withDoltliteReadOptimization(storePath, store, native), nil
}

// observationPreflightBDContextReader reads `bd context --json` through the
// verify-only runner.
func observationPreflightBDContextReader(ctx context.Context, cityPath string, cfg *config.City) func(scope string) (contract.PreflightBDContext, error) {
	return preflightBDContextReaderWith(func(scope string) (beads.CommandRunner, error) {
		return observationBdRunner(ctx, cityPath, resolveStoreScopeRoot(cityPath, scope), cfg)
	})
}

// openObservationStoreForCity returns a store factory for a verify-only caller
// holding the city's loaded config (nil to load it). Each scope is classified
// before any backend is touched; the stores outlive the call, so they carry no
// deadline of their own and the caller's check budgets bound the reads.
func openObservationStoreForCity(cityPath string, cfg *config.City) func(string) (beads.Store, error) {
	return func(dirPath string) (beads.Store, error) {
		return openObservationStoreAt(context.Background(), dirPath, cityPath, cfg)
	}
}

// openObservationStoreResultForCity is openObservationStoreForCity for a
// caller that needs the native/fallback selection diagnostic.
func openObservationStoreResultForCity(cityPath string, cfg *config.City) func(string) (beads.StoreOpenResult, error) {
	return func(dirPath string) (beads.StoreOpenResult, error) {
		return openObservationStoreResultAt(context.Background(), dirPath, cityPath, cfg)
	}
}

// newObservationSessionProvider builds the session provider for a verify-only
// caller. Its route table reads the session beads once; readSnapshot false
// skips that read (the provider then routes without bead history), which is
// what a caller does after its preflight found the store unreachable. The read
// is verify-only and bounded by statusSessionSnapshotTimeout.
func newObservationSessionProvider(cfg *config.City, cityPath string, readSnapshot bool) (runtime.Provider, error) {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	var snapshot *sessionBeadSnapshot
	if readSnapshot {
		readCtx, cancel := context.WithTimeout(context.Background(), statusSessionSnapshotTimeout)
		defer cancel()
		snapshot = loadProviderSessionSnapshotWith(ctx, func(path string) (beads.Store, error) {
			return openObservationStoreAt(readCtx, path, cityPath, cfg)
		}, observationSessionStore)
	}
	return withSessionProviderConstructionContext(newSessionProviderFromContext(ctx, snapshot))
}

// observationSessionName is cliSessionName for a verify-only caller: the
// session name derives from the city's session beads when the store is
// available, and from the template alone when it is stopped, stale or
// unreachable. It never starts or recovers the store.
func observationSessionName(cityPath, cityName, agentName, sessionTemplate string) string {
	if strings.TrimSpace(cityPath) == "" {
		return sessionName(nil, cityName, agentName, sessionTemplate)
	}
	ctx, cancel := observationContext()
	defer cancel()
	store, err := openObservationStoreAt(ctx, cityPath, cityPath, nil)
	if err != nil {
		return sessionName(nil, cityName, agentName, sessionTemplate)
	}
	defer func() { _ = closeBeadStoreHandle(store) }()
	return sessionName(observationSessionStore(store, nil, cityPath), cityName, agentName, sessionTemplate)
}
