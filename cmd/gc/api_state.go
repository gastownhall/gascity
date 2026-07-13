package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/emergency"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/usage"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// controllerState implements api.State, api.StateMutator, and
// api.ConfigWriteSerializer.
// Protected by an RWMutex for hot-reload: readers take RLock,
// the controller loop takes Lock when updating cfg/sp/stores.
type controllerState struct {
	mu  sync.RWMutex
	cfg *config.City
	// rawCfg is the raw (pre-expansion, site-bound) config snapshot captured
	// at the same generation as cfg. It is the basis the mutation gate uses
	// (Editor.UpdateAgent → AgentOrigin), cached here so provenance reads
	// (pack_derived on GET /agents) agree with the 409 gate without
	// re-parsing city.toml per request. Refreshed on every cfg swap; left
	// at its prior value if a refresh load fails so the read never falls
	// back to a nil-raw heuristic on a transient error.
	rawCfg                 *config.City
	sp                     runtime.Provider
	cacheCtx               context.Context
	beadStores             map[string]beads.Store
	cityBeadStore          beads.Store // city-level store for session beads
	cityBeadsDiagnostic    *beads.BeadsDiagnostic
	cityMailProv           mail.Provider // city-level mail provider (all mail is city-scoped)
	eventProv              events.Provider
	usageSink              usage.Sink
	editor                 *configedit.Editor
	cityName               string
	cityPath               string
	version                string
	startedAt              time.Time
	storeMetadataSignature string
	ct                     crashTracker  // nil if crash tracking disabled
	pokeCh                 chan struct{} // nil when poke is not available; triggers immediate reconciler tick
	configDirty            *atomic.Bool  // optional dirty flag shared with the reconciler reload path
	services               workspacesvc.Registry
	extmsgSvc              *extmsg.Services
	adapterReg             *extmsg.AdapterRegistry
	maintenanceLoop        *supervisor.StoreMaintenanceLoop // nil when [maintenance.dolt] enabled=false
	updateMu               sync.Mutex                       // serializes rebuild+swap so stale reloads cannot overtake newer mutations
	beadEventStartSeq      uint64

	// emergencyCh receives emergency.Record values from the gc emergency
	// subsystem. startEmergencyEventRelay drains this channel and mirrors
	// each record into the city event log as an emergency.signaled event.
	// Nil when the emergency relay is not configured.
	emergencyCh chan emergency.Record

	// True after an API config mutation refreshes controller state ahead of the
	// runtime reload loop. Runtime reloads from older revisions are ignored
	// until the loop observes and applies the same or a newer on-disk config.
	configMutationPending atomic.Bool
	pendingConfigRev      string
}

var controllerStateInitRigDirIfReady = initDirIfReady

var beadEventWatcherRetryDelay = time.Second

// newControllerStateOpenCityStore opens the city-level bead store for
// newControllerState. Test code can swap this to return an in-memory store
// and skip spawning managed dolt (~12s per call).
var newControllerStateOpenCityStore = openCityStoreResultAt

// controllerStateOpenRigStoreAtForCity routes controller rig stores through
// the same native-selection factory as direct city/rig store opens. Tests swap
// this seam to avoid opening real native Dolt handles.
var controllerStateOpenRigStoreAtForCity = beads.OpenStoreAtForCity

// controllerStateStoreCloseDelay gives handlers that already captured a store
// reference a short drain window before reload closes replaced backings.
var controllerStateStoreCloseDelay = 250 * time.Millisecond

type configMutationSnapshot struct {
	cityPath  string
	files     map[string][]byte
	existed   map[string]bool
	agentTree *fsys.TreeSnapshot
}

// newControllerState creates a controllerState with per-rig stores.
// BdStores are wrapped with CachingStore for in-memory reads.
func newControllerState(
	ctx context.Context,
	cfg *config.City,
	sp runtime.Provider,
	ep events.Provider,
	cityName, cityPath string,
) *controllerState {
	if ctx == nil {
		ctx = context.Background()
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	var beadEventStartSeq uint64
	if ep != nil {
		if seq, err := ep.LatestSeq(); err == nil {
			beadEventStartSeq = seq
		}
	}
	cs := &controllerState{
		cfg:               cfg,
		sp:                sp,
		cacheCtx:          ctx,
		eventProv:         ep,
		usageSink:         usageSinkForCity(cfg, cityPath),
		editor:            configedit.NewEditor(fsys.OSFS{}, tomlPath),
		cityName:          cityName,
		cityPath:          cityPath,
		version:           version,
		startedAt:         time.Now(),
		adapterReg:        extmsg.NewAdapterRegistry(),
		beadEventStartSeq: beadEventStartSeq,
	}
	cs.beadStores = cs.buildStores(cfg)
	// Capture the initial raw config snapshot so provenance reads before the
	// first reload still use the gate's basis. nil is tolerated: RawConfig
	// lazily retries on the first read.
	cs.rawCfg = cs.loadRawSnapshot()
	// Open city-level store for session beads and mail (best-effort).
	if opened, err := newControllerStateOpenCityStore(cityPath); err != nil {
		fmt.Fprintf(os.Stderr, "api: city bead store: %v (session/mail endpoints disabled)\n", err)
	} else {
		store := opened.Store
		cs.cityBeadStore = wrapWithCachingStore(ctx, store, ep, true)
		cs.cityBeadsDiagnostic = diagnosticPtr(opened.Diagnostic)
		cs.cityMailProv = newCityMailProvider(cs.cityBeadStore, cfg, cityPath, ep)
		svc := extmsg.NewServices(cs.cityBeadStore)
		cs.extmsgSvc = &svc
	}
	cs.storeMetadataSignature = storeMetadataSignature(cityPath, cfg)
	return cs
}

// wrapWithCachingStore wraps store in an in-memory read cache. When
// backgroundRefresh is true the cache fully primes and runs a continuous
// reconcile loop (the steady-state cost: one bd subprocess per cycle per scope).
// When false the cache only pre-primes active beads synchronously — enough for
// on-demand reads — and skips both the async full prime and the reconcile loop.
// Suspended rigs pass false: they spawn no agents, so nothing writes locally and
// a continuously refreshed cache buys nothing; reconciling every suspended rig
// every cycle is what pegs the supervisor (gastownhall/gascity #1978 follow-up).
func wrapWithCachingStore(ctx context.Context, store beads.Store, ep events.Provider, backgroundRefresh bool) beads.Store {
	baseStore, policyStore, policyWrapped := unwrapBeadPolicyStore(store)
	if baseStore == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var recorder events.Recorder
	if ep != nil {
		recorder = ep
	}
	onChange := func(eventType, beadID, runID, sessionID, stepID string, payload json.RawMessage) {
		if recorder != nil {
			recorder.Record(events.Event{
				Type:      eventType,
				Actor:     "cache-reconcile",
				Subject:   beadID,
				RunID:     runID,
				SessionID: sessionID,
				StepID:    stepID,
				Payload:   payload,
			})
		}
	}
	cs := beads.NewCachingStore(baseStore, onChange)
	// Pre-prime active beads synchronously (~1-2s, indexed queries).
	// Loads open + in_progress beads — enough for the startup path
	// (adoption, session snapshot, desired state) so the city can
	// reach "ready" without waiting for the full prime.
	if err := cs.PrimeActive(); err != nil {
		log.Printf("caching-store: pre-prime failed: %v", err)
	}
	// No cancellable ctx, or caller opted out of background refresh (suspended
	// rig): serve from the synchronous pre-prime only, no async prime/reconcile.
	if ctx.Done() == nil || !backgroundRefresh {
		if policyWrapped {
			return wrapStoreWithBeadPolicies(cs, policyStore.cfg)
		}
		return cs
	}
	// Full prime runs async — backfills remaining beads for List()
	// callers (convergence reconcile, sweep, API handlers).
	go primeThenStartReconciler(ctx, cs, os.Getenv("GC_AGENT"))
	if policyWrapped {
		return wrapStoreWithBeadPolicies(cs, policyStore.cfg)
	}
	return cs
}

// primeThenStartReconciler runs the async full prime and then arms the
// watchdog reconciler. The reconciler starts even when the prime fails:
// its periodic full scan loads the same snapshot a successful prime
// would and promotes the cache to live, so a transient prime failure at
// startup heals on the next reconcile cycle. Without this, one failed
// prime left the store serving its PrimeActive-era snapshot for the
// life of the controller — kept fresh only by event-bus writes — so
// storage-level state created before a restart (e.g. routed pool work
// feeding scale-check demand) stayed invisible until something else
// touched the bead. Only shutdown (ctx canceled) skips the reconciler.
func primeThenStartReconciler(ctx context.Context, cs *beads.CachingStore, agentID string) {
	log.Printf("caching-store: priming ...")
	if err := cs.Prime(ctx); err != nil {
		log.Printf("caching-store: prime FAILED: %v (reads use bd subprocess until the reconciler converges)", err)
	}
	if ctx.Err() != nil {
		return
	}
	cs.StartReconciler(ctx, beads.WithStaggerAuto(), agentID)
}

// buildStores creates bead stores for each rig in cfg.
// Mail providers are NOT built here — all mail uses the city-level store.
// Does not read or write mutable cs fields (safe to call unlocked); reads
// the runtime suspension state file to gate per-rig cache refresh.
func (cs *controllerState) buildStores(cfg *config.City) map[string]beads.Store {
	cityProvider := rawBeadsProviderForScope(cs.cityPath, cs.cityPath)
	suspState := loadSuspensionStateBestEffort(cs.cityPath)
	stores := make(map[string]beads.Store, len(cfg.Rigs))

	var sharedLegacyFileStore beads.Store
	var sharedLegacyCachedStore beads.Store
	if cityProvider == "file" && !fileStoreUsesScopedRoots(cs.cityPath) {
		store, err := openCompatibleFileStore(cs.cityPath, cs.cityPath)
		if err == nil {
			sharedLegacyFileStore = wrapStoreWithBeadPolicies(store, cfg)
		}
	}

	for _, rig := range cfg.Rigs {
		// Unbound rigs (declared in city.toml but missing a .gc/site.toml
		// binding) have an empty rig.Path. resolveStoreScopeRoot would
		// alias them to the city scope, silently routing rig-scoped API
		// traffic to the city store. Skip them so the API reports no
		// store for the rig and operators notice the unbound state.
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		scopeRoot := resolveStoreScopeRoot(cs.cityPath, rig.Path)
		scopeProvider := rawBeadsProviderForScope(scopeRoot, cs.cityPath)
		store := beads.Store(nil)
		if sharedLegacyFileStore != nil && scopeProvider == "file" && !scopeUsesFileStoreContract(scopeRoot) {
			// Legacy file mode aliases every rig to the same backing store, so
			// the cache handle must be shared too for immediate cross-rig reads.
			if sharedLegacyCachedStore == nil {
				sharedLegacyCachedStore = wrapWithCachingStore(cs.cacheCtx, sharedLegacyFileStore, cs.eventProv, true)
			}
			stores[rig.Name] = sharedLegacyCachedStore
			continue
		}
		store = cs.openRigStore(scopeProvider, rig.Name, scopeRoot, rig.EffectivePrefix(), cfg)
		stores[rig.Name] = wrapWithCachingStore(cs.cacheCtx, store, cs.eventProv, rigStoreBackgroundRefresh(suspState, rig))
	}
	return stores
}

// rigStoreBackgroundRefresh reports whether the controller should run
// the continuous cache refresh (async full prime + watchdog reconciler)
// for a rig's bead store. Suspended rigs skip it: they spawn no agents,
// so nothing writes locally and reconciling them every cycle is pure
// cost (gastownhall/gascity #1978 follow-up). Suspension here is the
// EFFECTIVE state — the runtime suspend/resume override layered over the
// rig's committable suspended_on_start default — not the deprecated raw
// [[rigs]] suspended field alone. Gating on the raw field misfires both
// ways: a rig resumed at runtime keeps refreshing only by accident of
// which config spelling it used, and a suspended_on_start rig never gets
// the skip at all.
func rigStoreBackgroundRefresh(suspState suspensionstate.State, rig config.Rig) bool {
	return !suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart())
}

// openRigStore creates a bead store for a rig path using the given provider.
func (cs *controllerState) openRigStore(provider, rigName, rigPath, prefix string, cfg *config.City) beads.Store {
	scopeRoot := resolveStoreScopeRoot(cs.cityPath, rigPath)
	openExecStore := func() (beads.Store, error) {
		s := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		env := gcExecStoreEnv(cs.cityPath, execStoreTarget{
			ScopeRoot: scopeRoot,
			ScopeKind: "rig",
			Prefix:    prefix,
			RigName:   rigName,
		}, provider)
		if execProviderNeedsScopedDoltStoreEnv(provider) {
			projected, err := bdRuntimeEnvForRigWithError(cs.cityPath, cfg, scopeRoot)
			if err != nil {
				return nil, fmt.Errorf("project rig store env %s: %w", scopeRoot, err)
			}
			copyExecProjectedBackendEnv(env, projected)
		}
		s.SetEnv(env)
		return s, nil
	}
	if strings.HasPrefix(provider, "exec:") && !providerUsesBdStoreContract(provider) {
		store, err := openExecStore()
		if err != nil {
			return unavailableStore{err: fmt.Errorf("open exec rig store %s: %w", scopeRoot, err)}
		}
		return wrapStoreWithBeadPolicies(store, cfg)
	}
	if provider == "file" {
		store, err := openCompatibleFileStore(scopeRoot, cs.cityPath)
		if err != nil {
			return unavailableStore{err: fmt.Errorf("open file rig store %s: %w", scopeRoot, err)}
		}
		return wrapStoreWithBeadPolicies(store, cfg)
	}
	result, err := controllerStateOpenRigStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:        scopeRoot,
		CityPath:         cs.cityPath,
		Provider:         provider,
		PreflightChecker: newBeadsPreflightChecker(cs.cityPath, provider),
		OpenFileStore: func() (beads.Store, error) {
			store, err := openCompatibleFileStore(scopeRoot, cs.cityPath)
			if err != nil {
				return nil, fmt.Errorf("open file rig store %s: %w", scopeRoot, err)
			}
			return store, nil
		},
		OpenBdStore: func() (beads.Store, error) {
			return bdStoreForRig(scopeRoot, cs.cityPath, cfg, prefix), nil
		},
		OpenExecStore: openExecStore,
		OpenNativeStore: func() (beads.Store, error) {
			env, err := nativeDoltOpenEnvForScope(cs.cityPath, cfg, scopeRoot)
			if err != nil {
				return nil, fmt.Errorf("project native rig store env %s: %w", scopeRoot, err)
			}
			// Reopen hook for the native read-path reconnect (see the matching
			// comment in main.go openStoreResultAtForCity): re-resolve the CURRENT
			// managed Dolt env on every reconnect so the controller's reconcile
			// scan / Get recovers a managed-Dolt hard-kill/rebind instead of
			// dialing the dead port for the whole retry budget.
			reopen := func(ctx context.Context) (beads.NativeStorage, error) {
				freshEnv, rerr := nativeDoltOpenEnvForScopeContext(ctx, cs.cityPath, cfg, scopeRoot)
				if rerr != nil {
					return nil, fmt.Errorf("re-resolve native rig store env %s: %w", scopeRoot, rerr)
				}
				return beads.OpenNativeStorage(ctx, scopeRoot, freshEnv)
			}
			return beads.OpenNativeDoltStoreAt(context.Background(), scopeRoot, env, beads.WithNativeReopen(reopen))
		},
	})
	if err != nil {
		return unavailableStore{err: fmt.Errorf("open rig store %s: %w", scopeRoot, err)}
	}
	return wrapStoreWithBeadPolicies(result.Store, cfg)
}

// startBeadEventWatcher subscribes to the event bus and feeds bead events
// to all CachingStore instances for sub-second cache freshness on agent-
// initiated bd mutations (bd hooks → gc event emit → this watcher → ApplyEvent).
func (cs *controllerState) startBeadEventWatcher(ctx context.Context) {
	ep := cs.EventProvider()
	if ep == nil {
		return
	}
	seq := cs.beadEventStartSeq
	go func() {
		for {
			watcher, err := ep.Watch(ctx, seq)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				fmt.Fprintf(os.Stderr, "api: bead event watcher: watch from seq %d: %v\n", seq, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(beadEventWatcherRetryDelay):
					continue
				}
			}
			for {
				evt, err := watcher.Next()
				if err != nil {
					_ = watcher.Close()
					break
				}
				seq = evt.Seq
				switch evt.Type {
				case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
					cs.applyBeadEventToStores(evt)
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

// startMaintenanceLoop launches the periodic Dolt store maintenance
// loop when [maintenance.dolt] enabled=true in city.toml. When the
// section is omitted or enabled=false, this is a no-op — the caller
// invokes it unconditionally so startup stays flat.
func (cs *controllerState) startMaintenanceLoop(ctx context.Context) {
	cs.mu.RLock()
	cfg := cs.cfg
	store := cs.cityBeadStore
	cityPath := cs.cityPath
	mailProv := cs.cityMailProv
	cs.mu.RUnlock()
	if cfg == nil || !cfg.Maintenance.Dolt.Enabled {
		return
	}
	deps := supervisor.StoreMaintenanceLoopDeps{
		Cfg:               cfg.Maintenance.Dolt,
		Store:             store,
		CityPath:          cityPath,
		Recorder:          cs.eventProv,
		Stderr:            os.Stderr,
		Mail:              mailProv,
		LastRunAt:         supervisor.SeedLastRunAt(cs.eventProv),
		DiskFreeBytes:     doltContainerFreeBytesFunc,
		DiskMinFreeBytes:  doltDiskMinFreeBytes(),
		DiskWarnFreeBytes: doltDiskWarnFreeBytes(),
	}
	active := deps.OpenDoltOps != nil && deps.OpenDoltBackup != nil
	// Always log the loop's startup so operators can confirm initialization
	// (and its mode) from the supervisor log, not just the observe-only case.
	fmt.Fprintln(os.Stderr, maintenanceStartupLine(cfg.Maintenance.Dolt.IntervalOrDefault(), active)) //nolint:errcheck // best-effort stderr
	loop := supervisor.NewStoreMaintenanceLoop(deps)
	// Retain the handle so the API layer can expose
	// /v0/city/{city}/maintenance/* (status reads + manual trigger)
	// without a separate wiring path.
	cs.mu.Lock()
	cs.maintenanceLoop = loop
	cs.mu.Unlock()
	go loop.Run(ctx)
}

// maintenanceStartupLine formats the one-line banner emitted when the Dolt
// store-maintenance loop launches. It always reports the schedule interval
// and whether the loop is wired for real GC ("active") or only observing
// ("observe-only") so operators can confirm initialization from the log.
func maintenanceStartupLine(interval time.Duration, active bool) string {
	mode := "active"
	if !active {
		mode = "observe-only (snapshot and DOLT_GC not yet wired)"
	}
	return fmt.Sprintf("store-maintenance: loop started interval=%s mode=%s", interval, mode)
}

// beadCloseAutocloseDispatch controls how convoy/wisp/molecule autoclose are
// dispatched after a bead.closed event. Default launches a background goroutine
// (best-effort, non-blocking). Tests swap to a synchronous call for
// deterministic assertions.
var beadCloseAutocloseDispatch = func(fn func()) { go fn() }

func (cs *controllerState) applyBeadEventToStores(evt events.Event) {
	if len(evt.Payload) == 0 {
		return
	}
	cs.mu.RLock()
	stores := cs.beadEventStoresLocked(evt)
	var storeRef string
	if evt.Type == events.BeadClosed {
		storeRef = cs.autocloseStoreRefLocked(evt.Subject)
	}
	cs.mu.RUnlock()

	for _, store := range stores {
		if cached, ok := store.(*beads.CachingStore); ok {
			cached.ApplyEvent(evt.Type, evt.Payload)
		}
	}
	if evt.Actor != "cache-reconcile" {
		cs.Poke()
	}
	if evt.Type == events.BeadClosed && evt.Subject != "" && len(stores) > 0 {
		cs.runBeadCloseAutoclose(evt.Subject, stores[0], storeRef)
	}
}

// autocloseStoreRefLocked returns the storeRef string for the store that owns
// beadID. Called under cs.mu read lock.
func (cs *controllerState) autocloseStoreRefLocked(beadID string) string {
	if cs.cfg == nil {
		return ""
	}
	cityPath := cs.cityPath
	cityName := loadedCityName(cs.cfg, cityPath)
	if prefix := config.EffectiveHQPrefix(cs.cfg); prefix != "" && strings.HasPrefix(beadID, prefix+"-") {
		return workflowStoreRefForDir(cityPath, cityPath, cityName, cs.cfg)
	}
	for _, rig := range cs.cfg.Rigs {
		if prefix := rig.EffectivePrefix(); prefix != "" && strings.HasPrefix(beadID, prefix+"-") {
			rigPath := rig.Path
			if !filepath.IsAbs(rigPath) {
				rigPath = filepath.Join(cityPath, rigPath)
			}
			return workflowStoreRefForDir(rigPath, cityPath, cityName, cs.cfg)
		}
	}
	return ""
}

// runBeadCloseAutoclose dispatches convoy/wisp/molecule autoclose for a closed
// bead via the controller's store. Replaces the shell on_close hook chain that
// spawned gc subprocesses per bead write (gastownhall/gascity#3248).
func (cs *controllerState) runBeadCloseAutoclose(beadID string, store beads.Store, storeRef string) {
	rec := events.Discard
	if cs.eventProv != nil {
		rec = cs.eventProv
	}
	// The just-closed bead is read from its owning store (store), but its
	// molecule and wisp GRAPH parents live in the graph-class store, so the
	// graph-root walks resolve through graphBeadStore() rather than assuming
	// co-residence with the closed bead. On a single-store city GraphBeadStore()
	// returns the same store, so this is identity today.
	graphStore := cs.GraphBeadStore()
	beadCloseAutocloseDispatch(func() {
		doConvoyAutocloseWith(store, rec, beadID, os.Stderr, os.Stderr)
		doWispAutocloseWith(store, beadID, os.Stderr, graphStore.Store)
		doMoleculeAutocloseWith(store, storeRef, rec, beadID, os.Stderr, graphStore.Store)
	})
}

func (cs *controllerState) beadEventStoresLocked(evt events.Event) []beads.Store {
	if id := beadEventID(evt); id != "" && cs.cfg != nil {
		if store, known := cs.beadEventConfiguredStoreLocked(id); known {
			if store == nil {
				return nil
			}
			return []beads.Store{store}
		}
	}

	stores := make([]beads.Store, 0, len(cs.beadStores)+1)
	for _, s := range cs.beadStores {
		stores = append(stores, s)
	}
	if cs.cityBeadStore != nil {
		stores = append(stores, cs.cityBeadStore)
	}
	return stores
}

func (cs *controllerState) beadEventConfiguredStoreLocked(id string) (beads.Store, bool) {
	// A configured prefix owns the id even when its store is not loaded: the
	// caller treats a (nil, true) result as "owned but absent here" and skips
	// the all-stores fallback, so nil stores are passed in as candidates.
	//
	// The candidate set is class-tagged: the city store under the HQ prefix is
	// the graph/sessions/mail/nudge/order class store, and each rig store under
	// its rig prefix is that rig's work-class store. On a single-store city these
	// all collapse to the same value, so the resolution is identical today; the
	// tagging marks where a future per-class backend would diverge. These read
	// the raw cs fields rather than the class accessors (graphBeadStore /
	// workBeadStores) because this runs under cs.mu and those accessors take the
	// same lock.
	//
	// The scan is a longest-prefix, namespace-only ("prefix-") match over the
	// configured prefixes, returning known=true when a configured prefix owns id
	// even if that prefix's store is not loaded (matchedStore stays nil). That
	// owned-but-unloaded signal is the call-site contract — the caller suppresses
	// the all-stores fallback on known — so the scan resolves against the
	// configured prefixes inline rather than each store's own IDPrefix.
	var matchedStore beads.Store
	matchedLen := -1
	match := func(prefix string, store beads.Store) {
		if prefix == "" || !strings.HasPrefix(id, prefix+"-") {
			return
		}
		if len(prefix) > matchedLen {
			matchedLen = len(prefix)
			matchedStore = store
		}
	}
	match(config.EffectiveHQPrefix(cs.cfg), cs.cityBeadStore)
	for _, rig := range cs.cfg.Rigs {
		match(rig.EffectivePrefix(), cs.beadStores[rig.Name])
	}
	return matchedStore, matchedLen >= 0
}

func beadEventID(evt events.Event) string {
	id := strings.TrimSpace(evt.Subject)
	if id == "" {
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(evt.Payload, &payload); err == nil {
			id = strings.TrimSpace(payload.ID)
		}
	}
	return id
}

// update replaces the config, session provider, and reopens stores.
// Stores are built outside the lock to avoid blocking readers during I/O.
func (cs *controllerState) update(cfg *config.City, sp runtime.Provider) {
	cs.updateMu.Lock()
	defer cs.updateMu.Unlock()

	// Build new stores outside the lock (may do file I/O / subprocess spawns).
	stores := cs.buildStores(cfg)
	storeSignature := storeMetadataSignature(cs.cityPath, cfg)
	// Capture the raw config from the same on-disk generation as cfg, outside
	// the lock (it does a TOML parse). nil signals "keep the prior snapshot".
	rawCfg := cs.loadRawSnapshot()
	// Recompute the usage sink so a changed [usage].provider takes effect on
	// reload instead of writing to the old sink until the controller restarts.
	usageSink := usageSinkForCity(cfg, cs.cityPath)
	// Reopen city-level store for session beads and mail.
	openedCityStore, err := newControllerStateOpenCityStore(cs.cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api: city bead store reload: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	cityStore := openedCityStore.Store
	cityBeadsDiagnostic := diagnosticPtr(openedCityStore.Diagnostic)
	var cityMailProv mail.Provider
	var extSvc *extmsg.Services
	if cityStore != nil {
		cityStore = wrapWithCachingStore(cs.cacheCtx, cityStore, cs.eventProv, true)
		cityMailProv = newCityMailProvider(cityStore, cfg, cs.cityPath, cs.eventProv)
		svc := extmsg.NewServices(cityStore)
		extSvc = &svc
	}

	// Swap under short critical section.
	var oldCityStore beads.Store
	var oldRigStores map[string]beads.Store
	cs.mu.Lock()
	cs.cfg = cfg
	if rawCfg != nil {
		cs.rawCfg = rawCfg
	}
	cs.sp = sp
	cs.usageSink = usageSink
	oldRigStores = cs.beadStores
	cs.beadStores = stores
	if cityStore != nil {
		oldCityStore = cs.cityBeadStore
		cs.cityBeadStore = cityStore
		cs.cityBeadsDiagnostic = cityBeadsDiagnostic
		cs.cityMailProv = cityMailProv
		cs.storeMetadataSignature = storeSignature
	}
	if extSvc != nil {
		cs.extmsgSvc = extSvc
	}
	// Keep prior non-nil store/provider if reopen fails.
	cs.mu.Unlock()
	if cityStore != nil && oldCityStore != nil && oldCityStore != cityStore {
		scheduleCloseBeadStoreHandle("city bead store", oldCityStore)
	}
	scheduleCloseReplacedBeadStoreHandles(oldRigStores, stores)
}

func scheduleCloseBeadStoreHandle(label string, store beads.Store) {
	if store == nil {
		return
	}
	closeFn := func() {
		if err := closeBeadStoreHandle(store); err != nil {
			log.Printf("api: close previous %s: %v", label, err)
		}
	}
	if controllerStateStoreCloseDelay <= 0 {
		closeFn()
		return
	}
	time.AfterFunc(controllerStateStoreCloseDelay, closeFn)
}

func closeBeadStoreHandle(store beads.Store) error {
	if store == nil {
		return nil
	}
	if base, _, ok := unwrapBeadPolicyStore(store); ok {
		return closeBeadStoreHandle(base)
	}
	if cached, ok := store.(*beads.CachingStore); ok {
		cached.StopReconciler()
		return closeBeadStoreHandle(cached.Backing())
	}
	closer, ok := store.(interface{ CloseStore() error })
	if !ok {
		return nil
	}
	return closer.CloseStore()
}

func scheduleCloseReplacedBeadStoreHandles(oldStores, newStores map[string]beads.Store) {
	if len(oldStores) == 0 {
		return
	}
	newKeys := make(map[uintptr]struct{}, len(newStores))
	for _, store := range newStores {
		if key, ok := storePointerKey(store); ok {
			newKeys[key] = struct{}{}
		}
	}
	closed := make(map[uintptr]struct{}, len(oldStores))
	for name, store := range oldStores {
		if key, ok := storePointerKey(store); ok {
			if _, reused := newKeys[key]; reused {
				continue
			}
			if _, seen := closed[key]; seen {
				continue
			}
			closed[key] = struct{}{}
		}
		scheduleCloseBeadStoreHandle(fmt.Sprintf("rig bead store %q", name), store)
	}
}

func storePointerKey(store beads.Store) (uintptr, bool) {
	value := reflect.ValueOf(store)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return 0, false
	}
	return value.Pointer(), true
}

func (cs *controllerState) updateFromRuntime(cfg *config.City, sp runtime.Provider, revision string) {
	if cs.configMutationPending.Load() {
		matchesPending, stale := cs.runtimeUpdateStatusForPendingMutation(revision)
		if stale {
			return
		}
		if matchesPending {
			if cs.runtimeUpdateDropsPendingRigs(cfg) {
				return
			}
			if cs.runtimeUpdateCanReuseCurrentStores(cfg) {
				cs.updateConfigAndProviderOnly(cfg, sp)
				cs.clearConfigMutationPending()
				return
			}
		}
	} else if cs.runtimeUpdateRevisionIsStale(revision) {
		return
	}
	if cs.runtimeUpdateCanReuseCurrentStores(cfg) {
		cs.updateConfigAndProviderOnly(cfg, sp)
		cs.clearConfigMutationPending()
		return
	}
	cs.update(cfg, sp)
	cs.clearConfigMutationPending()
}

func (cs *controllerState) updateConfigAndProviderOnly(cfg *config.City, sp runtime.Provider) {
	cs.updateMu.Lock()
	defer cs.updateMu.Unlock()

	// Recompute the usage sink so a changed [usage].provider takes effect even on
	// the store-reuse reload path.
	usageSink := usageSinkForCity(cfg, cs.cityPath)
	rawCfg := cs.loadRawSnapshot()
	cs.mu.Lock()
	cs.cfg = cfg
	if rawCfg != nil {
		cs.rawCfg = rawCfg
	}
	cs.sp = sp
	cs.usageSink = usageSink
	cs.mu.Unlock()
}

func (cs *controllerState) runtimeUpdateCanReuseCurrentStores(next *config.City) bool {
	cs.mu.RLock()
	current := cs.cfg
	cityStore := cs.cityBeadStore
	storeSignature := cs.storeMetadataSignature
	stores := make(map[string]beads.Store, len(cs.beadStores))
	for name, store := range cs.beadStores {
		stores[name] = store
	}
	cs.mu.RUnlock()

	if cityStore == nil || !sameStoreTopology(cs.cityPath, current, next) {
		return false
	}
	if storeSignature != "" && storeSignature != storeMetadataSignature(cs.cityPath, next) {
		return false
	}
	for _, rig := range next.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if stores[rig.Name] == nil {
			return false
		}
	}
	return true
}

func (cs *controllerState) storeMetadataChanged(next *config.City) bool {
	cs.mu.RLock()
	cityPath := cs.cityPath
	storeSignature := cs.storeMetadataSignature
	cs.mu.RUnlock()

	return storeSignature != "" && storeSignature != storeMetadataSignature(cityPath, next)
}

func storeMetadataSignature(cityPath string, cfg *config.City) string {
	if strings.TrimSpace(cityPath) == "" {
		return ""
	}
	var b strings.Builder
	appendScopeMetadataSignature := func(label, scopeRoot string) {
		if strings.TrimSpace(scopeRoot) == "" {
			scopeRoot = cityPath
		}
		scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
		fmt.Fprintf(&b, "%s:%s:", label, filepath.Clean(scopeRoot))
		data, err := os.ReadFile(scopeMetadataJSONPath(scopeRoot))
		switch {
		case err == nil:
			sum := sha256.Sum256(data)
			fmt.Fprintf(&b, "sha256=%x\n", sum)
		case os.IsNotExist(err):
			b.WriteString("missing\n")
		default:
			fmt.Fprintf(&b, "error=%T:%v\n", err, err)
		}
	}

	appendScopeMetadataSignature("city", cityPath)
	if cfg == nil {
		return b.String()
	}
	// The per-rig refresh gate is part of the signature: the captured
	// signature is compared against a recomputed one on reload
	// (runtimeUpdateCanReuseCurrentStores), so a runtime suspend/resume
	// flip invalidates store reuse and the next reload rebuilds stores
	// with the correct background-refresh gate.
	suspState := loadSuspensionStateBestEffort(cityPath)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		label := fmt.Sprintf("rig:%s:refresh=%t", rig.Name, rigStoreBackgroundRefresh(suspState, rig))
		appendScopeMetadataSignature(label, rig.Path)
	}
	return b.String()
}

func (cs *controllerState) runtimeUpdateDropsPendingRigs(next *config.City) bool {
	cs.mu.RLock()
	current := cs.cfg
	cs.mu.RUnlock()
	return configDropsBoundRigs(current, next)
}

func (cs *controllerState) runtimeUpdateStatusForPendingMutation(revision string) (matchesPending, stale bool) {
	pendingRev := cs.pendingConfigRevision()
	if pendingRev == "" {
		return false, true
	}
	if revision == "" {
		return false, true
	}
	if revision == pendingRev {
		return true, false
	}
	currentRev, err := cs.currentConfigRevision()
	if err != nil || currentRev != revision {
		return false, true
	}
	return false, false
}

func (cs *controllerState) runtimeUpdateRevisionIsStale(revision string) bool {
	if revision == "" {
		return false
	}
	currentRev, err := cs.currentConfigRevision()
	return err != nil || currentRev != revision
}

func (cs *controllerState) pendingConfigRevision() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.pendingConfigRev
}

func (cs *controllerState) currentConfigRevision() (string, error) {
	if cs.cityPath == "" {
		return "", nil
	}
	_, revision, err := cs.loadCurrentConfigSnapshot()
	if err != nil {
		return "", fmt.Errorf("loading current city config: %w", err)
	}
	return revision, nil
}

func (cs *controllerState) markConfigMutationPending(revision string) {
	cs.mu.Lock()
	cs.pendingConfigRev = revision
	cs.mu.Unlock()
	cs.configMutationPending.Store(true)
}

func (cs *controllerState) clearConfigMutationPending() {
	cs.mu.Lock()
	cs.pendingConfigRev = ""
	cs.mu.Unlock()
	cs.configMutationPending.Store(false)
}

type storeTopologyRig struct {
	path   string
	prefix string
}

func sameStoreTopology(cityPath string, current, next *config.City) bool {
	if current == nil || next == nil {
		return false
	}
	if strings.TrimSpace(current.Beads.Provider) != strings.TrimSpace(next.Beads.Provider) {
		return false
	}
	if strings.TrimSpace(current.Mail.Provider) != strings.TrimSpace(next.Mail.Provider) {
		return false
	}
	if config.EffectiveHQPrefix(current) != config.EffectiveHQPrefix(next) {
		return false
	}
	currentRigs := storeTopologyRigs(cityPath, current.Rigs)
	nextRigs := storeTopologyRigs(cityPath, next.Rigs)
	if len(currentRigs) != len(nextRigs) {
		return false
	}
	for name, currentRig := range currentRigs {
		if nextRig, ok := nextRigs[name]; !ok || nextRig != currentRig {
			return false
		}
	}
	return true
}

func storeTopologyRigs(cityPath string, rigs []config.Rig) map[string]storeTopologyRig {
	result := make(map[string]storeTopologyRig, len(rigs))
	for _, rig := range rigs {
		path := strings.TrimSpace(rig.Path)
		if path != "" {
			path = resolveStoreScopeRoot(cityPath, path)
		}
		result[rig.Name] = storeTopologyRig{
			path:   path,
			prefix: rig.EffectivePrefix(),
		}
	}
	return result
}

func configDropsBoundRigs(current, next *config.City) bool {
	if current == nil || next == nil {
		return false
	}
	nextRigPaths := make(map[string]string, len(next.Rigs))
	for _, rig := range next.Rigs {
		nextRigPaths[rig.Name] = strings.TrimSpace(rig.Path)
	}
	for _, rig := range current.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if nextRigPaths[rig.Name] == "" {
			return true
		}
	}
	return false
}

// --- api.State implementation ---

// MaintenanceLoop exposes the Dolt store maintenance loop to the API
// layer, returning nil when [maintenance.dolt] is disabled. The
// concrete *supervisor.StoreMaintenanceLoop satisfies
// api.MaintenanceProvider directly.
func (cs *controllerState) MaintenanceLoop() api.MaintenanceProvider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.maintenanceLoop == nil {
		return nil
	}
	return cs.maintenanceLoop
}

// Config returns the current city config snapshot.
func (cs *controllerState) Config() *config.City {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

// SessionProvider returns the current session provider.
func (cs *controllerState) SessionProvider() runtime.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.sp
}

// BeadStore returns the bead store for a rig (by name).
func (cs *controllerState) BeadStore(rig string) beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.beadStores[rig]
}

// BeadStores returns all rig names and their stores, including the HQ city store.
func (cs *controllerState) BeadStores() map[string]beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	// Return a copy to avoid races.
	m := make(map[string]beads.Store, len(cs.beadStores)+1)
	// Include the HQ (city-level) bead store so the /v0/beads endpoint
	// returns beads from the city root, not just from external rigs.
	if cs.cityBeadStore != nil {
		m[cs.cityName] = cs.cityBeadStore
	}
	for k, v := range cs.beadStores {
		m[k] = v
	}
	return m
}

// MailProvider returns the city-level mail provider.
// The rig parameter is accepted for interface compatibility but ignored —
// all mail is city-scoped.
func (cs *controllerState) MailProvider(_ string) mail.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cityMailProv
}

// MailProviders returns the city-level mail provider keyed by city name.
// All mail is city-scoped so there is at most one provider.
func (cs *controllerState) MailProviders() map[string]mail.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.cityMailProv == nil {
		return map[string]mail.Provider{}
	}
	return map[string]mail.Provider{cs.cityName: cs.cityMailProv}
}

// EventProvider returns the event provider.
func (cs *controllerState) EventProvider() events.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.eventProv
}

// UsageSink returns the usage-fact sink. Never nil: usage.Discard when usage is
// disabled or unset.
func (cs *controllerState) UsageSink() usage.Sink {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.usageSink == nil {
		return usage.Discard
	}
	return cs.usageSink
}

// CityName returns the city name.
func (cs *controllerState) CityName() string {
	return cs.cityName
}

// CityPath returns the city root directory.
func (cs *controllerState) CityPath() string {
	return cs.cityPath
}

// Version returns the GC binary version string.
func (cs *controllerState) Version() string {
	return cs.version
}

// StartedAt returns when the controller was started.
func (cs *controllerState) StartedAt() time.Time {
	return cs.startedAt
}

// IsQuarantined reports whether an agent is quarantined by the crash tracker.
func (cs *controllerState) IsQuarantined(sessionName string) bool {
	cs.mu.RLock()
	ct := cs.ct
	cs.mu.RUnlock()
	if ct == nil {
		return false
	}
	return ct.isQuarantined(sessionName, time.Now())
}

// ClearCrashHistory removes in-memory crash tracking for a session.
func (cs *controllerState) ClearCrashHistory(sessionName string) {
	cs.mu.RLock()
	ct := cs.ct
	cs.mu.RUnlock()
	if ct == nil {
		return
	}
	ct.clearHistory(sessionName)
}

// loadRawSnapshot loads the raw (pre-expansion, site-bound) config using the
// same basis as the mutation gate (Editor.UpdateAgent → AgentOrigin), so a
// cached snapshot drives provenance reads that must agree with the
// ErrPackDerived/409 gate. Returns nil on any load error; callers treat nil as
// "keep the previous snapshot" rather than poisoning the cache. Does no
// locking — call it outside cs.mu (it does TOML I/O).
func (cs *controllerState) loadRawSnapshot() *config.City {
	if cs.editor != nil {
		if raw, err := cs.editor.LoadRaw(); err == nil {
			return raw
		}
		return nil
	}
	// Defensive fallback for states constructed without an editor (e.g. some
	// tests): load directly. Still nil-on-error.
	tomlPath := filepath.Join(cs.cityPath, "city.toml")
	raw, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil {
		return nil
	}
	return raw
}

// RawConfig returns the cached raw (pre-expansion) config for provenance
// detection. Implements api.RawConfigProvider.
//
// The snapshot is captured at every cfg swap from the same on-disk generation
// as cs.cfg, so reads are O(1) (no per-request TOML parse) and agree with the
// mutation gate. If a swap-time load failed, the prior snapshot is retained,
// and on the very first reads before any swap-time capture it falls back to a
// one-time load so provenance is never decided on a nil-raw heuristic.
func (cs *controllerState) RawConfig() *config.City {
	cs.mu.RLock()
	cached := cs.rawCfg
	cs.mu.RUnlock()
	if cached != nil {
		return cached
	}
	// First-read fallback: no snapshot captured yet. Load once and memoize so
	// the read path still uses the gate's basis rather than nil.
	raw := cs.loadRawSnapshot()
	if raw == nil {
		return nil
	}
	cs.mu.Lock()
	if cs.rawCfg == nil {
		cs.rawCfg = raw
	}
	cached = cs.rawCfg
	cs.mu.Unlock()
	return cached
}

// CityBeadStore returns the city-level bead store for session beads.
func (cs *controllerState) CityBeadStore() beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cityBeadStore
}

// ScopedStoreLike implements api.State. See the interface doc comment for
// the contract; scopedStoreLike (cmd/gc/scoped_store.go) does the actual
// unwrap-and-rebuild work, reusing the same credential/env resolution as
// every other bd-CLI store this package constructs.
func (cs *controllerState) ScopedStoreLike(ctx context.Context, existing beads.Store) (beads.Store, error) {
	cs.mu.RLock()
	cityPath := cs.cityPath
	cfg := cs.cfg
	cs.mu.RUnlock()
	return scopedStoreLike(ctx, cityPath, cfg, existing)
}

// NudgesBeadStore returns the store backing the nudge-queue shadow beads. At the
// default backend resolveNudgesStore returns cityBeadStore, so this is byte-identical
// to CityBeadStore; when [beads.classes.nudges] is relocated it returns the per-class
// store. cs.eventProv is the recorder (an events.Recorder), matching how the city mail
// store is wired (newCityMailProvider), so relocated writes through this store emit
// bead.* exactly like the controller's own nudge writes. The result is wrapped in the
// strongly-typed beads.NudgesStore so the nudges class is statically visible to callers;
// the wrapper carries the same underlying store value, so runtime behavior is unchanged.
func (cs *controllerState) NudgesBeadStore() beads.NudgesStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.NudgesStore{Store: resolveNudgesStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// SessionsBeadStore returns the store backing session-class beads. At the default
// backend resolveSessionStore returns cityBeadStore, so this is byte-identical to
// CityBeadStore; when [beads.classes.sessions] is relocated it returns the per-class
// store. cs.eventProv is the recorder, matching the nudges/mail wiring, so relocated
// session writes emit bead.* exactly like the controller's own session writes. The
// result is wrapped in the strongly-typed beads.SessionStore so the session class is
// statically visible to callers; the wrapper carries the same underlying store value,
// so runtime behavior is unchanged.
func (cs *controllerState) SessionsBeadStore() beads.SessionStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.SessionStore{Store: resolveSessionStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// GraphBeadStore returns the store backing graph-class beads. At the default backend
// resolveGraphStore returns cityBeadStore, so this is byte-identical to CityBeadStore;
// when [beads.classes.graph] is relocated it returns the dedicated graph store at the
// legacy .gc/beads.sqlite location (or the gcg Postgres schema). cs.eventProv is
// passed for signature parity with the other accessors but is ignored by
// resolveGraphStore: the graph store stays event-silent, matching the prior Router
// graph leg. The result is wrapped in the strongly-typed beads.GraphStore so the
// graph class is statically visible to callers; the wrapper carries the same
// underlying store value, so runtime behavior is unchanged.
func (cs *controllerState) GraphBeadStore() beads.GraphStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.GraphStore{Store: resolveGraphStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// CityBeadsDiagnostic returns the city-level bead store selection diagnostic.
func (cs *controllerState) CityBeadsDiagnostic() *beads.BeadsDiagnostic {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.cityBeadsDiagnostic == nil {
		return nil
	}
	diag := *cs.cityBeadsDiagnostic
	return &diag
}

// Orders scans formula layers and returns active orders.
func (cs *controllerState) Orders() []orders.Order {
	return orders.FilterEnabled(cs.OrdersAll())
}

// OrdersAll scans formula layers and returns all orders after overrides.
func (cs *controllerState) OrdersAll() []orders.Order {
	cs.mu.RLock()
	cfg := cs.cfg
	cs.mu.RUnlock()

	allAA, err := orderdiscovery.ScanAll(cs.cityPath, cfg, orderdiscovery.ScanOptions{
		OnRigScanError: func(_ string, _ error) error {
			return nil
		},
		OnOverrideError: func(err error) error {
			log.Printf("gc api: applying order overrides for %s: %v", cs.cityPath, err)
			return nil
		},
		OnValidateError: func(orderName string, err error) error {
			log.Printf("gc api: skipping invalid order %s for %s: %v", orderName, cs.cityPath, err)
			return nil
		},
		ValidateOrder: validateOrderExecEnvOverrides,
	})
	if err != nil {
		return nil
	}

	return allAA
}

// --- api.StateMutator implementation ---

// EnableOrder creates or updates an override with enabled=true.
func (cs *controllerState) EnableOrder(name, rig string) error {
	enabled := true
	return cs.mutateAndPoke(func() error {
		return cs.editor.MergeOrderOverride(config.OrderOverride{
			Name:    name,
			Rig:     rig,
			Enabled: &enabled,
		})
	})
}

// DisableOrder creates or updates an override with enabled=false.
func (cs *controllerState) DisableOrder(name, rig string) error {
	enabled := false
	return cs.mutateAndPoke(func() error {
		return cs.editor.MergeOrderOverride(config.OrderOverride{
			Name:    name,
			Rig:     rig,
			Enabled: &enabled,
		})
	})
}

// SerializeConfigWrite runs fn under the same per-city mutation lock the
// configedit.Editor uses for agent/rig/provider/formula edits. The HTTP pack
// import add/remove handlers write pack.toml, packs.lock, and sometimes
// city.toml outside the Editor callback shape, so routing them through this
// shared lock keeps concurrent config writers from interleaving and losing an
// update or desyncing the manifest and lockfile.
func (cs *controllerState) SerializeConfigWrite(fn func() error) error {
	return cs.editor.Do(fn)
}

var _ api.ConfigWriteSerializer = (*controllerState)(nil)

// SuspendAgent writes suspended=true to durable agent config.
// Uses configedit.Editor for provenance-aware edit (inline vs discovered vs patch).
func (cs *controllerState) SuspendAgent(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.SuspendAgent(name)
	})
}

// ResumeAgent clears suspended in durable agent config.
func (cs *controllerState) ResumeAgent(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.ResumeAgent(name)
	})
}

// SuspendRig writes suspended=true on the rig in city.toml.
func (cs *controllerState) SuspendRig(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.SuspendRig(name)
	})
}

// ResumeRig clears suspended on the rig in city.toml.
func (cs *controllerState) ResumeRig(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.ResumeRig(name)
	})
}

// SuspendCity sets workspace.suspended = true.
func (cs *controllerState) SuspendCity() error {
	if err := cs.mutateAndPoke(func() error {
		return cs.editor.SuspendCity()
	}); err != nil {
		return err
	}
	if cs.eventProv != nil {
		cs.eventProv.Record(events.Event{Type: events.CitySuspended, Actor: "gc"})
	}
	return nil
}

// ResumeCity sets workspace.suspended = false.
func (cs *controllerState) ResumeCity() error {
	if err := cs.mutateAndPoke(func() error {
		return cs.editor.ResumeCity()
	}); err != nil {
		return err
	}
	if cs.eventProv != nil {
		cs.eventProv.Record(events.Event{Type: events.CityResumed, Actor: "gc"})
	}
	return nil
}

// CreateAgent adds a new agent to city.toml.
func (cs *controllerState) CreateAgent(a config.Agent) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.CreateAgent(a)
	})
}

// FormulaSource returns the raw TOML of an editable city-local formula. It is a
// read, so it does not refresh or poke.
func (cs *controllerState) FormulaSource(name string) ([]byte, bool, error) {
	return cs.editor.FormulaSource(name)
}

// UpsertFormula creates or replaces a city-local formula source, then refreshes
// the config snapshot so the new formula is re-discovered (FormulaLayers is
// recomputed during composition) and pokes the reconciler. If the post-write
// refresh fails, the prior on-disk source is restored so the file write does not
// outlive a rolled-back mutation. A rollback that itself fails (a double fault)
// is joined into the returned error rather than swallowed, mirroring the
// snapshot-restore discipline in mutateAndPoke, so disk-vs-memory divergence is
// never silent. If a prior source exists but cannot be read, the mutation aborts
// before any write, since rollback would have no basis to restore it.
func (cs *controllerState) UpsertFormula(name string, content []byte) error {
	// The read-prior -> write -> refresh -> rollback sequence is not atomic across
	// concurrent editor ops (the pre-existing mutateAndPoke rollback race class):
	// a same-name racing upsert could see this rollback's delete-on-no-prior erase
	// its committed file. Very low risk on a single-operator control plane; a
	// coarse per-city mutation lock is deferred as out of scope for this change.
	prior, hadPrior, readErr := cs.editor.FormulaSource(name)
	if readErr != nil {
		// FormulaSource reports a missing source as (nil, false, nil); a non-nil
		// error means a prior source exists but is unreadable. Treating that as
		// absent would let a refresh failure delete or overwrite the only
		// restorable copy, so abort before mutating.
		return fmt.Errorf("reading prior formula %q before upsert: %w", name, readErr)
	}
	err := cs.mutateAndPoke(func() error {
		return cs.editor.UpsertFormula(name, content)
	})
	if err != nil {
		var rollbackErr error
		if hadPrior {
			rollbackErr = cs.editor.UpsertFormula(name, prior)
		} else {
			// No prior file existed, so the desired rollback post-state is
			// "absent". A brand-new write that faulted before creating the file
			// leaves nothing to delete, and DeleteFormula then returns
			// ErrNotFound — that desired state, not a rollback failure. Joining it
			// would let mutationError map the create's real infrastructure or
			// validation failure to HTTP 404, masking the true error class, so
			// treat ErrNotFound here as a satisfied rollback.
			if rb := cs.editor.DeleteFormula(name); rb != nil && !errors.Is(rb, configedit.ErrNotFound) {
				rollbackErr = rb
			}
		}
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rolling back formula %q write: %w", name, rollbackErr))
		}
	}
	return err
}

// DeleteFormula removes a city-local formula source and refreshes state. A failed
// refresh restores the prior source; a restore that itself fails is joined into
// the returned error rather than swallowed. If the prior source exists but cannot
// be read, the delete aborts before mutating, since rollback would have no basis
// to restore it.
func (cs *controllerState) DeleteFormula(name string) error {
	prior, hadPrior, readErr := cs.editor.FormulaSource(name)
	if readErr != nil {
		return fmt.Errorf("reading prior formula %q before delete: %w", name, readErr)
	}
	err := cs.mutateAndPoke(func() error {
		return cs.editor.DeleteFormula(name)
	})
	if err != nil && hadPrior {
		if rollbackErr := cs.editor.UpsertFormula(name, prior); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rolling back formula %q delete: %w", name, rollbackErr))
		}
	}
	return err
}

// WaitForAgentVisibility blocks until findAgent in the controller's hot-reloaded
// config snapshot resolves the given qualified agent name. CreateAgent already
// refreshes cs.cfg from disk, so the first check normally succeeds; the wait
// preserves the HTTP contract that a successful POST /agents response can be
// followed immediately by POST /sling against the same target.
func (cs *controllerState) WaitForAgentVisibility(ctx context.Context, qualifiedName string) error {
	return api.WaitForAgentVisibilityIn(ctx, cs.Config, qualifiedName)
}

// UpdateAgent partially updates an existing agent definition in city.toml.
func (cs *controllerState) UpdateAgent(name string, patch api.AgentUpdate) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.UpdateAgent(name, configedit.AgentUpdate{
			Provider:  patch.Provider,
			Scope:     patch.Scope,
			Suspended: patch.Suspended,
		})
	})
}

// DeleteAgent removes an agent from city.toml.
func (cs *controllerState) DeleteAgent(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteAgent(name)
	})
}

// CreateRig adds a new rig to city.toml.
func (cs *controllerState) CreateRig(r config.Rig) error {
	r = detectRigDefaultBranch(cs.cityPath, r)
	if err := cs.initializeRigStoreForCreate(r); err != nil {
		return err
	}
	return cs.mutateAndPoke(func() error {
		return cs.editor.CreateRig(r)
	})
}

func detectRigDefaultBranch(cityPath string, r config.Rig) config.Rig {
	r.DefaultBranch = strings.TrimSpace(r.DefaultBranch)
	if r.DefaultBranch != "" {
		return r
	}
	rigPath := strings.TrimSpace(r.Path)
	if rigPath == "" {
		return r
	}
	rigPath = resolveStoreScopeRoot(cityPath, rigPath)
	if _, err := os.Stat(filepath.Join(rigPath, ".git")); err != nil {
		return r
	}
	r.DefaultBranch = git.New(rigPath).ProbeDefaultBranch()
	return r
}

func (cs *controllerState) initializeRigStoreForCreate(r config.Rig) error {
	cityPath := strings.TrimSpace(cs.cityPath)
	rigPath := strings.TrimSpace(r.Path)
	if cityPath == "" || rigPath == "" {
		return nil
	}

	cs.mu.RLock()
	cfg := cs.cfg
	cs.mu.RUnlock()
	if cfg != nil {
		for _, existing := range cfg.Rigs {
			if existing.Name == r.Name {
				return fmt.Errorf("%w: rig %q", configedit.ErrAlreadyExists, r.Name)
			}
		}
	}

	scopeRoot := resolveStoreScopeRoot(cityPath, rigPath)
	if _, err := controllerStateInitRigDirIfReady(cityPath, scopeRoot, r.EffectivePrefix()); err != nil {
		return fmt.Errorf("initializing rig %q beads: %w", r.Name, err)
	}
	return nil
}

// UpdateRig partially updates a rig in city.toml.
func (cs *controllerState) UpdateRig(name string, patch api.RigUpdate) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.UpdateRig(name, configedit.RigUpdate{
			Path:          patch.Path,
			Prefix:        patch.Prefix,
			DefaultBranch: patch.DefaultBranch,
			Suspended:     patch.Suspended,
		})
	})
}

// DeleteRig removes a rig from city.toml.
func (cs *controllerState) DeleteRig(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteRig(name)
	})
}

// CreateProvider adds a new city-level provider to city.toml.
func (cs *controllerState) CreateProvider(name string, spec config.ProviderSpec) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.CreateProvider(name, spec)
	})
}

// UpdateProvider partially updates an existing city-level provider.
func (cs *controllerState) UpdateProvider(name string, patch api.ProviderUpdate) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.UpdateProvider(name, configedit.ProviderUpdate{
			DisplayName:        patch.DisplayName,
			Base:               patch.Base,
			Command:            patch.Command,
			ACPCommand:         patch.ACPCommand,
			Args:               patch.Args,
			ACPArgs:            patch.ACPArgs,
			ArgsAppend:         patch.ArgsAppend,
			PromptMode:         patch.PromptMode,
			PromptFlag:         patch.PromptFlag,
			ReadyDelayMs:       patch.ReadyDelayMs,
			Env:                patch.Env,
			OptionsSchemaMerge: patch.OptionsSchemaMerge,
			OptionsSchema:      patch.OptionsSchema,
			OptionDefaults:     patch.OptionDefaults,
		})
	})
}

// DeleteProvider removes a city-level provider from city.toml.
func (cs *controllerState) DeleteProvider(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteProvider(name)
	})
}

// SetAgentPatch creates or replaces an agent patch in city.toml.
func (cs *controllerState) SetAgentPatch(patch config.AgentPatch) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.SetAgentPatch(patch)
	})
}

// DeleteAgentPatch removes an agent patch from city.toml.
func (cs *controllerState) DeleteAgentPatch(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteAgentPatch(name)
	})
}

// SetRigPatch creates or replaces a rig patch in city.toml.
func (cs *controllerState) SetRigPatch(patch config.RigPatch) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.SetRigPatch(patch)
	})
}

// DeleteRigPatch removes a rig patch from city.toml.
func (cs *controllerState) DeleteRigPatch(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteRigPatch(name)
	})
}

// SetProviderPatch creates or replaces a provider patch in city.toml.
func (cs *controllerState) SetProviderPatch(patch config.ProviderPatch) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.SetProviderPatch(patch)
	})
}

// DeleteProviderPatch removes a provider patch from city.toml.
func (cs *controllerState) DeleteProviderPatch(name string) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.DeleteProviderPatch(name)
	})
}

func captureConfigMutationSnapshot(cityPath string) (*configMutationSnapshot, error) {
	snapshot := &configMutationSnapshot{
		cityPath: cityPath,
		files:    make(map[string][]byte),
		existed:  make(map[string]bool),
	}

	capture := func(path string) error {
		// Snapshot at the resolved symlink target: restore writes with a
		// temp-file + rename, and renaming over the unresolved path would
		// replace a symlinked config with a regular file (the ga-lurp5d
		// failure mode). Resolve-only — restores write the original bytes
		// back, so the key-loss rewrite guard does not apply.
		path, err := fsys.ResolveSymlinks(fsys.OSFS{}, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			snapshot.files[path] = data
			snapshot.existed[path] = true
		case os.IsNotExist(err):
			snapshot.existed[path] = false
		default:
			return fmt.Errorf("reading %s: %w", path, err)
		}
		return nil
	}

	cityToml, err := cityTomlRollbackPath(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, fmt.Errorf("resolving city.toml for rollback snapshot: %w", err)
	}

	for _, path := range []string{
		cityToml,
		filepath.Join(cityPath, ".gc", "site.toml"),
	} {
		if err := capture(path); err != nil {
			return nil, err
		}
	}

	agentTree, err := fsys.SnapshotTree(fsys.OSFS{}, filepath.Join(cityPath, "agents"))
	if err != nil {
		return nil, fmt.Errorf("snapshotting agent scaffolds: %w", err)
	}
	snapshot.agentTree = agentTree

	// SnapshotTree preserves a symlinked agents/<name>/agent.toml as a link
	// entry but never the bytes behind it, while the forward agent mutation
	// path (WriteLocalDiscoveredAgentSuspended / removeAgentTomlConvention)
	// writes or removes the *resolved target*. Capture the resolved-target
	// bytes here — symmetric with city.toml/site.toml above — so restore()
	// rewrites the operator's checked-out agent.toml content after the tree
	// restore re-creates the link, closing the ga-lurp5d rollback gap.
	agentsDir := filepath.Join(cityPath, "agents")
	agentEntries, err := os.ReadDir(agentsDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing agents for symlinked agent.toml snapshot: %w", err)
	}
	for _, entry := range agentEntries {
		if !entry.IsDir() {
			continue
		}
		agentTomlPath := filepath.Join(agentsDir, entry.Name(), "agent.toml")
		info, lstatErr := os.Lstat(agentTomlPath)
		if lstatErr != nil {
			if os.IsNotExist(lstatErr) {
				continue
			}
			return nil, fmt.Errorf("inspecting agents/%s/agent.toml: %w", entry.Name(), lstatErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Regular-file agent.toml content is captured and restored by the
			// tree snapshot; only symlinked targets need separate handling.
			continue
		}
		if err := capture(agentTomlPath); err != nil {
			return nil, err
		}
	}

	return snapshot, nil
}

func (s *configMutationSnapshot) restore() error {
	var restoreErr error

	if s.agentTree != nil {
		if err := s.agentTree.Restore(fsys.OSFS{}); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring agent scaffolds: %w", err))
		}
	}

	for path, existed := range s.existed {
		if !existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("removing %s: %w", path, err))
			}
			continue
		}
		if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, s.files[path], 0o644); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring %s: %w", path, err))
		}
	}

	return restoreErr
}

func (cs *controllerState) mutateAndPoke(mutate func() error) error {
	var snapshot *configMutationSnapshot
	if cs.cityPath != "" {
		var err error
		snapshot, err = captureConfigMutationSnapshot(cs.cityPath)
		if err != nil {
			return fmt.Errorf("snapshotting current city config: %w", err)
		}
	}
	if err := mutate(); err != nil {
		return err
	}
	revision, err := cs.refreshConfigSnapshot()
	if err != nil {
		if snapshot != nil {
			if restoreErr := snapshot.restore(); restoreErr != nil {
				restoreFailure := fmt.Errorf("restoring previous city config: %w", restoreErr)
				return fmt.Errorf("refreshing updated city config: %w", errors.Join(err, restoreFailure))
			}
		}
		return fmt.Errorf("refreshing updated city config: %w", err)
	}
	cs.markConfigMutationPending(revision)
	if cs.configDirty != nil {
		cs.configDirty.Store(true)
	}
	cs.Poke()
	return nil
}

func (cs *controllerState) refreshConfigSnapshot() (string, error) {
	if cs.cityPath == "" || cs.cfg == nil {
		return "", nil
	}

	nextCfg, revision, err := cs.loadCurrentConfigSnapshot()
	if err != nil {
		return "", fmt.Errorf("loading updated city config: %w", err)
	}
	if revision == "" {
		return "", errors.New("computed empty config revision")
	}

	cs.mu.RLock()
	sp := cs.sp
	cs.mu.RUnlock()
	cs.update(nextCfg, sp)
	return revision, nil
}

func (cs *controllerState) loadCurrentConfigSnapshot() (*config.City, string, error) {
	nextCfg, prov, err := loadCityConfigWithBuiltinPacks(cs.cityPath, extraConfigFiles...)
	if err != nil {
		return nil, "", err
	}
	applyFeatureFlags(nextCfg)
	applyRuntimeCityIdentity(nextCfg, cs.cityName)
	revision := config.Revision(fsys.OSFS{}, prov, nextCfg, cs.cityPath)
	return nextCfg, revision, nil
}

// Poke signals the controller to trigger an immediate reconciler tick.
// Non-blocking: if a poke is already pending, additional pokes are dropped.
func (cs *controllerState) Poke() {
	if cs.pokeCh == nil {
		return
	}
	select {
	case cs.pokeCh <- struct{}{}:
	default: // poke already pending
	}
}

// WaitForSessionCommandable waits until the controller has reconciled an async
// session create into a lifecycle state that can accept normal commands.
func (cs *controllerState) WaitForSessionCommandable(ctx context.Context, sessionID string) (session.Info, error) {
	store := cs.CityBeadStore()
	if store == nil {
		return session.Info{}, errors.New("city bead store is unavailable")
	}
	catalog, err := workerSessionCatalogWithConfig(cs.CityPath(), store, cs.SessionProvider(), cs.Config())
	if err != nil {
		return session.Info{}, err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		info, err := catalog.Get(sessionID)
		if err != nil {
			return session.Info{}, err
		}
		if info.Closed {
			return session.Info{}, fmt.Errorf("session is closed: %s", sessionID)
		}
		switch info.State {
		case session.StateActive, session.StateAwake, session.StateAsleep, session.StateSuspended, session.StateQuarantined:
			return info, nil
		case session.StateStartPending, session.StateCreating, "":
		default:
			return session.Info{}, fmt.Errorf("session %s reached non-commandable state %q", sessionID, info.State)
		}

		select {
		case <-ctx.Done():
			return session.Info{}, fmt.Errorf("session %s did not become commandable: %w", sessionID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// ServiceRegistry returns the workspace service registry.
func (cs *controllerState) ServiceRegistry() workspacesvc.Registry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.services
}

// WebhookDispatcher implements api.WebhookDispatchProvider — the H1/E0.5 dispatch
// seam the supervisor webhook receiver (E3/E6) fires verified+matched deliveries
// through. It returns an adapter that dispatches a pre-resolved order through the
// same launchResolvedDispatch → dispatchOne core the controller tick loop uses.
//
// The adapter builds a fresh, detached memoryOrderDispatcher per delivery from the
// CURRENT cfg (read under the hot-reload lock) so a webhook dispatch reflects a
// config reload without a rebuild hook and never races the reconciler's live tick
// dispatcher (cr.od, which is single-goroutine-owned by the reconcile loop and may
// be nil for a webhook-only city). The seam's Dispatch path consults no per-tick
// dispatcher state (cooldown cache, open-work gate) — it validates required params,
// writes the tracking bead, and launches dispatchOne — so a per-delivery instance
// is byte-equivalent to a long-lived one, and the order's own timeout bounds the
// async work.
func (cs *controllerState) WebhookDispatcher() orderdispatch.Dispatcher {
	return controllerWebhookDispatcher{cs: cs}
}

// controllerWebhookDispatcher adapts controllerState into orderdispatch.Dispatcher.
type controllerWebhookDispatcher struct{ cs *controllerState }

func (d controllerWebhookDispatcher) Dispatch(ctx context.Context, req orderdispatch.DispatchRequest) (orderdispatch.DispatchResult, error) {
	cs := d.cs
	cs.mu.RLock()
	cfg := cs.cfg
	var rec events.Recorder = cs.eventProv
	cs.mu.RUnlock()
	if rec == nil {
		// dispatchOne records OrderFired/Completed/Failed unconditionally; a
		// discard recorder keeps it panic-free when the city has events disabled.
		rec = events.Discard
	}
	md := newMemoryOrderDispatcher(nil, cs.cityPath, cfg, rec, os.Stderr)
	return md.Dispatch(ctx, req)
}

// ExtMsgServices returns the external messaging services.
func (cs *controllerState) ExtMsgServices() *extmsg.Services {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.extmsgSvc
}

// AdapterRegistry returns the external messaging adapter registry.
func (cs *controllerState) AdapterRegistry() *extmsg.AdapterRegistry {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.adapterReg
}
