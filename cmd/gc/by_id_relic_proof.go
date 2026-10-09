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
// The proof is PER ID whenever the migration's copy manifest could be read: a
// refused city's residence probe turns Fatal for an id only when that id has a
// frozen twin, i.e. when the binding holds the id now, or when the manifest
// records delivering it (the binding's own GC has since removed it, and the
// retained work-store copy is still the pre-migration one). With no readable
// manifest a miss proves nothing, and the binding-wide rule below still
// applies (bindingGivesIDATwin). Every other non-reserved id falls through to the work
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
// # The same census answers before the funnel, and usually ends the question
//
// Entering the by-id door resolves the one-shot funnel, which is the boot gate:
// on a converged or refused split it lists the WORK store's whole
// infrastructure slice through bd (both tiers, closed rows included) to prove
// containment, and on a served one it also censuses the binding. On
// maintainer-city that was ~6 s of a ~14 s `gc bd show <work-id>` whose bd
// call takes ~5 s — paid for a verdict the answer did not depend on.
//
// For a NON-RESERVED id the verdict decides nothing once this census has
// answered twinNone or twinNoSplit, because every verdict then falls through:
//
//   - bypass (no relocated class): the door is not entered at all;
//   - served: the binding is the only leg the door probes, and its Get
//     answered not-found (a retired probe falls through without asking);
//   - refused, for ANY reason: the probe is tolerated unless this census
//     denies the id, and twinNone is exactly bindingGivesIDATwin's "no" (the
//     per-id manifest rule when the manifest was read, the binding-wide rule
//     when it was not). twinNoSplit is the shape the census never denies
//     anything for, so a refused city with no whole split falls through too.
//
// So bdByIDAnswerIsThePassthroughForEveryVerdict takes the census first and
// skips the funnel when every subject is non-reserved and twinNone or
// twinNoSplit. Every other answer — a reserved id, a binding hit, a binding
// Get fault, a manifest-delivered id, a binding-wide relic with no readable
// manifest, any undecided read — enters the funnel exactly as before, and the
// refused path then reads the proof from the memo this census filled rather
// than opening the binding a second time. The denial is unchanged: it is
// computed by the same function, for the same ids, from the same reads.
//
// What the skip drops is the funnel's side output, never an answer: the boot
// refusal it prints once to stderr, and a served city's served-binding note,
// which the controller's own boot writes.
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

// bdByIDAnswerIsThePassthroughForEveryVerdict reports whether the by-id door
// would hand ids to the passthrough whatever the boot gate's verdict, so the
// door need not resolve the funnel (and pay its work-store census) to say so.
// See "The same census answers before the funnel" above for why each verdict
// falls through on these ids.
//
// false is always safe: it sends the caller into the funnel, which is the
// path every answer took before. It is returned for a reserved id, an empty id
// set, a census that proved a twin or could not decide, and for the two states
// in which this process already holds the city's routes (a resolved funnel, or
// a controller's registration) — there the census would open a second handle
// on a binding root that is already open, and the routes answer cheaply anyway.
func bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath string, ids []string) bool {
	if cityPath == "" || len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || bdIDIsClassReserved(id) {
			return false
		}
	}
	if _, registered := registeredResidencyEntry(cityPath); registered {
		return false
	}
	if cliStorageRoutesResolved(cityPath) {
		return false
	}
	for _, proof := range byIDTwinProofsFor(cityPath, ids) {
		if proof.verdict != twinNone && proof.verdict != twinNoSplit {
			return false
		}
	}
	return true
}

// provenTwinRefsForID opens the binding this city is configured for and
// returns the binding refs PROVEN to give id a frozen twin in the work store:
// the binding holds id now, or the migration's copy manifest records
// delivering it there.
//
// It is called for a city whose boot refused, which is also what makes
// opening the binding here safe: the refusal is why nothing else in this
// process holds it open. A served city's binding is already open on the funnel,
// and a second handle on a binding root — a duplicate managed-Dolt server, a
// second sqlite writer — is the bug the residency constructors exist to avoid.
// The by-id door's funnel skip (bdByIDAnswerIsThePassthroughForEveryVerdict)
// takes the same census BEFORE the funnel has opened anything, which is the
// other moment no handle exists, and the memo it fills is this one.
//
// The handle is closed before the verdict is returned. Nothing downstream reads
// through it: what travels is a set of refs.
//
// An empty result is "nothing proved", which every failure path also produces
// and which the caller reads as no evidence.
func provenTwinRefsForID(cityPath, id string) map[storeref.StoreRef]bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	return byIDTwinProofsFor(cityPath, []string{id})[id].proven
}

// byIDTwinVerdict is what one census read established about one id.
type byIDTwinVerdict uint8

const (
	// twinUndecided is the tolerant unknown: a config that will not load, a
	// plan that will not resolve, a binding not on disk or not openable, a
	// binding leg with no store. It proves nothing, so the refused path
	// denies nothing on it — and the funnel skip takes nothing from it either.
	twinUndecided byIDTwinVerdict = iota
	// twinDeny: the read must be denied (bindingGivesIDATwin): the binding
	// holds the id, its Get failed other than not-found, the copy manifest
	// records delivering it, or — with no readable manifest — the binding
	// holds some id outside its reserved namespaces.
	twinDeny
	// twinNone: the binding ANSWERED not-found for the id and no twin is
	// proven by the rule bindingGivesIDATwin applies (the manifest, when it
	// was read; else the binding-wide census).
	twinNone
	// twinNoSplit: the city configures no whole-split binding, the only
	// arrangement a migration can have produced, so no binding can hold a
	// preserved id. It is also the one arrangement the boot gate never serves.
	twinNoSplit
)

// byIDTwinProof is one id's census answer: the verdict, and on twinDeny the
// binding refs the proof is keyed by.
type byIDTwinProof struct {
	verdict byIDTwinVerdict
	proven  map[storeref.StoreRef]bool
}

// byIDTwinProofsFor answers the census for every id, memoized per (city, id),
// taking ONE census — one config load, one binding open, one Get per id and at
// most one manifest read — for the ids the memo does not hold yet. A bulk
// by-id argv (a maintenance close of a batch of stale wisps) therefore opens
// the binding once, not once per id.
func byIDTwinProofsFor(cityPath string, ids []string) map[string]byIDTwinProof {
	key := filepath.Clean(cityPath)
	out := make(map[string]byIDTwinProof, len(ids))
	var missing []string
	provenRelicRefsMu.Lock()
	if provenRelicRefsByCity == nil {
		provenRelicRefsByCity = make(map[string]map[string]byIDTwinProof, 1)
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if cached, ok := provenRelicRefsByCity[key][id]; ok {
			out[id] = cached
			continue
		}
		missing = append(missing, id)
	}
	provenRelicRefsMu.Unlock()
	if len(missing) == 0 {
		return out
	}

	read := censusCityBindingTwins(cityPath, missing)

	provenRelicRefsMu.Lock()
	defer provenRelicRefsMu.Unlock()
	for _, id := range missing {
		out[id] = read[id]
	}
	if provenRelicRefsByCity == nil {
		// resetProvenRelicRefs ran while this read was in flight. The answer
		// is still right for this caller, so it is returned unmemoized rather
		// than assigned into a nil map.
		return out
	}
	if provenRelicRefsByCity[key] == nil {
		provenRelicRefsByCity[key] = make(map[string]byIDTwinProof, len(missing))
	}
	for _, id := range missing {
		provenRelicRefsByCity[key][id] = read[id]
	}
	return out
}

// censusCityBindingTwins is the read itself: resolve this city's storage
// plan, open the binding it names, and ask each derived binding whether each
// id has a twin there.
//
// Every early return before the open is either twinNoSplit (the config names
// no whole split, so no binding can hold a preserved id) or twinUndecided, and
// they are deliberately silent. A refused city has already had its refusal
// printed once by the one-shot gate, and the reasons a binding cannot be
// reopened here are the reasons it was refused in the first place; reporting
// them again would put a second copy of the same sentence on every by-id read
// of an unconverged city.
//
// The config is loaded without the revision snapshot, for the funnel's reason
// (cliStorageRoutesLoad): nothing here reads config.Revision(), and the
// snapshot content-hashes every pack file, which on maintainer-city was most of
// this function's CPU.
func censusCityBindingTwins(cityPath string, ids []string) map[string]byIDTwinProof {
	out := make(map[string]byIDTwinProof, len(ids))
	all := func(verdict byIDTwinVerdict) map[string]byIDTwinProof {
		for _, id := range ids {
			out[id] = byIDTwinProof{verdict: verdict}
		}
		return out
	}
	cfg, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), cliStorageRoutesLoad)
	if err != nil || cfg == nil {
		return all(twinUndecided)
	}
	if cfg.Storage == nil {
		return all(twinNoSplit)
	}
	storage := cfg.EffectiveStorage()
	shape, binding := storageSplitShapeOf(storage)
	if shape != storageSplitWhole {
		// The only arrangement this build serves is also the only one a
		// migration can have produced, so it is the only one whose binding can
		// hold a preserved id.
		return all(twinNoSplit)
	}
	// Native transport is checked here, against the city's own cfg, because
	// the open below carries none: native_transport "off" keeps the census from
	// opening a natively served binding, as it keeps the boot gate from
	// serving one.
	if nativeTransportBindingRefusal(binding, storebinding.ProviderID(storage.Bindings[binding].Provider), cfg) != nil {
		return all(twinUndecided)
	}
	if !refusedBindingIsAlreadyOnDisk(cityPath, cfg) {
		return all(twinUndecided)
	}
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return all(twinUndecided)
	}
	// Unstamped (nil cfg): the census only reads, and a require refusal here
	// would read as "cannot open the binding".
	routes, err := openStorageRoutes(plan, infraBindingTarget{Binding: binding}, nil, "", nil)
	if err != nil {
		// "Cannot open the binding" — the one refusal that really does say the
		// binding is unreadable. No proof, and the read falls through.
		return all(twinUndecided)
	}
	defer routes.close() //nolint:errcheck // a close failure cannot unsay what the census already read

	manifest, manifestRead := migrationCopyManifest(cityPath, cfg)
	bindings, _ := residencyBindingsFromRoutes(routes)
	for _, id := range ids {
		out[id] = bindingsTwinProof(bindings, id, manifest, manifestRead)
	}
	return out
}

// bindingsTwinProof folds every derived binding's answer for id into one
// proof. Any binding denying the id denies it; the id is twinNone only when
// there was at least one binding and EVERY one of them answered absence and
// proved no twin.
func bindingsTwinProof(bindings []storeref.ClassBinding, id string, manifest map[string]bool, manifestRead bool) byIDTwinProof {
	proof := byIDTwinProof{verdict: twinUndecided}
	if len(bindings) == 0 {
		return proof
	}
	allNone := true
	for _, b := range bindings {
		switch bindingTwinVerdict(b, id, manifest, manifestRead) {
		case twinDeny:
			if proof.proven == nil {
				proof.proven = make(map[storeref.StoreRef]bool, len(bindings))
			}
			proof.proven[b.Leg.Ref] = true
		case twinNone:
		default:
			allNone = false
		}
	}
	switch {
	case len(proof.proven) > 0:
		proof.verdict = twinDeny
	case allNone:
		proof.verdict = twinNone
	}
	return proof
}

// bindingGivesIDATwin reports whether the work store's copy of id may be a
// frozen pre-migration twin of a bead this binding owns, so the by-id read
// must be denied rather than fall through to the work ledger.
//
// A binding that HOLDS id proves it outright: an id outside every reserved
// namespace (the only kind that reaches a residence probe) can be in the
// binding only because a migration or `gc storage recover-stranded` carried it
// across with its id preserved, and neither deletes the source.
//
// A binding that does NOT hold id still owns it when the migration delivered it
// and the binding's own GC has since hard-deleted it (expired closed wisps,
// read mail) — the work store keeps the pre-migration row forever. Only the
// copy manifest can tell that id from one that was never migrated, so:
//
//   - manifest READ: the per-id rule. Deny iff the manifest lists id. This is
//     what lets a plain work bead on a refused city with relics (the
//     maintainer-city shape) reach bd.
//   - manifest absent (a city converged before the manifest was recorded, a
//     provider this build resolves no migration target for) or unreadable:
//     a miss proves nothing, so the pre-per-id rule applies — deny whenever
//     the binding holds ANY id outside its reserved namespaces. Without that,
//     a collected relic on a pre-manifest city is served from its frozen copy.
//
// recover-stranded edge: it copies stranded beads into the binding with their
// ids preserved and does not add them to the manifest. While the binding holds
// them, the Get below denies them; once the binding's GC collects one, the
// per-id rule lets its work-store row (the pre-recovery copy) be read again.
// That copy was the authoritative one until the recovery, so the exposure is a
// closed-and-collected wisp or mail row reading as its pre-recovery state.
//
// A Get that fails for any reason other than absence is a failure to decide,
// and is reported as one: the binding is treated as owning id, so the read is
// DENIED (an error naming the binding) rather than handed to the work ledger.
func bindingGivesIDATwin(b storeref.ClassBinding, id string, manifest map[string]bool, manifestRead bool) bool {
	return bindingTwinVerdict(b, id, manifest, manifestRead) == twinDeny
}

// bindingTwinVerdict is bindingGivesIDATwin's decision with the one bit the
// funnel skip needs on top: twinNone says not only "no twin" but that the
// binding ANSWERED for id — its Get came back not-found — so a served city's
// probe of the same binding falls through too. A binding with no store decides
// nothing (twinUndecided); the refused path reads that as no twin, as it always
// has, and the skip reads it as "take the verdict".
func bindingTwinVerdict(b storeref.ClassBinding, id string, manifest map[string]bool, manifestRead bool) byIDTwinVerdict {
	if b.Leg.Store == nil {
		return twinUndecided
	}
	_, err := b.Leg.Store.Get(id) // residency:allow — the per-id twin proof for a refused binding; resolves nothing
	switch {
	case err == nil:
		return twinDeny
	case !errors.Is(err, beads.ErrNotFound):
		return twinDeny
	case manifestRead:
		if manifest[id] {
			return twinDeny
		}
		return twinNone
	case bindingHoldsAnyRelic(b):
		return twinDeny
	default:
		return twinNone
	}
}

// bindingHoldsAnyRelic is the city-wide fallback verdict: does the binding hold
// any id outside its reserved namespaces, closed rows and both tiers included.
// It asks the store's one-statement census when the store has one and lists the
// binding only when it does not. A census that could not run proves nothing
// (false), exactly as the binding-keyed proof always answered.
func bindingHoldsAnyRelic(b storeref.ClassBinding) bool {
	if census, ok := beads.NamespaceCensusFor(b.Leg.Store); ok {
		has, err := census.HasResidentOutside(b.Prefixes)
		return err == nil && has
	}
	return storeref.ProvenLegacyResidents(b) // residency:allow — censuses the binding this proof is about; resolves nothing
}

// migrationCopyManifest reads the copy manifest the infra migration recorded
// (infra.migrated.beads): the ids its equality stage proved it copied into the
// binding, the same evidence the boot gate's containment check classifies
// GC'd rows by. read=false whenever no manifest was actually read — no
// migration target this build resolves, no manifest on disk, or one that could
// not be read — and the caller then falls back to the binding-wide verdict
// rather than reading the absence as "never delivered".
func migrationCopyManifest(cityPath string, cfg *config.City) (manifest map[string]bool, read bool) {
	target, configured, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !configured {
		return nil, false
	}
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil || !recorded {
		return nil, false
	}
	return proven, true
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
		// same cfg (censusCityBindingTwins). It is kept because it is what
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
	// provenRelicRefsByCity memoizes byIDTwinProofsFor: city -> id -> proof.
	provenRelicRefsByCity map[string]map[string]byIDTwinProof
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
