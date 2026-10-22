package main

// Installing the guarded rig store (rig_namespace_store.go) at the two seams
// that open rig stores, and the pieces around it:
//
//   - The controller: controllerState.openRigStore returns policy(guard(engine))
//     for a guarded rig, and wrapWithCachingStoreFeed rebuilds it as
//     policy(guard(cache)) with the event filter below on the cache's
//     notifications. The resolver behind every guard is the city's one
//     rigNamespaceSessions, fed from the city's sessions store only.
//   - The shared by-path open body (openStoreResultAtForCityScoped): a scope
//     whose store is a guarded rig's, found by store identity rather than by
//     path, returns policy(guard(engine)), fresh or borrowed from the
//     invocation memo.
//
// Either seam resolves a scope's guarded rig by store identity
// (guardedRigForScope), so an unguarded rig that shares a guarded rig's store
// is refused too, and refuses a guarded rig whose store is not its own.

import (
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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// guardRigStore installs the guard for ns under the store's bead-policy
// layer: policy(x) becomes policy(guard(x)), keeping the policy layer's
// config and ownership, and a store with no policy layer becomes guard(x). A
// nil namespace returns store unchanged (the rig is not guarded), and so does
// no store (beads.IsNilStore: nil, or a typed nil that would panic on its
// first call); a store already guarded is returned as it is.
func guardRigStore(store beads.Store, ns *rigNamespace) beads.Store {
	if beads.IsNilStore(store) || ns == nil {
		return store
	}
	base, policy, underPolicy := unwrapBeadPolicyStore(store)
	if _, _, already := unwrapRigNamespaceStore(base); already {
		return store
	}
	guarded := newRigNamespaceStore(base, ns)
	if !underPolicy {
		return guarded
	}
	return wrapStoreWithBeadPoliciesOwnership(guarded, policy.cfg, policy.borrowed)
}

// unwrapRigNamespaceStore returns the store under a guard and the guard's
// namespace, or store itself and ok=false when store is not a guard.
func unwrapRigNamespaceStore(store beads.Store) (beads.Store, *rigNamespace, bool) {
	g, ok := store.(*rigNamespaceStore)
	if !ok || g == nil {
		return store, nil, false
	}
	return g.inner, g.ns, true
}

// rigNamespaceOfStore returns the namespace guarding store, looking under its
// bead-policy layer, or nil when store is not guarded.
func rigNamespaceOfStore(store beads.Store) *rigNamespace {
	base, _, _ := unwrapBeadPolicyStore(store)
	if _, ns, ok := unwrapRigNamespaceStore(base); ok {
		return ns
	}
	return nil
}

// errRigNamespaceSessionsUnavailable is the lookup failure while this
// city's sessions store is not open (yet) in this process. Rows that need a
// session record stay Foreign and reads report a partial result.
var errRigNamespaceSessionsUnavailable = errors.New("this city's sessions store is not open")

// ---------------------------------------------------------------------------
// The controller's resolver.

// rigNamespaceSessions is one city's own-session resolver for its guarded
// rig stores in the controller. Every guard of the city holds it, so a
// reload swaps the resolver underneath without rebuilding the guards. It is
// built from newControllerSessionResolver over:
//
//   - the session snapshot the city runtime last loaded from its sessions
//     store (publishSnapshot), never a census that also reads rig stores;
//   - the city's sessions store (setSessionStore), the same store, as the
//     controller's sessions handle, captured as a value so a lookup takes no
//     controller lock;
//   - the session id prefixes the city mints pool session ids under.
//
// It is never shared between cities.
type rigNamespaceSessions struct {
	cityPath string
	snap     atomic.Pointer[sessionBeadSnapshot]

	mu       sync.Mutex
	store    beads.SessionStore
	prefixes []string
	warned   bool

	res atomic.Pointer[citySessionResolver]
}

var _ ownSessionResolver = (*rigNamespaceSessions)(nil)

func newRigNamespaceSessions(cityPath string) *rigNamespaceSessions {
	h := &rigNamespaceSessions{cityPath: cityPath}
	h.res.Store(h.build(beads.SessionStore{}, nil))
	return h
}

func (h *rigNamespaceSessions) build(store beads.SessionStore, prefixes []string) *citySessionResolver {
	open := func() (beads.SessionStore, error) {
		if noSessionStore(store) {
			return beads.SessionStore{}, errRigNamespaceSessionsUnavailable
		}
		return store, nil
	}
	return newControllerSessionResolver(h.cityPath, h.snapshot, open, prefixes...)
}

// snapshot returns the last published snapshot, or a nil index (never a
// typed nil) before the first.
func (h *rigNamespaceSessions) snapshot() sessionRecordIndex {
	if s := h.snap.Load(); s != nil {
		return s
	}
	return nil
}

// publishSnapshot records the session snapshot the city runtime loaded from
// its sessions store.
func (h *rigNamespaceSessions) publishSnapshot(s *sessionBeadSnapshot) {
	if h == nil || s == nil {
		return
	}
	h.snap.Store(s)
}

// setSessionStore points the resolver at the city's sessions store and the
// session id prefixes, building a new resolver (and dropping every
// remembered answer) when either changed. The store is compared below its
// handle (sameSessionStore): a reload re-mints the handles over the same
// store, and that must keep the memo.
func (h *rigNamespaceSessions) setSessionStore(store beads.SessionStore, prefixes []string) {
	if h == nil {
		return
	}
	prefixes = normalizeSessionIDPrefixes(prefixes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if sameSessionStore(store, h.store) && equalStringSlices(prefixes, h.prefixes) {
		return
	}
	h.store, h.prefixes = store, prefixes
	h.res.Store(h.build(store, prefixes))
}

// beginPass ends the resolver's error backoff; the controller calls it once
// per pass.
func (h *rigNamespaceSessions) beginPass() {
	if h == nil {
		return
	}
	h.res.Load().beginPass()
}

func (h *rigNamespaceSessions) lookupOwnSession(v string) (ownSessionRecord, bool, error) {
	if h == nil {
		return ownSessionRecord{}, false, errRigNamespaceSessionsUnavailable
	}
	return h.res.Load().lookupOwnSession(v)
}

func (h *rigNamespaceSessions) lookupOwnSessionsByName(name string, accept func(ownSessionRecord) bool) ([]ownSessionRecord, error) {
	if h == nil {
		return nil, errRigNamespaceSessionsUnavailable
	}
	return h.res.Load().lookupOwnSessionsByName(name, accept)
}

// warnNoMintPrefixOnce reports, once, a city with a guarded rig whose
// sessions store has no id prefix to mint pinned pool session ids under: its
// pool sessions are then never admitted by provenance.
func (h *rigNamespaceSessions) warnNoMintPrefixOnce(stderr io.Writer, cityName string) {
	if h == nil || stderr == nil {
		return
	}
	h.mu.Lock()
	if h.warned {
		h.mu.Unlock()
		return
	}
	h.warned = true
	h.mu.Unlock()
	fmt.Fprintf(stderr, "api: city %q has a rig with federation = \"rig\" but its sessions store mints no pinned pool session ids; pool sessions of the rig's agents will not be admitted, so give its pools a rig alias or use a sessions store with an id prefix\n", cityName) //nolint:errcheck // best-effort stderr
}

// sameSessionStore reports whether a and b are one physical store
// (beads.SameStore, below the handles), or both no store.
func sameSessionStore(a, b beads.SessionStore) bool {
	if noSessionStore(a) || noSessionStore(b) {
		return noSessionStore(a) && noSessionStore(b)
	}
	return beads.SameStore(a, b)
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Resolvers for the shared open body.

var rigNamespaceResolvers struct {
	mu sync.Mutex
	// controllers holds each controller city's resolver, by city path.
	controllers map[string]*rigNamespaceSessions
	// cli holds each city's one-shot resolver, by city path, in a process
	// that runs no controller.
	cli map[string]*citySessionResolver
	// controllerProcess counts the controllers this process runs (the
	// supervisor, a foreground controller). While it is non-zero the process
	// never builds a one-shot resolver: a city without a registered
	// controller resolver gets one that admits no session.
	controllerProcess int
}

// markRigNamespaceControllerProcess records that this process runs a
// controller until the returned func is called.
func markRigNamespaceControllerProcess() (done func()) {
	rigNamespaceResolvers.mu.Lock()
	rigNamespaceResolvers.controllerProcess++
	rigNamespaceResolvers.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			rigNamespaceResolvers.mu.Lock()
			rigNamespaceResolvers.controllerProcess--
			rigNamespaceResolvers.mu.Unlock()
		})
	}
}

// registerRigNamespaceSessions makes h the resolver every guarded store this
// process opens for cityPath uses, and returns the func that drops the
// registration again if it is still h.
func registerRigNamespaceSessions(cityPath string, h *rigNamespaceSessions) func() {
	key := filepath.Clean(cityPath)
	if strings.TrimSpace(cityPath) == "" || h == nil {
		return func() {}
	}
	rigNamespaceResolvers.mu.Lock()
	if rigNamespaceResolvers.controllers == nil {
		rigNamespaceResolvers.controllers = make(map[string]*rigNamespaceSessions)
	}
	rigNamespaceResolvers.controllers[key] = h
	rigNamespaceResolvers.mu.Unlock()
	return func() {
		rigNamespaceResolvers.mu.Lock()
		if rigNamespaceResolvers.controllers[key] == h {
			delete(rigNamespaceResolvers.controllers, key)
		}
		rigNamespaceResolvers.mu.Unlock()
	}
}

// unavailableSessionResolver admits no session and says why.
type unavailableSessionResolver struct{ err error }

func (u unavailableSessionResolver) lookupOwnSession(string) (ownSessionRecord, bool, error) {
	return ownSessionRecord{}, false, u.err
}

func (u unavailableSessionResolver) lookupOwnSessionsByName(string, func(ownSessionRecord) bool) ([]ownSessionRecord, error) {
	return nil, u.err
}

// rigNamespaceOpenCityStore opens the city's sessions store for a one-shot
// resolver: the session-class handle over the city's own bead store
// (cliStores), never a rig store. Assigned in init: the open body it calls
// installs guards through rigNamespaceSessionsForOpen, a
// package-initialization cycle otherwise.
var rigNamespaceOpenCityStore func(cityPath string, cfg *config.City) (beads.SessionStore, error)

func init() {
	rigNamespaceOpenCityStore = func(cityPath string, cfg *config.City) (beads.SessionStore, error) {
		cityStore, err := openStoreAtForCityWithConfig(cityPath, cityPath, cfg)
		if err != nil {
			return beads.SessionStore{}, err
		}
		if cityStore == nil {
			return beads.SessionStore{}, errors.New("no city store")
		}
		return cliStores(cityPath, cityStore).Sessions(), nil
	}
}

// rigNamespaceCityResolver is the resolver of a guarded store opened by path
// for one city. It holds no resolver of its own: every lookup asks the
// registry, so a controller resolver registered (or rebuilt) after the store
// was opened is the one that answers, and no guard keeps a stale one.
type rigNamespaceCityResolver struct {
	cityPath string
	cfg      *config.City
}

var _ ownSessionResolver = rigNamespaceCityResolver{}

func (r rigNamespaceCityResolver) lookupOwnSession(v string) (ownSessionRecord, bool, error) {
	return r.current().lookupOwnSession(v)
}

func (r rigNamespaceCityResolver) lookupOwnSessionsByName(name string, accept func(ownSessionRecord) bool) ([]ownSessionRecord, error) {
	return r.current().lookupOwnSessionsByName(name, accept)
}

// current returns the city's controller resolver when one is registered;
// in a controller process without one, a resolver that admits no session;
// otherwise the city's one-shot resolver, built once per process.
func (r rigNamespaceCityResolver) current() ownSessionResolver {
	key := filepath.Clean(r.cityPath)
	rigNamespaceResolvers.mu.Lock()
	defer rigNamespaceResolvers.mu.Unlock()
	if h := rigNamespaceResolvers.controllers[key]; h != nil {
		return h
	}
	if rigNamespaceResolvers.controllerProcess > 0 {
		return unavailableSessionResolver{err: fmt.Errorf("city %s: %w (its controller has not registered its resolver)", r.cityPath, errRigNamespaceSessionsUnavailable)}
	}
	if res := rigNamespaceResolvers.cli[key]; res != nil {
		return res
	}
	cityPath, cfg := r.cityPath, r.cfg
	res := newCityCLISessionResolver(cityPath, cfg, func() (beads.SessionStore, error) {
		return rigNamespaceOpenCityStore(cityPath, cfg)
	})
	if rigNamespaceResolvers.cli == nil {
		rigNamespaceResolvers.cli = make(map[string]*citySessionResolver)
	}
	rigNamespaceResolvers.cli[key] = res
	return res
}

// rigNamespaceSessionsForOpen returns the resolver a guarded store opened by
// path in this process uses for cityPath (rigNamespaceCityResolver).
func rigNamespaceSessionsForOpen(cityPath string, cfg *config.City) ownSessionResolver {
	return rigNamespaceCityResolver{cityPath: cityPath, cfg: cfg}
}

// rigNamespaceForOpen returns the namespace guarding the store scopeRoot
// opens for cityPath, or nil when that store is no guarded rig's
// (guardedRigForScope, which also refuses aliased stores).
func rigNamespaceForOpen(cfg *config.City, cityPath, scopeRoot string) (*rigNamespace, error) {
	if !cityHasGuardedRig(cfg) {
		return nil, nil
	}
	return rigNamespaceForScope(cfg, cityPath, scopeRoot, rigNamespaceSessionsForOpen(cityPath, cfg))
}

// guardStoreOpenResult installs the guard for the scope's namespace, if any,
// on an opened store.
func guardStoreOpenResult(result beads.StoreOpenResult, cfg *config.City, cityPath, scopeRoot string) (beads.StoreOpenResult, error) {
	ns, err := rigNamespaceForOpen(cfg, cityPath, scopeRoot)
	if err != nil {
		return beads.StoreOpenResult{}, err
	}
	result.Store = guardRigStore(result.Store, ns)
	return result, nil
}

// ---------------------------------------------------------------------------
// The event ingress filter.

// rigNamespaceEventFilter drops a guarded rig cache's bead notifications
// whose row is Foreign, so another city's changes to the shared store wake
// nothing in this city: no wake marks, no event-driven lane deltas, no
// event-triggered orders, no autoclose triggers. A notification whose row
// cannot be decoded, or whose classification failed, is dropped too: the
// lanes' backstops still see this city's own rows. The cache itself stays
// unfiltered.
func rigNamespaceEventFilter(ns *rigNamespace, onChange cacheOnChange) cacheOnChange {
	if ns == nil || onChange == nil {
		return onChange
	}
	var undecodable atomic.Bool
	return func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
		switch eventType {
		case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
			b, ok := beads.DecodeBeadEventPayload(payload)
			if !ok {
				if !undecodable.Swap(true) {
					log.Printf("rig %q: dropping a %s notification for %s whose row could not be decoded (federation = \"rig\")", ns.rigName(), eventType, beadID)
				}
				return
			}
			if ns.classify(b) == rowForeign {
				return
			}
		}
		onChange(source, eventType, beadID, runID, sessionID, stepID, dependsOnStepIDs, payload)
	}
}

// warnRigNamespaceOpenRefused reports a guarded rig the controller runs
// without.
func warnRigNamespaceOpenRefused(err error) {
	fmt.Fprintf(os.Stderr, "api: %v (running without this rig)\n", err) //nolint:errcheck // best-effort stderr
}
