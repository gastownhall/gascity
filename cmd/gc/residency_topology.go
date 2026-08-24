package main

// The cmd/gc topology constructors: opened routes as data.
//
// storeref's residency resolver is pure — it plans over a Topology and opens
// nothing. This file builds that Topology for the two cmd/gc planes: the
// one-shot CLI (which resolves its own storage funnel) and the controller
// (which opened its binding at boot).
//
// # From OPENED ROUTES, never from config
//
// This is the cityQueryTopology lesson, and it is why neither constructor reads
// [storage]. storageSplitShapeOf reads the section alone and answers "no split"
// for a city whose section was DELETED after it had already served one. That
// city's infrastructure beads are in a binding, its boot refuses, and its
// routes serve every infrastructure class from refusedClassStore. Asking config
// would build it a work-only topology whose plans read the work ledger and
// report "no work" forever; asking the ROUTES gets the refusal, which fails
// loud with the sentence that names the remedy.
//
// # Told, not deciding
//
// A constructor takes the opened work and rig stores it is handed and the
// routes this process already resolved. It does not decide which rigs are
// serving — a suspended rig is simply absent from the map it is given — and it
// does not decide whether a binding mints truthfully: it asks the store, which
// declares the namespace it mints into. The residence probe nonetheless stays
// in every plan, because retirement also needs a binding known to hold no
// relics and nothing here censuses residents. The corpus already carries the
// retired row, so the day the census ships this is a bit, not a redesign.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// StorageRefusal reports the refusal this store answers every operation with.
// It lets a topology constructor recognize a refused city from the opened
// routes without performing a read (storeref.RefusingStore).
func (s refusedClassStore) StorageRefusal() error { return s.err }

// StandingStorageRefusal marks this error as the verdict this build took about
// a CITY rather than a fault a particular read ran into
// (storeref.StandingRefusal). The resolver keys its one tolerated non-fault on
// it: a refused city still serves WORK from its work ledger, so a residence
// probe for an id no relocated class could own may skip the refusing leg.
func (standingStorageRefusal) StandingStorageRefusal() {}

// cliResidencyTopology builds the one-shot CLI's topology.
//
// work and rigs are the scope stores the command already opened — the
// constructor opens nothing. The bindings come from cliStorageRoutes, the
// memoized funnel every one-shot class resolver already takes its verdict from,
// so the CLI and the controller answer residency from the same routes.
func cliResidencyTopology(cityPath string, cfg *config.City, work beads.Store, rigs map[string]beads.Store) storeref.Topology {
	bindings, refused := cliResidencyBindings(cityPath)
	return assembleResidencyTopology(cfg, work, rigs, bindings, refused)
}

// residencyTopology builds the controller's topology from the binding it opened
// at boot, its city work store, and the rig legs it is TOLD are serving.
//
// # The suspension frame is a parameter, and that is load-bearing
//
// rigBeadStores() is every rig the runtime holds open, suspended or not, and a
// suspended rig is routinely DARK — its store is an unavailableStore whose every
// read returns the open error. Planning a census over it makes each result
// Partial, and Partial means retain-don't-reap: one suspended rig would pin the
// whole fleet's sessions against every drain and reap decision. So the serving
// set arrives from the caller that already computed it
// (buildSuspendedRigPathsForCity, threaded through the census arms) rather than
// being re-derived here: the constructor is told, it does not decide.
//
// The residence probe stays in every plan for the same reason it does
// everywhere else: the mint bit is observed, but nothing censuses a binding's
// relics, so the retirement condition's other half is never satisfied.
func (cr *CityRuntime) residencyTopology(servingRigs map[string]beads.Store) storeref.Topology {
	bindings, refused := residencyBindingsFromRoutes(cr.storageRoutes)
	return assembleResidencyTopology(cr.cfg, cr.cityBeadStore(), servingRigs, bindings, refused)
}

// residencyTopologyForCity builds a topology over the caller's own opened work
// legs and the bindings THIS process serves for cityPath.
//
// It is the constructor for the assigned-work spine, whose scans are reached
// from both planes through the same free functions — a reconciler tick and a
// one-shot `gc session close` both call the same release sweep, and neither
// carries a CityRuntime down to it. So the binding half is resolved by city
// rather than by handle: a controller REGISTERS the routes it opened at boot
// (registerResidencyRoutes) and every other caller falls back to the one-shot
// funnel. That registration is not a convenience — resolving a controller's
// bindings through the one-shot funnel would open a SECOND handle on the same
// binding root, a duplicate managed-Dolt server or a second sqlite writer,
// which is a worse bug than the blindness this closes.
func residencyTopologyForCity(cityPath string, cfg *config.City, work beads.Store, rigs map[string]beads.Store) storeref.Topology {
	if entry, ok := registeredResidencyEntry(cityPath); ok {
		bindings, refused := residencyBindingsFromRoutes(entry.routes)
		return assembleResidencyTopology(cfg, work, rigs, bindings, refused)
	}
	return cliResidencyTopology(cityPath, cfg, work, rigs)
}

// registeredCityWorkStore returns the CITY WORK store the runtime serving
// cityPath opened at boot, or nil when no runtime here serves that city.
//
// The census needs it because the store its arms are HANDED is the sessions
// leg, which on a converged split is the binding — and a census whose work leg
// is the binding is the E2 dual role: the binding stands in for the city work
// store, and a city-scope routed WORK bead is then in no leg at all (D6,
// ga-88mxz). Naming both requires the work store, and reopening it from
// cityPath would put a second handle on the work root, which is a worse bug
// than the blindness it closes — the same argument that made the binding half a
// registration rather than a funnel call.
//
// It is a func rather than a beads.Store because the runtime registers under
// the controller lock, and its controllerState (which owns the cached city
// store) is installed a few statements later. Reading the accessor at census
// time rather than at registration time means the registration cannot capture a
// nil.
func registeredCityWorkStore(cityPath string) beads.Store {
	entry, ok := registeredResidencyEntry(cityPath)
	if !ok || entry.work == nil {
		return nil
	}
	return entry.work()
}

// registerResidencyRoutes records the routes a long-running runtime opened at
// boot as this process's answer for cityPath.
//
// # Call it only while holding the controller lock
//
// The lock is what makes "one registration per city" true. A supervisor builds
// a replacement runtime — opening its own binding — BEFORE it learns whether the
// predecessor has released the lock, so registering at construction time lets a
// replacement that then LOSES the lock repoint a live city's release sweeps at a
// handle it is about to close. Registering after the lock is taken means the
// loser never registers at all, and the live runtime's registration stands
// untouched. (`runController` already holds the lock before it constructs.)
//
// # What the memo drop does and does not do
//
// dropCLIResidencyBindings clears only the DERIVED per-city binding grouping
// this file memoizes, so a later resolution re-reads the registry instead of a
// pre-registration answer. It does NOT close or invalidate the one-shot
// cliStorageRoutes funnel: a handle that funnel already opened in this process
// stays open, and the 21 non-test sites that call cliStorageRoutes directly
// keep using it. Neutralizing the funnel is not this slice's scope; keeping the spine off
// it is.
// work is the runtime's city WORK-store accessor, read lazily (see
// registeredCityWorkStore); pass nil from a caller that has none.
func registerResidencyRoutes(cityPath string, routes *storageRoutes, work func() beads.Store) {
	if cityPath == "" {
		return
	}
	key := filepath.Clean(cityPath)
	residencyRoutesMu.Lock()
	if residencyRoutesByCity == nil {
		residencyRoutesByCity = make(map[string]residencyRegistration, 1)
	}
	residencyRoutesByCity[key] = residencyRegistration{routes: routes, work: work}
	residencyRoutesMu.Unlock()
	dropCLIResidencyBindings(key)
}

// unregisterResidencyRoutes drops a runtime's registration, and ONLY its own.
// The routes it named are the runtime's to close; this just stops the spine from
// naming them.
//
// routes is the caller's own *storageRoutes, and the delete is conditional on
// the registry still holding exactly that pointer — the same
// unlink-only-if-ours discipline the supervisor uses for its socket. A
// delete-by-key would let a runtime that lost the controller lock remove a
// registration a live one had already replaced it with, and the live city's
// release sweeps would fall back to the one-shot funnel: a second handle on the
// binding root, or on a city the funnel cannot resolve, a binding-blind release
// sweep with ga-j4ob9 back and silent.
func unregisterResidencyRoutes(cityPath string, routes *storageRoutes) {
	if cityPath == "" {
		return
	}
	key := filepath.Clean(cityPath)
	residencyRoutesMu.Lock()
	if current, ok := residencyRoutesByCity[key]; ok && current.routes == routes {
		delete(residencyRoutesByCity, key)
	}
	residencyRoutesMu.Unlock()
	dropCLIResidencyBindings(key)
}

// residencyRegistration is what one long-running runtime declares about a city:
// the class routes it opened at boot, and the accessor for the city WORK store
// it supervises.
type residencyRegistration struct {
	routes *storageRoutes
	work   func() beads.Store
}

// registeredResidencyEntry reports the runtime-registered stores for a city.
// The bool distinguishes "this process serves the city and relocates nothing"
// (registered nil routes) from "no runtime here", which is what keeps a
// single-store controller off the one-shot funnel entirely.
func registeredResidencyEntry(cityPath string) (residencyRegistration, bool) {
	if cityPath == "" {
		return residencyRegistration{}, false
	}
	residencyRoutesMu.Lock()
	defer residencyRoutesMu.Unlock()
	entry, ok := residencyRoutesByCity[filepath.Clean(cityPath)]
	return entry, ok
}

var (
	residencyRoutesMu     sync.Mutex
	residencyRoutesByCity map[string]residencyRegistration
)

// cliResidencyBindingsEntry is one city's resolved bindings, computed at most
// once per process.
type cliResidencyBindingsEntry struct {
	bindings []storeref.ClassBinding
	refused  error
}

var (
	cliResidencyBindingsMu     sync.Mutex
	cliResidencyBindingsByCity map[string]*cliResidencyBindingsEntry
)

// cliResidencyBindings memoizes the binding derivation per city.
//
// The funnel underneath is already memoized, so what this adds is that the
// class-to-binding grouping — the part a by-id read would otherwise redo on
// every call — happens once per process. That is the ga-4qdfn latency fix as a
// property of the resolver rather than a short-circuit at one call site; the
// other half is the identity fast-path, where a single-store city's plan has
// one leg and performs no probe at all.
//
// The work and rig legs are NOT memoized: they are the caller's own opened
// scope stores, and a command that changed scope must get its own legs.
func cliResidencyBindings(cityPath string) ([]storeref.ClassBinding, error) {
	if cityPath == "" {
		return nil, nil
	}
	key := filepath.Clean(cityPath)
	cliResidencyBindingsMu.Lock()
	if cliResidencyBindingsByCity == nil {
		cliResidencyBindingsByCity = make(map[string]*cliResidencyBindingsEntry, 1)
	}
	entry, ok := cliResidencyBindingsByCity[key]
	cliResidencyBindingsMu.Unlock()
	if ok {
		return entry.bindings, entry.refused
	}

	bindings, refused := residencyBindingsFromRoutes(cliStorageRoutes(cityPath))

	cliResidencyBindingsMu.Lock()
	defer cliResidencyBindingsMu.Unlock()
	if existing, raced := cliResidencyBindingsByCity[key]; raced {
		return existing.bindings, existing.refused
	}
	// A nil map HERE is not the cold start the first section handles — that
	// section made it non-nil before this goroutine ever left the lock. It means
	// resetCLIResidencyBindings ran while this derivation was in flight, which
	// happens on the shutdown path of a long-lived `gc start`. Storing anyway
	// would do two wrong things at once: assign into a nil map, which panics the
	// process during its own teardown, and re-populate a retired memo with
	// bindings over stores closeCLIStorageRoutes has already closed. The answer
	// is still correct for THIS caller, so it is returned unmemoized.
	if cliResidencyBindingsByCity == nil {
		return bindings, refused
	}
	cliResidencyBindingsByCity[key] = &cliResidencyBindingsEntry{bindings: bindings, refused: refused}
	return bindings, refused
}

// cliRelocatedBinding is the one relocated class binding a city is served from.
//
// It carries the opened store rather than the storeref.ClassBinding it was
// derived from, and that is the residency boundary rather than a convenience: a
// caller handed the ClassBinding would read binding.Leg.Store, which is a
// consumer picking a store out of a plan leg — the shape the boundary check
// forbids, because a second consumer will pick a different one. The leg access
// happens here, in the file that assembles legs, exactly once.
//
// Name is the operator-facing name of the configured [storage] binding, which is
// not derivable from the ClassBinding: storeref carries a class REF
// ("class:graph+sessions+…"), describing what the binding answers for and not
// which configured binding it is. Operator text wants the latter.
type cliRelocatedBinding struct {
	Store   beads.Store
	Classes []coordclass.Class
	Name    string
}

// cliSoleClassBinding returns the single relocated class binding serving this
// city, for the callers that answer EVERY reserved prefix from ONE store.
//
// # It turns a comment into a check
//
// The by-id front door and the claim route both used to ask
// graphClassBinding — "which store serves the graph class" — and then answer
// for sessions, convoy and mail ids out of that same store. That is correct
// only because storageSplitShapeOf admits a split only when all five
// infrastructure classes name the same binding and refuses a per-class fan-out
// outright, and until now that argument lived entirely in a comment. Reading
// the grouped bindings instead makes it a runtime condition: two bindings is a
// fan-out neither caller can serve, and it says so rather than quietly picking
// the graph one and letting a sessions-class read come back truthfully absent.
//
// # The refusal is carried, not returned
//
// A city configured for a binding it has not converged on still produces a
// binding here — a refusedClassStore whose every operation returns the boot
// gate's sentence — and cliResidencyBindings reports that refusal alongside it.
// Both callers want the STORE in that case, exactly as they got it before: the
// refusal reaches them through the reads they were going to make anyway, and is
// classified there. Neither classifies it itself any more: bdByIDClassDoor.resolve
// runs storeref's plan (ga-qdt5y.18) and hookClaimClassRoute.holds runs the same
// plan over its own captured frame (ga-qdt5y.16), and the plan tolerates the
// refusal on a residence probe and surfaces it on the authority leg.
// Returning it here instead would collapse "this city cannot be served" into
// "this city relocates nothing", which sends those reads back to the work
// ledger the beads were migrated off — the exact stale-answer path the door was
// built to close.
func cliSoleClassBinding(cityPath string) (cliRelocatedBinding, bool, error) {
	bindings, _ := cliResidencyBindings(cityPath)
	switch len(bindings) {
	case 0:
		return cliRelocatedBinding{}, false, nil
	case 1:
		return cliRelocatedBinding{
			Store:   bindings[0].Leg.Store,
			Classes: bindings[0].Classes,
			Name:    cliStorageRoutes(cityPath).binding,
		}, true, nil
	default:
		return cliRelocatedBinding{}, false, fmt.Errorf(
			"this city serves its coordination classes from %d separate bindings; %s",
			len(bindings), storageSupportedTopologyStatement)
	}
}

// resetCLIResidencyBindings drops the memo wholesale. closeCLIStorageRoutes
// calls it: this grouping is DERIVED from the routes that call closes, so it
// cannot outlive them.
func resetCLIResidencyBindings() {
	cliResidencyBindingsMu.Lock()
	cliResidencyBindingsByCity = nil
	cliResidencyBindingsMu.Unlock()
}

// dropCLIResidencyBindings drops one city's memoized funnel answer.
func dropCLIResidencyBindings(key string) {
	cliResidencyBindingsMu.Lock()
	delete(cliResidencyBindingsByCity, key)
	cliResidencyBindingsMu.Unlock()
}

// residencyBindingsFromRoutes groups the relocated classes by the STORE that
// serves them — one ClassBinding per distinct store, carrying every class it
// answers for and the reserved prefixes those classes mint.
//
// Grouping by store identity rather than by class count is what lets the same
// code describe the whole split this build serves (five classes, one binding)
// and a per-class fan-out it does not (one class each). The tripwire that this
// build produces only the former lives in the constructors' test, in one place.
//
// # Why a map keyed by beads.Store is safe HERE and is not in the resolver
//
// storeref.dedupeLegs cannot key a map on a store — beads.Store is an interface,
// and an implementation whose dynamic type carries a slice, a map or a func is
// neither hashable nor ==-able, so a caller's store panics it. This map is
// confined to the CONSTRUCTORS: every value in it comes from routes.storeFor,
// which returns what storage boot opened — *SQLiteStore, *BdStore and
// *CachingStore, all pointer-typed and so reference-identified, plus
// refusedClassStore, which is a value carrying one error field.
//
// That last one is comparable rather than reference-identified, and the
// difference is visible: two refusedClassStore values built from the SAME error
// compare equal, so a refused city's five classes group into one binding even
// though storeFor handed back five separate values. That is the answer this
// build wants — a refused city is refused as a whole, and cliSoleClassBinding
// reports one binding for it rather than a five-way fan-out — but it is a
// property of the type being comparable, not of the map being identity-keyed,
// and a second value-typed route with a differing field would group differently
// for the same reason. Same confinement for relocatedGraphLegFrom's
// `binding == cityStore`. A route that ever yields a NON-comparable store makes
// both of these the resolver's bug, one file over; reuse storeref's
// identity-based set at that point.
func residencyBindingsFromRoutes(routes *storageRoutes) ([]storeref.ClassBinding, error) {
	byStore := map[beads.Store][]coordclass.Class{}
	var order []beads.Store
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() {
			continue
		}
		store, relocated := routes.storeFor(class)
		if !relocated || store == nil {
			continue
		}
		if _, seen := byStore[store]; !seen {
			order = append(order, store)
		}
		byStore[store] = append(byStore[store], class)
	}
	return residencyBindingsFor(order, byStore, routes.hasLegacyResidents)
}

// residencyBindingsFor turns a store->classes grouping into bindings, and
// reports the standing refusal when any binding is a refusing store.
//
// The body is storeref.BuildBindings, shared with the API plane. What survives
// here is the only thing that is this plane's own: which options it passes.
// storageRoutes resolves every coordination class directly, so there is no
// blind spot to correct for and the observed class set stands as observed.
//
// relics answers the boot census's question for a binding store. A nil one is
// the pessimistic answer for every store, which is what a caller holding no
// censused routes is entitled to claim.
func residencyBindingsFor(order []beads.Store, byStore map[beads.Store][]coordclass.Class, relics func(beads.Store) bool) ([]storeref.ClassBinding, error) {
	return storeref.BuildBindings(order, byStore, storeref.BindingOptions{Relics: relics})
}

// soleBindingResidency derives the bindings for a caller that holds ONE opened
// class store and no census — the shape a route constructed over a bare store is
// in, with no routes behind it to ask.
//
// It lives here rather than at that call site because this file is where legs
// are assembled, and it goes through residencyBindingsFor for the reason every
// other constructor does: Prefixes, Classes, Ref and both retirement bits then
// come from storeref.BuildBindings, so a topology built from a bare store cannot
// disagree with one built from a city about what a binding's namespaces are.
//
// The relics func is nil, which BuildBindings reads as HasLegacyResidents=true
// for every store. That is not a stub — it is the honest verdict for a caller
// that took no census, and it keeps the residence probe running, which is the
// safe direction.
//
// store is used as a MAP KEY, so it inherits residencyBindingsFromRoutes's
// confinement verbatim: a beads.Store whose dynamic type carries a slice, a map
// or a func is not hashable and panics here. Every caller's store is one storage
// boot opened or one a test wrapped around it, all pointer- or field-comparable,
// which is the same argument that makes the grouping above safe.
func soleBindingResidency(store beads.Store) ([]storeref.ClassBinding, error) {
	order := []beads.Store{store} // residency:allow one opened binding, grouped for the shared BuildBindings derivation below
	return residencyBindingsFor(order, map[beads.Store][]coordclass.Class{store: infrastructureClasses()}, nil)
}

// infrastructureClasses is the class set a whole split relocates: every
// coordination class but work. Derived from coordclass rather than listed, so a
// sixth class joins every binding the day it is declared.
func infrastructureClasses() []coordclass.Class {
	classes := make([]coordclass.Class, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			classes = append(classes, class)
		}
	}
	return classes
}

// assembleResidencyTopology puts the work leg, the rig legs and the bindings
// together. The work leg carries the city's configured HQ prefix and each rig
// leg its own effective prefix, because those are what decide whether a work
// store SHADOWS an id inside a relocated class's namespace.
func assembleResidencyTopology(cfg *config.City, work beads.Store, rigs map[string]beads.Store, bindings []storeref.ClassBinding, refused error) storeref.Topology {
	topo := storeref.Topology{
		Work:     storeref.Leg{Ref: storeref.WorkRef, Store: work, Prefix: hqPrefixOf(cfg)},
		Bindings: bindings,
		Refused:  refused,
	}
	prefixes := rigPrefixesOf(cfg)
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		store := rigs[name]
		if store == nil {
			continue
		}
		topo.Rigs = append(topo.Rigs, storeref.Leg{Ref: storeref.RigRef(name), Store: store, Prefix: prefixes[name]})
	}
	return topo
}

func hqPrefixOf(cfg *config.City) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(config.EffectiveHQPrefix(cfg))
}

func rigPrefixesOf(cfg *config.City) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		out[rig.Name] = strings.TrimSpace(rig.EffectivePrefix())
	}
	return out
}
