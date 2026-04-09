package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// controllerState implements api.State and api.StateMutator.
// Protected by an RWMutex for hot-reload: readers take RLock,
// the controller loop takes Lock when updating cfg/sp/stores.
type controllerState struct {
	mu            sync.RWMutex
	cfg           *config.City
	sp            runtime.Provider
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
	services      workspacesvc.Registry
	extmsgSvc     *extmsg.Services
	adapterReg    *extmsg.AdapterRegistry
}

// newControllerState creates a controllerState with per-rig stores.
// BdStores are wrapped with CachingStore for in-memory reads.
func newControllerState(
	cfg *config.City,
	sp runtime.Provider,
	ep events.Provider,
	cityName, cityPath string,
) *controllerState {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cs := &controllerState{
		cfg:        cfg,
		sp:         sp,
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
		cs.cityBeadStore = wrapWithCachingStore(store, ep)
		cs.cityMailProv = newMailProvider(cs.cityBeadStore)
		svc := extmsg.NewServices(cs.cityBeadStore)
		cs.extmsgSvc = &svc
	}
	return cs
}

// wrapWithCachingStore wraps a BdStore with a CachingStore that primes
// and starts a background reconciler. Non-BdStore stores are returned as-is.
func wrapWithCachingStore(store beads.Store, ep events.Provider) beads.Store {
	bdStore, ok := store.(*beads.BdStore)
	if !ok {
		return store
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
	// Full prime runs async — backfills remaining beads for List()
	// callers (convergence reconcile, sweep, API handlers).
	go func() {
		log.Printf("caching-store: priming ...")
		if err := cs.Prime(context.Background()); err != nil {
			log.Printf("caching-store: prime FAILED: %v (reads will use bd subprocess)", err)
			return
		}
		cs.StartReconciler(context.Background())
	}()
	return cs
}

// buildStores creates bead stores for each rig in cfg.
// Mail providers are NOT built here — all mail uses the city-level store.
// Pure function of cfg — does not read or write cs fields (safe to call unlocked).
func (cs *controllerState) buildStores(cfg *config.City) map[string]beads.Store {
	provider := beadsProviderFor(cfg)
	stores := make(map[string]beads.Store, len(cfg.Rigs))

	// For the "file" provider, all rigs share the same city-level beads.json.
	var sharedFileStore beads.Store
	if provider == "file" {
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cs.cityPath, ".gc", "beads.json"))
		if err == nil {
			sharedFileStore = store
		} else {
			// Fall back to bd provider rather than opening duplicate per-rig file stores.
			fmt.Fprintf(os.Stderr, "api: failed to open shared file store: %v (falling back to bd provider)\n", err)
			provider = "bd"
		}
	}

	for _, rig := range cfg.Rigs {
		if sharedFileStore != nil {
			stores[rig.Name] = sharedFileStore
		} else {
			stores[rig.Name] = wrapWithCachingStore(
				cs.openRigStore(provider, rig.Path, rig.EffectivePrefix()),
				cs.eventProv,
			)
		}
	}
	return stores
}

// beadsProviderFor returns the bead store provider name from the given config.
// Pure function — does not read controllerState fields.
func beadsProviderFor(cfg *config.City) string {
	if v := os.Getenv("GC_BEADS"); v != "" {
		return v
	}
	if cfg.Beads.Provider != "" {
		return cfg.Beads.Provider
	}
	return "bd"
}

// openRigStore creates a bead store for a rig path using the given provider.
func (cs *controllerState) openRigStore(provider, rigPath, prefix string) beads.Store {
	if strings.HasPrefix(provider, "exec:") {
		s := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		env := citylayout.CityRuntimeEnvMap(cs.cityPath)
		env["GC_BEADS_PREFIX"] = prefix
		s.SetEnv(env)
		return s
	}
	switch provider {
	case "file":
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cs.cityPath, ".gc", "beads.json"))
		if err != nil {
			return bdStoreForRig(rigPath, cs.cityPath, cs.cfg)
		}
		return store
	default: // "bd" or unrecognized
		return bdStoreForRig(rigPath, cs.cityPath, cs.cfg)
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
	// Skip events we emitted ourselves (reconciler-detected changes).
	if evt.Actor == "cache-reconcile" {
		return
	}

	cs.mu.RLock()
	stores := make([]beads.Store, 0, len(cs.beadStores)+1)
	for _, s := range cs.beadStores {
		stores = append(stores, s)
	}
	if cs.cityBeadStore != nil {
		stores = append(stores, cs.cityBeadStore)
	}
	cs.mu.RUnlock()

	for _, store := range stores {
		if cached, ok := store.(*beads.CachingStore); ok {
			cached.ApplyEvent(evt.Type, evt.Payload)
		}
	}
}

// update replaces the config, session provider, and reopens stores.
// Stores are built outside the lock to avoid blocking readers during I/O.
func (cs *controllerState) update(cfg *config.City, sp runtime.Provider) {
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
		cityStore = wrapWithCachingStore(cityStore, cs.eventProv)
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
	return cs.editor.SetOrderOverride(config.OrderOverride{
		Name:    name,
		Rig:     rig,
		Enabled: &enabled,
	})
}

// DisableOrder creates or updates an override with enabled=false.
func (cs *controllerState) DisableOrder(name, rig string) error {
	enabled := false
	return cs.editor.SetOrderOverride(config.OrderOverride{
		Name:    name,
		Rig:     rig,
		Enabled: &enabled,
	})
}

// SuspendAgent writes suspended=true to city.toml (durable desired state).
// Uses configedit.Editor for provenance-aware edit (inline vs patch).
func (cs *controllerState) SuspendAgent(name string) error {
	return cs.editor.SuspendAgent(name)
}

// ResumeAgent clears suspended in city.toml (durable desired state).
func (cs *controllerState) ResumeAgent(name string) error {
	return cs.editor.ResumeAgent(name)
}

// SuspendRig writes suspended=true on the rig in city.toml.
func (cs *controllerState) SuspendRig(name string) error {
	return cs.editor.SuspendRig(name)
}

// ResumeRig clears suspended on the rig in city.toml.
func (cs *controllerState) ResumeRig(name string) error {
	return cs.editor.ResumeRig(name)
}

// SuspendCity sets workspace.suspended = true.
func (cs *controllerState) SuspendCity() error {
	return cs.editor.SuspendCity()
}

// ResumeCity sets workspace.suspended = false.
func (cs *controllerState) ResumeCity() error {
	return cs.editor.ResumeCity()
}

// CreateAgent adds a new agent to city.toml.
func (cs *controllerState) CreateAgent(a config.Agent) error {
	return cs.editor.CreateAgent(a)
}

// UpdateAgent partially updates an existing agent definition in city.toml.
func (cs *controllerState) UpdateAgent(name string, patch api.AgentUpdate) error {
	return cs.editor.UpdateAgent(name, configedit.AgentUpdate{
		Provider:  patch.Provider,
		Scope:     patch.Scope,
		Suspended: patch.Suspended,
	})
}

// DeleteAgent removes an agent from city.toml.
func (cs *controllerState) DeleteAgent(name string) error {
	return cs.editor.DeleteAgent(name)
}

// CreateRig adds a new rig to city.toml.
func (cs *controllerState) CreateRig(r config.Rig) error {
	return cs.editor.CreateRig(r)
}

// UpdateRig partially updates a rig in city.toml.
func (cs *controllerState) UpdateRig(name string, patch api.RigUpdate) error {
	return cs.editor.UpdateRig(name, configedit.RigUpdate{
		Path:      patch.Path,
		Prefix:    patch.Prefix,
		Suspended: patch.Suspended,
	})
}

// DeleteRig removes a rig from city.toml.
func (cs *controllerState) DeleteRig(name string) error {
	return cs.editor.DeleteRig(name)
}

// CreateProvider adds a new city-level provider to city.toml.
func (cs *controllerState) CreateProvider(name string, spec config.ProviderSpec) error {
	return cs.editor.CreateProvider(name, spec)
}

// UpdateProvider partially updates an existing city-level provider.
func (cs *controllerState) UpdateProvider(name string, patch api.ProviderUpdate) error {
	return cs.editor.UpdateProvider(name, configedit.ProviderUpdate{
		DisplayName:  patch.DisplayName,
		Command:      patch.Command,
		Args:         patch.Args,
		PromptMode:   patch.PromptMode,
		PromptFlag:   patch.PromptFlag,
		ReadyDelayMs: patch.ReadyDelayMs,
		Env:          patch.Env,
	})
}

// DeleteProvider removes a city-level provider from city.toml.
func (cs *controllerState) DeleteProvider(name string) error {
	return cs.editor.DeleteProvider(name)
}

// SetAgentPatch creates or replaces an agent patch in city.toml.
func (cs *controllerState) SetAgentPatch(patch config.AgentPatch) error {
	return cs.editor.SetAgentPatch(patch)
}

// DeleteAgentPatch removes an agent patch from city.toml.
func (cs *controllerState) DeleteAgentPatch(name string) error {
	return cs.editor.DeleteAgentPatch(name)
}

// SetRigPatch creates or replaces a rig patch in city.toml.
func (cs *controllerState) SetRigPatch(patch config.RigPatch) error {
	return cs.editor.SetRigPatch(patch)
}

// DeleteRigPatch removes a rig patch from city.toml.
func (cs *controllerState) DeleteRigPatch(name string) error {
	return cs.editor.DeleteRigPatch(name)
}

// SetProviderPatch creates or replaces a provider patch in city.toml.
func (cs *controllerState) SetProviderPatch(patch config.ProviderPatch) error {
	return cs.editor.SetProviderPatch(patch)
}

// DeleteProviderPatch removes a provider patch from city.toml.
func (cs *controllerState) DeleteProviderPatch(name string) error {
	return cs.editor.DeleteProviderPatch(name)
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
