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
// does not decide whether a binding mints truthfully: nothing in this build
// verifies a binding's mint prefix, so MintsReserved stays false everywhere
// here and the residence probe stays in every plan. The corpus already carries
// the retired row, so the day verification ships this is a bit, not a redesign.

import (
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
// at boot and the stores it supervises.
//
// # It currently includes SUSPENDED rigs, and S3 must fix that before consuming it
//
// rigBeadStores() is every rig the runtime holds open, suspended or not. The
// census path today does not read it raw: buildDesiredState computes
// suspendedRigPaths (build_desired_state.go:401) and threads it into
// collectAllOpenSessionInfos, which is what keeps a suspended rig out of the
// scan. This constructor has no suspension frame to thread, so a Topology built
// here carries the suspended rig as an ordinary FederationTail leg.
//
// That is harmless while nothing consumes it — S0 changes no behavior — and it
// is a REGRESSION the moment S3 routes the census through Plan(Census): a rig
// suspended because it is dark would fail its leg, mark every census result
// Partial, and trip the retain-don't-reap rule fleet-wide. S3 must pass the
// suspension frame in (excluding suspended rigs from the map it hands here, the
// "it is told, it does not decide" contract) before the first consumer lands.
func (cr *CityRuntime) residencyTopology() storeref.Topology {
	bindings, refused := residencyBindingsFromRoutes(cr.storageRoutes)
	return assembleResidencyTopology(cr.cfg, cr.cityBeadStore(), cr.rigBeadStores(), bindings, refused)
}

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
	cliResidencyBindingsByCity[key] = &cliResidencyBindingsEntry{bindings: bindings, refused: refused}
	return bindings, refused
}

// resetCLIResidencyBindings drops the memo. Wired into closeCLIStorageRoutes's
// caller-visible lifecycle by the tests that need a second city in one process.
func resetCLIResidencyBindings() {
	cliResidencyBindingsMu.Lock()
	cliResidencyBindingsByCity = nil
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
	return residencyBindingsFor(order, byStore)
}

// residencyBindingsFor turns a store->classes grouping into bindings, and
// reports the standing refusal when any binding is a refusing store.
func residencyBindingsFor(order []beads.Store, byStore map[beads.Store][]coordclass.Class) ([]storeref.ClassBinding, error) {
	var refused error
	bindings := make([]storeref.ClassBinding, 0, len(order))
	for _, store := range order {
		classes := byStore[store]
		bindings = append(bindings, storeref.ClassBinding{
			Classes:  classes,
			Prefixes: reservedPrefixesFor(classes),
			Leg:      storeref.Leg{Ref: storeref.ClassRef(classes), Store: store},
			// MintsReserved and HasLegacyResidents stay false: nothing in this
			// build verifies a binding's mint prefix or censuses its relics, so
			// the residence probe stays in every plan.
		})
		if refusing, ok := store.(storeref.RefusingStore); ok && refused == nil {
			refused = refusing.StorageRefusal()
		}
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Leg.Ref < bindings[j].Leg.Ref })
	return bindings, refused
}

// reservedPrefixesFor returns the reserved id prefixes a class set mints.
func reservedPrefixesFor(classes []coordclass.Class) []string {
	prefixes := make([]string, 0, len(classes))
	for _, class := range classes {
		if prefix, ok := config.ReservedClassPrefix(class.String()); ok {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
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
