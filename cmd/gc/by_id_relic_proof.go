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
// keep (AGENTS.md: no status files — query live state), and it could not
// replace the census anyway: a note is only as good as the process that wrote
// it, so it is absent on every city no such process has visited, and the read
// below has to exist for those cities regardless.
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
// and requires zero. A refused city pays one engine open and one by-id Get of
// the binding (plus, on a miss, one read of the copy manifest), on the by-id
// path only. It used to pay a full closed-inclusive list of the binding: on the
// measured city that was ~134k rows hydrated from a 2 GB database, ~20 s per
// `gc bd show`, to answer a question about one id.
//
// The memo below makes that ONE read per (city, id) for the callers this door
// actually has: one-shot cobra commands that resolve by-id sequentially. It is
// not a single-flight — provenTwinRefsForID releases the memo lock across the
// read — so two concurrent by-id callers on the same refused city would each
// take the read, and each would open its own handle on the binding root. That
// is the second-handle hazard named below, and what rules it out today is the
// absence of such a caller, not the memo. A parallel by-id caller has to bring
// a single-flight (or a sync.Once entry) with it: ga-nzxob.
//
// # Who the denial reaches
//
// The proof is PER ID: a refused city's residence probe turns Fatal for an id
// only when that id has a frozen twin, i.e. when the binding holds the id now,
// or when the migration's copy manifest records delivering it (the binding's
// own GC has since removed it, and the retained work-store copy is still the
// pre-migration one). Every other non-reserved id falls through to the work
// ledger exactly as on a refused city with no relics at all, because for that
// id the work ledger IS the only copy: a bead minted in the work store after the
// cutover, a rig-shadowed id, or one of the "stranded" infra beads the boot gate
// itself reports as "intact in the work store".
//
// It used to be keyed by BINDING ref, so one proven relic took the by-id door
// away for the whole city. That was a deliberate trade at the time ("a per-id
// rule would have the population it has to consult"), and it is the trade the
// measured city lost: its binding held ~790 relics while its boot gate refused
// for 790 unrelated stranded wisps, so every plain `gc bd show <work-id>` exited
// 1 before reaching bd, for a bead nothing had ever migrated. The ga-q8ick
// guarantee is unchanged — the id that HAS a twin is still denied, with the same
// sentinel and remedy — and the storeref corpus is unchanged too: its T3k rows
// pin what a proven binding plans, and this file now only decides, per id,
// whether to hand the planner that proof.
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
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

// byIDResidencyTopology is the topology the by-id door plans over for id: the
// city's own residency topology, plus — on a REFUSED city only — the proof that
// the binding it cannot serve holds a frozen-twin hazard for THIS id.
//
// The refused city is re-derived rather than patched. A ClassBinding's two
// relic bits have an implication between them that storeref.BuildBindings
// enforces, and reaching into the assembled topology to raise one of them by
// hand is how a plane ends up spelling a state no city can be in. Handing the
// proof back through residencyBindingsFromRoutesWithProof means both bits and
// the ref they are keyed by come from the one derivation.
//
// The topology is therefore id-specific on a refused city, which is no new
// constraint: every caller plans it straight into Plan(ByID{ID: id}), and a
// by-id plan is already id-specific (resolveByID refuses one executed for a
// different id).
func byIDResidencyTopology(cityPath string, cfg *config.City, work beads.Store, rigs map[string]beads.Store, id string) storeref.Topology {
	topo := residencyTopologyForCity(cityPath, cfg, work, rigs)
	if topo.Refused == nil || len(topo.Bindings) == 0 || cityPath == "" {
		return topo
	}
	proven := provenTwinRefsForID(cityPath, id)
	if len(proven) == 0 {
		return topo
	}
	bindings, refused := residencyBindingsFromRoutesWithProof(
		residencyRoutesForCity(cityPath),
		func(ref storeref.StoreRef) bool { return proven[ref] },
	)
	return assembleResidencyTopology(cfg, work, rigs, bindings, refused)
}

// provenTwinRefsForID opens the binding this city is configured for and
// returns the binding refs PROVEN to give id a frozen twin in the work store:
// the binding holds id now, or the migration's copy manifest records
// delivering it there.
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
func provenTwinRefsForID(cityPath, id string) map[storeref.StoreRef]bool {
	key := filepath.Clean(cityPath)
	id = strings.TrimSpace(id)
	provenRelicRefsMu.Lock()
	if provenRelicRefsByCity == nil {
		provenRelicRefsByCity = make(map[string]map[string]map[storeref.StoreRef]bool, 1)
	}
	cached, ok := provenRelicRefsByCity[key][id]
	provenRelicRefsMu.Unlock()
	if ok {
		return cached
	}

	proven := censusRefusedCityBindingFor(cityPath, id)

	provenRelicRefsMu.Lock()
	defer provenRelicRefsMu.Unlock()
	if provenRelicRefsByCity == nil {
		// resetProvenRelicRefs ran while this read was in flight. The answer
		// is still right for this caller, so it is returned unmemoized rather
		// than assigned into a nil map.
		return proven
	}
	if provenRelicRefsByCity[key] == nil {
		provenRelicRefsByCity[key] = make(map[string]map[storeref.StoreRef]bool, 1)
	}
	provenRelicRefsByCity[key][id] = proven
	return proven
}

// censusRefusedCityBindingFor is the read itself: resolve this city's storage
// plan, open the binding it names, and ask each derived binding whether id has
// a twin there.
//
// Every early return is the same answer — no proof — and they are deliberately
// silent. A refused city has already had its refusal printed once by the
// one-shot gate, and the reasons a binding cannot be reopened here are the
// reasons it was refused in the first place; reporting them again would put a
// second copy of the same sentence on every by-id read of an unconverged city.
func censusRefusedCityBindingFor(cityPath, id string) map[storeref.StoreRef]bool {
	if id == "" {
		return nil
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil || cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.EffectiveStorage()
	shape, binding := storageSplitShapeOf(storage)
	if shape != storageSplitWhole {
		// The only arrangement this build serves is also the only one a
		// migration can have produced, so it is the only one whose binding can
		// hold a preserved id.
		return nil
	}
	// Native transport is checked here, against the city's own cfg, because
	// the open below carries none: native_transport "off" keeps the census from
	// opening a natively served binding, as it keeps the boot gate from
	// serving one.
	if nativeTransportBindingRefusal(binding, storebinding.ProviderID(storage.Bindings[binding].Provider), cfg) != nil {
		return nil
	}
	if !refusedBindingIsAlreadyOnDisk(cityPath, cfg) {
		return nil
	}
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return nil
	}
	// Unstamped (nil cfg): the census only reads, and a require refusal here
	// would read as "cannot open the binding".
	routes, err := openStorageRoutes(plan, infraBindingTarget{Binding: binding}, nil, "", nil)
	if err != nil {
		// "Cannot open the binding" — the one refusal that really does say the
		// binding is unreadable. No proof, and the read falls through.
		return nil
	}
	defer routes.close() //nolint:errcheck // a close failure cannot unsay what the census already read

	delivered := migrationDeliveredID(cityPath, cfg)
	bindings, _ := residencyBindingsFromRoutes(routes)
	proven := make(map[storeref.StoreRef]bool, len(bindings))
	for _, b := range bindings {
		if bindingGivesIDATwin(b, id, delivered) {
			proven[b.Leg.Ref] = true
		}
	}
	return proven
}

// bindingGivesIDATwin reports whether the work store's copy of id is a frozen
// pre-migration twin of a bead this binding owns.
//
// A binding that HOLDS id proves it outright: an id outside every reserved
// namespace (the only kind that reaches a residence probe) can be in the
// binding only because a migration or recover-stranded carried it across with
// its id preserved, and neither deletes the source. A binding that no longer
// holds id still owns it when the copy manifest says the migration delivered
// it — the binding's own GC hard-deletes expired closed wisps and read mail,
// and the work store keeps the pre-migration row forever.
//
// Only a completed read proves anything: a Get that fails for any reason other
// than absence is proof-absent, the same tolerant unknown as the open above.
func bindingGivesIDATwin(b storeref.ClassBinding, id string, delivered func(string) bool) bool {
	if b.Leg.Store == nil {
		return false
	}
	_, err := b.Leg.Store.Get(id) // residency:allow — the per-id twin proof for a refused binding; resolves nothing
	switch {
	case err == nil:
		return true
	case errors.Is(err, beads.ErrNotFound):
		return delivered(id)
	default:
		return false
	}
}

// migrationDeliveredID answers "did the infra migration's equality stage prove
// it copied this id into the binding" from the copy manifest the migration
// recorded (infra.migrated.beads), the same evidence the boot gate's own
// containment check classifies GC'd rows by. It is read at most once per call,
// lazily, and only on a Get miss. A binding this build resolves no migration
// target for, an absent manifest, or one that cannot be read, answers false:
// nothing is proved.
func migrationDeliveredID(cityPath string, cfg *config.City) func(string) bool {
	var (
		once     sync.Once
		manifest map[string]bool
	)
	return func(id string) bool {
		once.Do(func() {
			target, configured, err := resolveInfraBindingTarget(cityPath, cfg)
			if err != nil || !configured {
				return
			}
			proven, recorded, err := readInfraCopyManifest(target)
			if err != nil || !recorded {
				return
			}
			manifest = proven
		})
		return manifest[id]
	}
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
		// Deliberately re-derived, not a second independent precondition: the
		// one caller today reaches here having already established this on the
		// same cfg (censusRefusedCityBinding). It is kept because it is what
		// makes `binding` safe to read below — storageSplitShapeOf names no
		// binding for any other shape — so the helper stays correct for a
		// caller it may later acquire.
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
	provenRelicRefsMu sync.Mutex
	// provenRelicRefsByCity memoizes provenTwinRefsForID: city -> id -> refs.
	provenRelicRefsByCity map[string]map[string]map[storeref.StoreRef]bool
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
