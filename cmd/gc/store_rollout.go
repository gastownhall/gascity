package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// resolvedConditionalWritesFlags resolves the rollout flags from an
// already-loaded config for store-open threading. A nil config or a resolve
// error yields (zero, false) — the factory maps the unset mode to off with a
// defaulted marker, so a best-effort open can never RAISE enforcement. The
// loud surfaces for a resolve error are the controller boot latch and gc
// doctor; store-open helpers stay best-effort, matching their existing
// config-error tolerance. Resolution is per-process by design (the env
// break-glass is per-process; the supported whole-city change is config edit
// plus restart).
func resolvedConditionalWritesFlags(cfg *config.City) (rollout.Flags, bool) {
	if cfg == nil {
		return rollout.Flags{}, false
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{})
	if err != nil {
		return rollout.Flags{}, false
	}
	return flags, true
}

// resolvedConditionalWritesMode is the mode-only view of
// resolvedConditionalWritesFlags for store-open threading.
func resolvedConditionalWritesMode(cfg *config.City) gate.Mode {
	flags, ok := resolvedConditionalWritesFlags(cfg)
	if !ok {
		return gate.ModeUnset
	}
	return flags.BeadsConditionalWrites()
}

// lazyConditionalWritesDegradeEmitter builds the factory degrade callback for
// open paths that have no live event provider in hand (the shared CLI open
// helper and the control dispatcher). The recorder is constructed INSIDE the
// callback — which the factory latches to at most one invocation per store —
// so routine opens pay nothing and an auto-degrade still lands in the city's
// event log instead of persisting unnoticed.
func lazyConditionalWritesDegradeEmitter(cityPath, storeID string, flags rollout.Flags, resolved bool) func(beads.ConditionalWritesDegrade) {
	if !resolved || strings.TrimSpace(cityPath) == "" {
		return nil
	}
	return func(d beads.ConditionalWritesDegrade) {
		cb := conditionalWritesDegradedRecorder(openCityRecorderAt(cityPath, io.Discard), flags, storeID)
		if cb != nil {
			cb(d)
		}
	}
}

// conditionalWritesStoreID labels a store scope for the degraded event
// (matching the DESIGN examples: "city", "rig/<name>").
func conditionalWritesStoreID(scopeRoot, cityPath string) string {
	if samePath(scopeRoot, cityPath) {
		return "city"
	}
	return "rig/" + filepath.Base(scopeRoot)
}

// openControlBdStoreThroughFactory routes a control-plane store through the
// beads factory so it carries the conditional-writes stamp and, where the
// scope passes preflight, comes back as the in-process native store rather
// than a bd-forking BdStore. The control dispatcher is a long-lived serve
// loop whose ready-cache probe and prime battery run on every wake, so a
// BdStore here meant a `bd` subprocess per wake forever; the native store
// answers the same reads over one Dolt connection.
// The store comes back raw (no CachingStore/policy wrap), matching the
// control path's deliberately unwrapped handles.
func openControlBdStoreThroughFactory(scopeRoot, cityPath, provider string, cfg *config.City, openBd func() (beads.Store, error)) (beads.Store, error) {
	if store := nativeControlStores.lookup(scopeRoot); store != nil {
		return store, nil
	}
	flags, resolved := resolvedConditionalWritesFlags(cfg)
	mode := gate.ModeUnset
	if resolved {
		mode = flags.BeadsConditionalWrites()
	}
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         scopeRoot,
		CityPath:          cityPath,
		Provider:          provider,
		ConditionalWrites: mode,
		OnConditionalWritesDegraded: lazyConditionalWritesDegradeEmitter(
			cityPath, conditionalWritesStoreID(scopeRoot, cityPath), flags, resolved),
		OpenBdStore:      openBd,
		PreflightChecker: newControlPreflightChecker(cityPath, provider),
		OpenNativeStore: func() (beads.Store, error) {
			return openNativeControlStore(scopeRoot, cityPath, cfg)
		},
	})
	if err != nil {
		return nil, err
	}
	return nativeControlStores.retain(scopeRoot, result.Store), nil
}

// openNativeControlStore opens the native store for a control-plane scope
// the same way the controller's rig stores do (api_state.go openRigStore):
// the scoped Dolt env for the initial open, plus a reopen hook that
// re-resolves the CURRENT env on every reconnect. The reopen deliberately
// reloads config (cfg is nil) because the dispatcher process can outlive a
// managed-Dolt restart or rebind by days, and the env it opened with pins
// the port as of open time.
func openNativeControlStore(scopeRoot, cityPath string, cfg *config.City) (beads.Store, error) {
	env, err := nativeDoltOpenEnvForScope(cityPath, cfg, scopeRoot)
	if err != nil {
		return nil, fmt.Errorf("project native control store env %s: %w", scopeRoot, err)
	}
	reopen := func(ctx context.Context) (beads.NativeStorage, error) {
		freshEnv, rerr := nativeDoltOpenEnvForScopeContext(ctx, cityPath, nil, scopeRoot)
		if rerr != nil {
			return nil, fmt.Errorf("re-resolve native control store env %s: %w", scopeRoot, rerr)
		}
		return beads.OpenNativeStorage(ctx, scopeRoot, freshEnv)
	}
	return beads.OpenNativeDoltStoreAt(context.Background(), scopeRoot, env, beads.WithNativeReopen(reopen))
}

// newControlPreflightChecker is a seam so tests of the control path can
// substitute a checker that never forks bd; production uses the same checker
// as every other gc store open.
var newControlPreflightChecker = newBeadsPreflightChecker

// nativeControlStores retains one native store per scope for the life of the
// process. The control-dispatcher serve loop opens the control store afresh
// for every control bead it processes (runControlDispatcherInStore) and never
// closes it; that was free with a BdStore, which holds nothing, but a native
// store is a preflight (one bd fork, one identity probe) plus a Dolt
// connection pool per open, and a pending bead is re-processed every tick.
// Only native stores are retained: bd/file/mem fallbacks stay per-open, so
// tests that reopen a temp scope under a different config are unaffected.
// Scope resolution is thereby fixed at first open per process -- the same
// restart-to-relocate rule the control-ready cache's retained backing already
// documents.
var nativeControlStores = &controlStoreRegistry{byRoot: map[string]beads.Store{}}

type controlStoreRegistry struct {
	mu     sync.Mutex
	byRoot map[string]beads.Store
}

func (r *controlStoreRegistry) lookup(scopeRoot string) beads.Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byRoot[scopeRoot]
}

// retain returns the store to hand out for scopeRoot: the store itself when
// it is the first native store opened for that scope, the already-retained
// one (closing the newcomer) when a concurrent first open lost the race, and
// the store unchanged when it is not native.
func (r *controlStoreRegistry) retain(scopeRoot string, store beads.Store) beads.Store {
	native, ok := store.(*beads.NativeDoltStore)
	if !ok {
		return store
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, exists := r.byRoot[scopeRoot]; exists {
		_ = native.CloseStore()
		return existing
	}
	r.byRoot[scopeRoot] = store
	return store
}

// conditionalWritesEventStoreKind maps internal store-kind names onto the
// beads.conditional_writes.degraded wire vocabulary
// (bd | native | sqlite-graph | caching | mem | file).
func conditionalWritesEventStoreKind(kind string) string {
	switch kind {
	case beads.BeadsStoreNameBdStore:
		return "bd"
	case beads.BeadsStoreNameNativeDoltStore:
		return "native"
	case beads.BeadsStoreNameFileStore:
		return "file"
	case "MemStore":
		return "mem"
	case "CachingStore":
		return "caching"
	case "*beads.DoltliteReadStore":
		// DoltliteReadStore only exists under the gascity_native_beads build
		// tag, so beads.conditionalStoreKind cannot name it and it arrives as
		// the %T spelling. It embeds *BdStore and its entire conditional-write
		// surface IS bd's, so on the wire it is a bd store.
		return "bd"
	default:
		return kind
	}
}

// conditionalWritesDegradedRecorder converts the beads factory's degrade
// notification into the typed beads.conditional_writes.degraded event,
// attaching what only the composition root knows: the store scope and the
// resolved mode's origin. The factory latches invocation once per store
// instance, so this cannot storm.
func conditionalWritesDegradedRecorder(rec events.Recorder, flags rollout.Flags, storeID string) func(beads.ConditionalWritesDegrade) {
	if rec == nil {
		return nil
	}
	return func(d beads.ConditionalWritesDegrade) {
		payload, err := json.Marshal(events.ConditionalWritesDegradedPayload{
			StoreID:   storeID,
			StoreKind: conditionalWritesEventStoreKind(d.StoreKind),
			Mode:      d.Mode,
			Origin:    string(flags.OriginOf(rollout.KeyBeadsConditionalWrites)),
			Reason:    d.Reason,
		})
		if err != nil {
			return
		}
		rec.Record(events.Event{
			Type:    events.BeadsConditionalWritesDegraded,
			Actor:   "gc",
			Subject: storeID,
			Message: fmt.Sprintf("conditional_writes degraded: store=%s mode=%s reason=%q", storeID, d.Mode, d.Reason),
			Payload: payload,
		})
	}
}
