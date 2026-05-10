package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// controllerState implements api.State and api.StateMutator.
// Protected by an RWMutex for hot-reload: readers take RLock,
// the controller loop takes Lock when updating cfg/sp/stores.
type controllerState struct {
	mu            sync.RWMutex
	cfg           *config.City
	sp            runtime.Provider
	cacheCtx      context.Context
	beadStores    map[string]beads.Store
	cityBeadStore beads.Store   // city-level store for session beads
	cityMailProv  mail.Provider // city-level mail provider (all mail is city-scoped)
	eventProv     events.Provider
	editor        *configedit.Editor
	cityName      string
	cityPath      string
	version       string
	startedAt     time.Time
	ct            crashTracker  // nil if crash tracking disabled
	pokeCh        chan struct{} // nil when poke is not available; triggers immediate reconciler tick
	configDirty   *atomic.Bool  // optional dirty flag shared with the reconciler reload path
	services      workspacesvc.Registry
	extmsgSvc     *extmsg.Services
	adapterReg    *extmsg.AdapterRegistry
	updateMu      sync.Mutex // serializes rebuild+swap so stale reloads cannot overtake newer mutations

	// True after an API config mutation refreshes controller state ahead of the
	// runtime reload loop. Runtime reloads from older revisions are ignored
	// until the loop observes and applies the same or a newer on-disk config.
	configMutationPending atomic.Bool
	pendingConfigRev      string
}

var controllerStateInitRigDirIfReady = initDirIfReady

type configMutationSnapshot struct {
	cityPath   string
	files      map[string][]byte
	existed    map[string]bool
	agentFiles map[string]struct{}
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
	cs := &controllerState{
		cfg:        cfg,
		sp:         sp,
		cacheCtx:   ctx,
		eventProv:  ep,
		editor:     configedit.NewEditor(fsys.OSFS{}, tomlPath),
		cityName:   cityName,
		cityPath:   cityPath,
		version:    version,
		startedAt:  time.Now(),
		adapterReg: extmsg.NewAdapterRegistry(),
	}
	cs.beadStores = cs.buildStores(cfg)
	// Open city-level store for session beads and mail (best-effort).
	if store, err := openCityStoreAt(cityPath); err != nil {
		fmt.Fprintf(os.Stderr, "api: city bead store: %v (session/mail endpoints disabled)\n", err)
	} else {
		cs.cityBeadStore = wrapWithCachingStore(ctx, store, ep)
		cs.cityMailProv = newMailProvider(cs.cityBeadStore)
		svc := extmsg.NewServices(cs.cityBeadStore)
		cs.extmsgSvc = &svc
	}
	return cs
}

// wrapWithCachingStore wraps a BdStore with a CachingStore that primes
// and starts a background reconciler. Non-BdStore stores are returned as-is.
func wrapWithCachingStore(ctx context.Context, store beads.Store, ep events.Provider) beads.Store {
	bdStore, ok := store.(*beads.BdStore)
	if !ok {
		return store
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var recorder events.Recorder
	if ep != nil {
		recorder = ep
	}
	onChange := func(eventType, beadID string, payload json.RawMessage) {
		if recorder != nil {
			recorder.Record(events.Event{
				Type:    eventType,
				Actor:   "cache-reconcile",
				Subject: beadID,
				Payload: payload,
			})
		}
	}
	cs := beads.NewCachingStore(bdStore, onChange)
	// Pre-prime active beads synchronously (~1-2s, indexed queries).
	// Loads open + in_progress beads — enough for the startup path
	// (adoption, session snapshot, desired state) so the city can
	// reach "ready" without waiting for the full prime.
	if err := cs.PrimeActive(); err != nil {
		log.Printf("caching-store: pre-prime failed: %v", err)
	}
	if ctx.Done() == nil {
		return cs
	}
	// Full prime runs async — backfills remaining beads for List()
	// callers (convergence reconcile, sweep, API handlers).
	go func() {
		log.Printf("caching-store: priming ...")
		if err := cs.Prime(ctx); err != nil {
			log.Printf("caching-store: prime FAILED: %v (reads will use bd subprocess)", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		cs.StartReconciler(ctx, beads.WithStaggerAuto(), os.Getenv("GC_AGENT"))
	}()
	return cs
}

// buildStores creates bead stores for each rig in cfg.
// Mail providers are NOT built here — all mail uses the city-level store.
// Pure function of cfg — does not read or write cs fields (safe to call unlocked).
func (cs *controllerState) buildStores(cfg *config.City) map[string]beads.Store {
	cityProvider := rawBeadsProviderForScope(cs.cityPath, cs.cityPath)
	stores := make(map[string]beads.Store, len(cfg.Rigs))

	var sharedLegacyFileStore beads.Store
	if cityProvider == "file" && !fileStoreUsesScopedRoots(cs.cityPath) {
		store, err := openCompatibleFileStore(cs.cityPath, cs.cityPath)
		if err == nil {
			sharedLegacyFileStore = store
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
			store = sharedLegacyFileStore
		} else {
			store = cs.openRigStore(scopeProvider, rig.Name, scopeRoot, rig.EffectivePrefix(), cfg)
		}
		stores[rig.Name] = wrapWithCachingStore(cs.cacheCtx, store, cs.eventProv)
	}
	return stores
}

// openRigStore creates a bead store for a rig path using the given provider.
func (cs *controllerState) openRigStore(provider, rigName, rigPath, prefix string, cfg *config.City) beads.Store {
	scopeRoot := resolveStoreScopeRoot(cs.cityPath, rigPath)
	if strings.HasPrefix(provider, "exec:") {
		s := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		env := gcExecStoreEnv(cs.cityPath, execStoreTarget{
			ScopeRoot: scopeRoot,
			ScopeKind: "rig",
			Prefix:    prefix,
			RigName:   rigName,
		}, provider)
		if execProviderNeedsScopedDoltStoreEnv(provider) {
			copyExecProjectedDoltEnv(env, bdRuntimeEnvForRig(cs.cityPath, cfg, scopeRoot))
		}
		s.SetEnv(env)
		return s
	}
	switch provider {
	case "file":
		store, err := openCompatibleFileStore(scopeRoot, cs.cityPath)
		if err != nil {
			return unavailableStore{err: fmt.Errorf("open file rig store %s: %w", scopeRoot, err)}
		}
		return store
	default: // "bd" or unrecognized
		return bdStoreForRig(scopeRoot, cs.cityPath, cfg, prefix)
	}
}

// startBeadEventWatcher subscribes to the event bus and feeds bead events
// to all CachingStore instances for sub-second cache freshness on agent-
// initiated bd mutations (bd hooks → gc event emit → this watcher → ApplyEvent).
func (cs *controllerState) startBeadEventWatcher(ctx context.Context) {
	ep := cs.EventProvider()
	if ep == nil {
		return
	}
	go func() {
		seq, _ := ep.LatestSeq()
		for {
			watcher, err := ep.Watch(ctx, seq)
			if err != nil {
				return
			}
			for {
				evt, err := watcher.Next()
				if err != nil {
					_ = watcher.Close()
					break
				}
				seq = evt.Seq
				switch evt.Type {
				case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
					cs.applyBeadEventToStores(evt)
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

func (cs *controllerState) applyBeadEventToStores(evt events.Event) {
	if len(evt.Payload) == 0 {
		return
	}
	cs.mu.RLock()
	stores := cs.beadEventStoresLocked(evt)
	cs.mu.RUnlock()

	for _, store := range stores {
		if cached, ok := store.(*beads.CachingStore); ok {
			cached.ApplyEvent(evt.Type, evt.Payload)
		}
	}
	if evt.Actor != "cache-reconcile" {
		cs.Poke()
	}
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
	// Reopen city-level store for session beads and mail.
	cityStore, err := openCityStoreAt(cs.cityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api: city bead store reload: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	var cityMailProv mail.Provider
	var extSvc *extmsg.Services
	if cityStore != nil {
		cityStore = wrapWithCachingStore(cs.cacheCtx, cityStore, cs.eventProv)
		cityMailProv = newMailProvider(cityStore)
		svc := extmsg.NewServices(cityStore)
		extSvc = &svc
	}

	// Swap under short critical section.
	cs.mu.Lock()
	cs.cfg = cfg
	cs.sp = sp
	cs.beadStores = stores
	if cityStore != nil {
		cs.cityBeadStore = cityStore
		cs.cityMailProv = cityMailProv
	}
	if extSvc != nil {
		cs.extmsgSvc = extSvc
	}
	// Keep prior non-nil store/provider if reopen fails.
	cs.mu.Unlock()
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

	cs.mu.Lock()
	cs.cfg = cfg
	cs.sp = sp
	cs.mu.Unlock()
}

func (cs *controllerState) runtimeUpdateCanReuseCurrentStores(next *config.City) bool {
	cs.mu.RLock()
	current := cs.cfg
	cityStore := cs.cityBeadStore
	stores := make(map[string]beads.Store, len(cs.beadStores))
	for name, store := range cs.beadStores {
		stores[name] = store
	}
	cs.mu.RUnlock()

	if cityStore == nil || !sameStoreTopology(cs.cityPath, current, next) {
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

// RawConfig returns the raw (pre-expansion) config for provenance detection.
// Implements api.RawConfigProvider.
//
// Holds cs.mu.RLock during the load to ensure the raw config is from the
// same generation as the expanded cs.cfg snapshot.
func (cs *controllerState) RawConfig() *config.City {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	tomlPath := filepath.Join(cs.cityPath, "city.toml")
	raw, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil {
		return nil
	}
	return raw
}

// CityBeadStore returns the city-level bead store for session beads.
func (cs *controllerState) CityBeadStore() beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cityBeadStore
}

// Orders scans formula layers and returns all orders.
func (cs *controllerState) Orders() []orders.Order {
	cs.mu.RLock()
	cfg := cs.cfg
	cs.mu.RUnlock()

	allAA, err := scanAllOrders(cs.cityPath, cfg, io.Discard, "gc api: order scan")
	if err != nil {
		return nil
	}

	if len(cfg.Orders.Overrides) > 0 {
		orders.ApplyOverrides(allAA, convertOverrides(cfg.Orders.Overrides)) //nolint:errcheck // best-effort
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
	return cs.mutateAndPoke(func() error {
		return cs.editor.SuspendCity()
	})
}

// ResumeCity sets workspace.suspended = false.
func (cs *controllerState) ResumeCity() error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.ResumeCity()
	})
}

// CreateAgent adds a new agent to city.toml.
func (cs *controllerState) CreateAgent(a config.Agent) error {
	return cs.mutateAndPoke(func() error {
		return cs.editor.CreateAgent(a)
	})
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
		cityPath:   cityPath,
		files:      make(map[string][]byte),
		existed:    make(map[string]bool),
		agentFiles: make(map[string]struct{}),
	}

	capture := func(path string) error {
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

	for _, path := range []string{
		filepath.Join(cityPath, "city.toml"),
		filepath.Join(cityPath, ".gc", "site.toml"),
	} {
		if err := capture(path); err != nil {
			return nil, err
		}
	}

	agentFiles, err := filepath.Glob(filepath.Join(cityPath, "agents", "*", "agent.toml"))
	if err != nil {
		return nil, fmt.Errorf("listing agent overrides: %w", err)
	}
	for _, path := range agentFiles {
		snapshot.agentFiles[path] = struct{}{}
		if err := capture(path); err != nil {
			return nil, err
		}
	}

	return snapshot, nil
}

func (s *configMutationSnapshot) restore() error {
	var restoreErr error

	currentAgentFiles, err := filepath.Glob(filepath.Join(s.cityPath, "agents", "*", "agent.toml"))
	if err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("listing current agent overrides: %w", err))
	} else {
		for _, path := range currentAgentFiles {
			if _, existed := s.agentFiles[path]; existed {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("removing %s: %w", path, err))
			}
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
		case session.StateCreating, "":
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
