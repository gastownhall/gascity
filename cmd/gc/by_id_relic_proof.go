package main

// The by-id door's relic proof: computed, never recorded.
//
// A city whose boot REFUSED still serves WORK from its work ledger, so the
// residency resolver tolerates the standing storage refusal on a residence
// probe and the surface falls through to its own axis. The sentence that
// justifies that is "this leg was only ever a probe for an id no relocated
// class could own", and there is exactly one shape of city it is false for: one
// whose binding still holds ids `gc storage migrate` carried across under their
// original work-shaped names. There, falling through does not land on "no
// answer" — it lands on the frozen pre-migration copy the migration left in the
// work store, which answers confidently and wrongly, and the close that follows
// writes it. That is ga-q8ick.
//
// # Why the proof is taken here rather than read off disk
//
// The obvious place to keep this verdict is a note under the city's own .gc,
// written by whichever process last managed to read the binding. It is the
// wrong place twice over. It is a status file, which this codebase does not
// keep (AGENTS.md: no status files — query live state), and it goes stale in
// the DENYING direction: relics close over the weeks after a migration, so a
// note written in March denies reads in June for a binding that has been clean
// since April.
//
// The premise the note was there to work around turns out to be false anyway.
// "Refused" is a verdict about SERVING the binding — a convergence check, a
// served-binding note, a discipline the boot gate enforces — and almost none of
// those verdicts say the binding cannot be READ. So the census that decides
// this runs right here, against a handle opened for the read and closed after
// it, and its answer is about the binding as it is now.
//
// # What it costs, and who pays
//
// Only a refused city reaches any of it. residencyTopologyForCity has already
// answered by the time the gate below is consulted, and a served city's
// Topology.Refused is nil, so it resolves no plan, opens no engine and takes no
// read — TestServedCityPaysNothingForTheRelicProof counts the plan resolutions
// and requires zero. A refused city pays one engine open and one full list of
// the binding, once per process per city, on the by-id path only. That city is
// already in the incident state its own boot gate reported.
//
// # The absent case is the tolerant one
//
// Every way of not reaching an answer — a config that will not load, a plan
// that will not resolve, a provider that opens no engine, an open that fails —
// is proof-ABSENT, and proof-absent falls through exactly as today. The bit
// only ever denies, so its unknown must be false: a binding nobody could read
// has proved nothing, and denying on it would take work-bead reads away from
// every city whose binding is merely unreachable.
//
// # It opens only a binding that ALREADY EXISTS on disk
//
// beads.OpenSQLiteStore creates the database when it is absent, so an opener
// used as a probe answers its own question: it would report "no relics" about a
// binding it just created. The never-migrated city — a [storage] split
// configured and never cut over — is the most common refused city there is, and
// on it an ordinary by-id read would leave an empty binding root behind that a
// later boot reads as a genesis root. A workspace-backed provider would start a
// managed engine for the same read. So existence is checked BEFORE the open,
// through the same two helpers the migration's own no-open probe uses
// (infraBindingRootEnumerable, infraPathExists), and a binding that is not
// already there is proof-absent.

import (
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storeref"
)

// byIDResidencyTopology is the topology the by-id door plans over: the city's
// own residency topology, plus — on a REFUSED city only — the proof that the
// binding it cannot serve still holds ids the migration preserved.
//
// The refused city is re-derived rather than patched. A ClassBinding's two
// relic bits have an implication between them that storeref.BuildBindings
// enforces, and reaching into the assembled topology to raise one of them by
// hand is how a plane ends up spelling a state no city can be in. Handing the
// proof back through residencyBindingsFromRoutesWithProof means both bits and
// the ref they are keyed by come from the one derivation.
func byIDResidencyTopology(cityPath string, cfg *config.City, work beads.Store, rigs map[string]beads.Store) storeref.Topology {
	topo := residencyTopologyForCity(cityPath, cfg, work, rigs)
	if topo.Refused == nil || len(topo.Bindings) == 0 || cityPath == "" {
		return topo
	}
	proven := provenRelicRefsForCity(cityPath)
	if len(proven) == 0 {
		return topo
	}
	bindings, refused := residencyBindingsFromRoutesWithProof(
		residencyRoutesForCity(cityPath),
		func(ref storeref.StoreRef) bool { return proven[ref] },
	)
	return assembleResidencyTopology(cfg, work, rigs, bindings, refused)
}

// provenRelicRefsForCity opens the binding this city is configured for, censuses
// it, and returns the binding refs the census PROVED hold ids outside the
// namespaces they declare.
//
// It is called only for a city whose boot refused, which is also what makes
// opening the binding here safe: the refusal is why nothing else in this
// process holds it open. A served city's binding is already open on the funnel,
// and a second handle on a binding root — a duplicate managed-Dolt server, a
// second sqlite writer — is the bug the residency constructors exist to avoid.
//
// The handle is closed before the verdict is returned. Nothing downstream reads
// through it: what travels is a set of refs.
//
// An empty result is "nothing proved", which every failure path also produces
// and which the caller reads as no evidence.
func provenRelicRefsForCity(cityPath string) map[storeref.StoreRef]bool {
	key := filepath.Clean(cityPath)
	provenRelicRefsMu.Lock()
	if provenRelicRefsByCity == nil {
		provenRelicRefsByCity = make(map[string]map[storeref.StoreRef]bool, 1)
	}
	cached, ok := provenRelicRefsByCity[key]
	provenRelicRefsMu.Unlock()
	if ok {
		return cached
	}

	proven := censusRefusedCityBinding(cityPath)

	provenRelicRefsMu.Lock()
	defer provenRelicRefsMu.Unlock()
	if provenRelicRefsByCity == nil {
		// resetProvenRelicRefs ran while this census was in flight. The answer
		// is still right for this caller, so it is returned unmemoized rather
		// than assigned into a nil map.
		return proven
	}
	provenRelicRefsByCity[key] = proven
	return proven
}

// censusRefusedCityBinding is the read itself: resolve this city's storage
// plan, open the binding it names, and ask each derived binding whether it
// still holds a relic.
//
// Every early return is the same answer — no proof — and they are deliberately
// silent. A refused city has already had its refusal printed once by the
// one-shot gate, and the reasons a binding cannot be reopened here are the
// reasons it was refused in the first place; reporting them again would put a
// second copy of the same sentence on every by-id read of an unconverged city.
func censusRefusedCityBinding(cityPath string) map[storeref.StoreRef]bool {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil || cfg == nil || cfg.Storage == nil {
		return nil
	}
	shape, binding := storageSplitShapeOf(cfg.EffectiveStorage())
	if shape != storageSplitWhole {
		// The only arrangement this build serves is also the only one a
		// migration can have produced, so it is the only one whose binding can
		// hold a preserved id.
		return nil
	}
	if !refusedBindingIsAlreadyOnDisk(cityPath, cfg) {
		return nil
	}
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return nil
	}
	routes, err := openStorageRoutes(plan, infraBindingTarget{Binding: binding})
	if err != nil {
		// "Cannot open the binding" — the one refusal that really does say the
		// binding is unreadable. No proof, and the read falls through.
		return nil
	}
	defer routes.close() //nolint:errcheck // a close failure cannot unsay what the census already read

	bindings, _ := residencyBindingsFromRoutes(routes)
	proven := make(map[storeref.StoreRef]bool, len(bindings))
	for _, b := range bindings {
		if storeref.ProvenLegacyResidents(b) { // residency:allow — censuses the binding this proof is about; resolves nothing
			proven[b.Leg.Ref] = true
		}
	}
	return proven
}

// refusedBindingIsAlreadyOnDisk reports whether this city's configured binding
// exists, WITHOUT opening it.
//
// It is the precondition on the census above, and it is a correctness gate
// rather than an optimization. The engine opener creates the database when it is
// absent, so a census that skipped this would report "no relics" about a
// binding it had just brought into existence — and would leave that binding
// behind. infraBindingHoldsNothing already states the rule for the migration's
// own probe: a probe that creates the database it is asked about is answering
// its own question. Both checks are the migration's, reused rather than
// re-spelled, so the two probes cannot disagree about what "the binding is
// there" means.
//
// The root is checked as well as the database, and for this gate that is
// belt-and-braces rather than a second condition: every way the root check can
// fail — absent, not a directory, not enumerable by this process — also makes
// the database stat below answer absent or error, and both decline. It is kept
// so this precondition reads the same as infraBindingHoldsNothing's, which is
// the migration's own no-open probe and the place the rule is argued. The
// DATABASE check is the one carrying the weight here, and it is the one a
// mutation kills: a binding root that exists and holds no database is a city
// that has not cut over, and only that check can tell it from one whose binding
// is clean.
//
// A binding served by a provider this build resolves no target for takes the
// second branch. resolveInfraBindingTarget answers only for the built-in bead
// engine, and for anything else this file cannot know which file under a root
// is the database — so the PROVIDER is asked where it serves from, and that
// location has to exist. Declining is always safe here: the bit only ever
// denies.
func refusedBindingIsAlreadyOnDisk(cityPath string, cfg *config.City) bool {
	target, configured, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil {
		return false
	}
	if !configured {
		return foreignBindingLocationExists(cityPath, cfg)
	}
	if err := infraBindingRootEnumerable(target.Root); err != nil {
		return false
	}
	present, err := infraPathExists(target.Database)
	return err == nil && present
}

// foreignBindingLocationExists is the same question for a binding served by a
// provider whose layout this build does not own: does the location the PROVIDER
// reports it serves from already exist?
//
// It asks the provider rather than guessing, because only the provider knows —
// the built-in engine reports a database FILE, another may report a directory,
// and a third may report something that is not a path at all. All three answer
// this correctly: a location that is not an existing path is not a binding this
// process may bring into existence, so it is proof-absent, and the read falls
// through. A remote or opaque location fails the same way, which is the safe
// direction.
func foreignBindingLocationExists(cityPath string, cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	storage := cfg.EffectiveStorage()
	shape, binding := storageSplitShapeOf(storage)
	if shape != storageSplitWhole {
		return false
	}
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return false
	}
	location, err := servedBindingLocation(plan, binding, storage.Bindings[binding])
	if err != nil || location == "" {
		return false
	}
	present, err := infraPathExists(location)
	return err == nil && present
}

var (
	provenRelicRefsMu     sync.Mutex
	provenRelicRefsByCity map[string]map[storeref.StoreRef]bool
)

// resetProvenRelicRefs drops the memo wholesale, alongside the routes and the
// binding grouping derived from them. The verdict is about a binding this
// process opened and closed, so it cannot outlive the funnel that decided the
// city was refused in the first place.
func resetProvenRelicRefs() {
	provenRelicRefsMu.Lock()
	provenRelicRefsByCity = nil
	provenRelicRefsMu.Unlock()
}

// dropProvenRelicRefs drops one city's memoized verdict.
func dropProvenRelicRefs(key string) {
	provenRelicRefsMu.Lock()
	delete(provenRelicRefsByCity, key)
	provenRelicRefsMu.Unlock()
}
