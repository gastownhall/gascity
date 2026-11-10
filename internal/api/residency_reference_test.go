package api

// The API plane's by-id residency and residency topology as they stood before
// typed-stores T7c, frozen (plan §2.3; S5-D's rig_import_plan_reference_test.go
// precedent), with every helper they call: residencyTopology derived the
// topology from the identity of the class handles, and the work axis routed
// by-id reads to raw stores. Each declaration is copied, doc comment and all,
// from 9aaf324499, renamed to its reference form; methods take the Server as
// their first argument. The equivalence tests run them beside
// Stores().Topology() and the ref-routed work axis that replaced them. Do not
// edit them to follow production.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// referenceResidencyTopology builds the API's view of which stores answer which
// questions.
//
// A class accessor that returns the city store, or nil, relocates nothing and
// contributes no binding — the identity gate that keeps a single-store city
// byte-identical. A plan's residence probe retires only when both halves say
// so: the mint bit observed from the store, the relic bit carried across the
// State surface from the boot that censused the binding.
func referenceResidencyTopology(s *Server) storeref.Topology {
	cfg := s.state.Config()
	city := s.state.CityBeadStore()

	byStore := map[beads.Store][]coordclass.Class{}
	var order []beads.Store
	for _, entry := range referenceClassStoreAccessors(s) {
		store := entry.store
		if store == nil || store == city {
			continue
		}
		if _, seen := byStore[store]; !seen {
			order = append(order, store)
		}
		byStore[store] = append(byStore[store], entry.class)
	}

	topo := storeref.Topology{
		Work: storeref.Leg{Ref: storeref.WorkRef, Store: city, Prefix: strings.TrimSpace(config.EffectiveHQPrefix(cfg))},
	}
	topo.Bindings, topo.Refused = referenceAPIResidencyBindings(order, byStore, s.state.ClassBindingHasLegacyResidents)

	if cfg != nil {
		for _, rig := range cfg.Rigs {
			store := s.state.BeadStore(rig.Name)
			if store == nil || store == city {
				continue
			}
			topo.Rigs = append(topo.Rigs, storeref.Leg{
				Ref:    storeref.RigRef(rig.Name),
				Store:  store,
				Prefix: strings.TrimSpace(rig.EffectivePrefix()),
			})
		}
	}
	return topo
}

// classStoreAccessor pairs a coordination class with the store the State
// surface serves it from, in coordclass order.
type classStoreAccessor struct {
	class coordclass.Class
	store beads.Store
}

// referenceClassStoreAccessors is every coordination class the State surface can
// OBSERVE a store for.
//
// Messaging is absent, and deliberately so: State exposes no messaging
// accessor (mail reaches its store through the mail provider), so this plane
// cannot see where the class is served from. completeObservedClasses is what
// keeps that blind spot from producing a different StoreRef for the same
// physical binding than the CLI and controller planes produce.
func referenceClassStoreAccessors(s *Server) []classStoreAccessor {
	st := s.state.Stores()
	return []classStoreAccessor{
		{coordclass.ClassGraph, st.Graph().Store},
		{coordclass.ClassSessions, st.Sessions().Store},
		{coordclass.ClassOrders, st.Orders().Store},
		{coordclass.ClassNudges, st.Nudges().Store},
	}
}

// observableInfraClasses is the set referenceClassStoreAccessors covers.
func observableInfraClasses() []coordclass.Class {
	return []coordclass.Class{coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassOrders, coordclass.ClassNudges}
}

// completeObservedClasses adds the classes this plane cannot observe to a
// binding that carries every one it can.
//
// A StoreRef must name the same physical binding on every plane — it is the
// vocabulary a persisted census row and the wake filter that reads it back
// share. Deriving it from the OBSERVED class set alone would give the API
// "class:gnos" where the CLI and controller give "class:gmnos", for one store,
// and the filter would drop rows written by the other half of the program.
//
// The completion is an observation about the opened stores, not a config read:
// when every infrastructure class this plane can see resolves to ONE non-work
// store, that store is the whole-split binding, and this build serves the whole
// split or none of it (cmd/gc's storageSupportedTopologyStatement). A partial
// arrangement never reaches a serving runtime. A binding carrying only SOME
// observable classes is left exactly as observed, so a future per-class split
// is described honestly rather than rounded up.
func completeObservedClasses(classes []coordclass.Class) []coordclass.Class {
	observable := observableInfraClasses()
	if len(classes) != len(observable) {
		return classes
	}
	out := append([]coordclass.Class(nil), classes...)
	for _, class := range coordclass.Classes() {
		if !class.IsInfrastructure() || containsClass(out, class) {
			continue
		}
		out = append(out, class)
	}
	return out
}

func containsClass(classes []coordclass.Class, want coordclass.Class) bool {
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

// referenceAPIResidencyBindings groups the relocated classes by the store serving them.
//
// The body is storeref.BuildBindings, shared with the CLI plane. What survives
// here is the only thing that is this plane's own: it observes four of the five
// infrastructure classes, so it hands the builder completeObservedClasses to
// round a whole-split binding back up to the class set — and therefore the
// StoreRef — the other planes name for the same store.
//
// relics is the boot census's verdict, read across the State surface. A nil one
// is the pessimistic answer for every store: a caller with no census to offer
// has not cleared anything.
func referenceAPIResidencyBindings(order []beads.Store, byStore map[beads.Store][]coordclass.Class, relics func(beads.Store) bool) ([]storeref.ClassBinding, error) {
	return storeref.BuildBindings(order, byStore, storeref.BindingOptions{
		Relics:          relics,
		CompleteClasses: completeObservedClasses,
	})
}

// referenceWorkAxis is this plane's by-id work-axis router, declared to the resolver.
//
// It is consulted for one arm only — an id no binding's reserved namespace
// claims — and that is what makes it correct as well as cheap. Inside a
// namespace the binding is the authority and the work stores are a probe list
// behind it, so running the prefix arms there would let a rig configured with a
// reserved prefix capture an id the binding mints; it would also pay a
// routes.jsonl disk read on every molecule-id read of a relocated city.
type referenceWorkAxis struct{ s *Server }

// WorkLegs returns the work stores that can hold id, in this plane's own
// order: the store whose configured prefix owns the id, else the store its
// routes.jsonl entry names, else the full scan — the city/HQ store ahead of
// every rig work store.
//
// This is the pre-resolver candidate list verbatim. It is stated as legs rather
// than stores so a partial answer, a failure and a persisted census row can all
// name which store they came from.
func (a referenceWorkAxis) WorkLegs(id string) []storeref.Leg {
	configured := referenceConfiguredPrefixLegs(a.s)
	if leg, ok := referenceLegByConfiguredIDPrefix(id, configured); ok {
		return []storeref.Leg{leg}
	}
	if prefix := referenceBeadPrefix(id); prefix != "" {
		if store := referenceStoreByPrefix(a.s, prefix); store != nil {
			return []storeref.Leg{referenceWorkLegFor(a.s, store)}
		}
	}
	return referenceUnroutedWorkLegs(a.s)
}

// referenceUnroutedWorkLegs is the legacy scan: the city/HQ store first, then every rig
// work store in rig-name order.
//
// It is reached by an id that matches no configured prefix and no routes.jsonl
// entry — a routes-era id, a hand-minted one, an id from a rig whose prefix was
// later changed — and dropping it would make every such bead unreachable by id.
// That is why this plane declares its work axis to the resolver instead of
// taking the resolver's own rule, which keeps only the rigs whose CONFIGURED
// prefix covers the id and would keep none of these.
func referenceUnroutedWorkLegs(s *Server) []storeref.Leg {
	stores := s.state.BeadStores()
	rigNames := referenceSortedRigNames(stores)
	legs := make([]storeref.Leg, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		legs = append(legs, storeref.Leg{Ref: storeref.WorkRef, Store: cityStore})
	}
	for _, rigName := range rigNames {
		legs = append(legs, storeref.Leg{Ref: storeref.RigRef(rigName), Store: stores[rigName]})
	}
	return legs
}

// referenceWorkLegFor names a work store this plane resolved by route. The ref is for
// diagnostics — a leg that fails is reported by name — so an unrecognized store
// yields an unnamed leg rather than a failure: the store still answers.
func referenceWorkLegFor(s *Server, store beads.Store) storeref.Leg {
	for _, leg := range referenceUnroutedWorkLegs(s) {
		if leg.Store == store {
			return leg
		}
	}
	return storeref.Leg{Store: store}
}

// referenceConfiguredPrefixLegs returns the loaded work stores paired with the id
// prefixes they were CONFIGURED with — the city/HQ prefix first, then each rig
// in config order.
//
// Only stores that are actually loaded are listed: a configured prefix whose
// store is missing must not win a slot, so a shorter loaded prefix can still own
// an id (and otherwise the id is left to the scan).
//
// This walks every rig, which the pre-seam resolver did not: it looked the store
// up only for a rig whose prefix already covered the id and beat the best match
// so far. State.BeadStore is a map read under an RLock, so the widened walk
// costs a lookup per rig and no I/O.
func referenceConfiguredPrefixLegs(s *Server) []storeref.Leg {
	cfg := s.state.Config()
	if cfg == nil {
		return nil
	}
	legs := make([]storeref.Leg, 0, len(cfg.Rigs)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		legs = append(legs, storeref.Leg{
			Ref:    storeref.WorkRef,
			Store:  cityStore,
			Prefix: strings.TrimSpace(config.EffectiveHQPrefix(cfg)),
		})
	}
	for _, rig := range cfg.Rigs {
		store := s.state.BeadStore(rig.Name)
		if store == nil {
			continue
		}
		legs = append(legs, storeref.Leg{
			Ref:    storeref.RigRef(rig.Name),
			Store:  store,
			Prefix: strings.TrimSpace(rig.EffectivePrefix()),
		})
	}
	return legs
}

// referenceLegByConfiguredIDPrefix routes an id on the configured (rig/HQ)
// prefixes with a longest-prefix, exact-or-hyphen match
// (storeref.IDInNamespace). It resolves against the configured prefixes, not
// each store's own IDPrefix, and requires the longest configured prefix to win
// — so it is not the namespace-only, first-match storeref.PrefixOwner. Ties keep
// config order (HQ first), which cannot arise between distinct prefixes: two
// prefixes of equal length never cover the same id.
func referenceLegByConfiguredIDPrefix(id string, configured []storeref.Leg) (storeref.Leg, bool) {
	if strings.TrimSpace(id) == "" {
		return storeref.Leg{}, false
	}
	var best storeref.Leg
	bestLen := -1
	for _, candidate := range configured {
		if !storeref.IDInNamespace(id, candidate.Prefix) || len(candidate.Prefix) <= bestLen {
			continue
		}
		best = candidate
		bestLen = len(candidate.Prefix)
	}
	return best, bestLen >= 0
}

// referenceStoreByPrefix finds the store that owns a bead prefix by checking
// routes.jsonl files in the city and each rig's .beads/ directory, then
// mapping the resolved store path back to the correct store.
func referenceStoreByPrefix(s *Server, prefix string) beads.Store {
	cfg := s.state.Config()
	if cfg == nil {
		return nil
	}
	stores := s.state.BeadStores()
	cityPath := strings.TrimSpace(s.state.CityPath())

	if prefix == config.EffectiveHQPrefix(cfg) {
		if cityStore := s.state.CityBeadStore(); cityStore != nil {
			return cityStore
		}
	}
	for _, rig := range cfg.Rigs {
		if prefix != rig.EffectivePrefix() {
			continue
		}
		if store, exists := stores[rig.Name]; exists {
			return store
		}
		return nil
	}

	// Build rig path → name map for reverse lookup (used by both city
	// and rig route resolution below).
	rigPathToName := make(map[string]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rp := strings.TrimSpace(rig.Path)
		if rp == "" {
			continue
		}
		if !filepath.IsAbs(rp) && cityPath != "" {
			rp = filepath.Join(cityPath, rp)
		}
		rigPathToName[filepath.Clean(rp)] = rig.Name
	}

	// Check city-level routes first.
	if cityPath != "" {
		if storePath, ok := referenceRoutePrefix(cityPath, prefix); ok {
			cleanPath := filepath.Clean(storePath)
			// Route may point to a rig directory — resolve to the rig store.
			if rigName, found := rigPathToName[cleanPath]; found {
				if store, exists := stores[rigName]; exists {
					return store
				}
			}
			// Route points to the city itself (e.g. prefix "mc" → ".").
			if cleanPath == filepath.Clean(cityPath) {
				if cityStore := s.state.CityBeadStore(); cityStore != nil {
					return cityStore
				}
			}
		}
	}

	// Search routes.jsonl in each rig's .beads/ directory.
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) && cityPath != "" {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		storePath, ok := referenceRoutePrefix(rigPath, prefix)
		if !ok {
			continue
		}
		// The resolved store path might point to a different rig
		// (e.g., prefix "gb" in alpha's routes maps to ../beta).
		cleanPath := filepath.Clean(storePath)
		if rigName, found := rigPathToName[cleanPath]; found {
			if store, exists := stores[rigName]; exists {
				return store
			}
		}
		// Fallback: the route pointed back at this rig itself (a self-route).
		// Only then may this rig answer. A route that resolves to some OTHER
		// path — one that is not a registered rig, e.g. a relocated class store
		// directory — says the prefix does NOT live here, so it must fall
		// through to the caller's candidate scan rather than pin the wrong store.
		if cleanPath == filepath.Clean(rigPath) {
			if store, exists := stores[rig.Name]; exists {
				return store
			}
		}
	}
	return nil
}

// referenceSortedRigNames returns rig names from the store map in deterministic sorted order,
// deduplicating rigs that share the same underlying store (e.g. file provider mode).
func referenceSortedRigNames(stores map[string]beads.Store) []string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	// Deduplicate by store identity — when multiple rigs share the same
	// store instance (file provider), only keep the first rig name to
	// prevent duplicate results in aggregate queries.
	seen := make(map[beads.Store]bool, len(names))
	deduped := names[:0]
	for _, name := range names {
		s := stores[name]
		if seen[s] {
			continue
		}
		seen[s] = true
		deduped = append(deduped, name)
	}
	return deduped
}

// referenceBeadPrefix extracts the config-free heuristic prefix from a bead ID.
func referenceBeadPrefix(id string) string {
	return referenceBeadPrefixHeuristic(id)
}

// referenceRoutePrefix reads routes.jsonl from a rig's .beads/ directory and
// resolves the given prefix to an absolute store path.
func referenceRoutePrefix(rigPath, prefix string) (string, bool) {
	routesPath := filepath.Join(rigPath, ".beads", "routes.jsonl")
	data, err := os.ReadFile(routesPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Prefix string `json:"prefix"`
			Path   string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Prefix == prefix {
			resolved := entry.Path
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(rigPath, resolved)
			}
			return resolved, true
		}
	}
	return "", false
}

// referenceStoreClassHolds returns the classes store holds on this city, or nil on a
// single-store city (where the one store holds every class). A class binding
// holds the classes it serves; a work store — the city's or a rig's — holds
// work plus every class no binding serves.
func referenceStoreClassHolds(s *Server, store beads.Store) []coordclass.Class {
	topo := referenceResidencyTopology(s)
	if topo.IsSingleStore() {
		return nil
	}
	for _, binding := range topo.Bindings {
		if binding.Leg.Store == store { // residency:allow — identifies which binding the already-resolved owner store is, to read its classes; resolves no bead
			return binding.Classes
		}
	}
	holds := []coordclass.Class{coordclass.ClassWork}
	for _, class := range coordclass.Classes() {
		if _, served := topo.BindingFor(class); class.IsInfrastructure() && !served {
			holds = append(holds, class)
		}
	}
	return holds
}

// referenceBindingReservedPrefixes is the reserved id namespaces store holds as a class
// binding: the prefixes of every binding the residency topology serves from it,
// as cmd/gc's readyLegReservedPrefixes reads them for `gc ready`. The
// cross-store ready rule takes the binding's own verdict on a blocker in one of
// them, which no other store can hold, and reads every copy of any other.
func referenceBindingReservedPrefixes(s *Server, store beads.Store) []string {
	var prefixes []string
	for _, binding := range referenceResidencyTopology(s).Bindings {
		if binding.Leg.Store == store { // residency:allow — matches the already-resolved graph store to its binding, to read its namespaces; no store is ordered, opened or read
			prefixes = append(prefixes, binding.Prefixes...)
		}
	}
	return prefixes
}

func referenceBeadPrefixHeuristic(beadID string) string {
	beadID = strings.TrimSpace(beadID)
	lastIdx := strings.LastIndex(beadID, "-")
	if lastIdx <= 0 {
		return ""
	}
	suffix := beadID[lastIdx+1:]
	if suffix == "" {
		return strings.ToLower(strings.TrimRight(beadID[:lastIdx], "-"))
	}
	base := suffix
	if dot := strings.IndexByte(suffix, '.'); dot > 0 {
		base = suffix[:dot]
	}
	if referenceIsBeadNumeric(base) || referenceIsBeadHash(base) {
		return strings.ToLower(strings.TrimRight(beadID[:lastIdx], "-"))
	}
	firstIdx := strings.Index(beadID, "-")
	if firstIdx <= 0 {
		return ""
	}
	return strings.ToLower(beadID[:firstIdx])
}

func referenceIsBeadNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// referenceIsBeadHash is only the config-free BeadPrefix heuristic's suffix gate. It
// intentionally rejects longer all-letter words so prose like
// "code-review-please" falls back to the first dash instead of being treated
// as a hyphenated rig prefix. Config-aware routing must use BeadPrefixForCity
// and the configured-prefix matchers instead.
func referenceIsBeadHash(s string) bool {
	if len(s) < 3 || len(s) > 8 {
		return false
	}
	hasDigit := len(s) == 3
	for _, c := range s {
		if c >= '0' && c <= '9' {
			hasDigit = true
			continue
		}
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return hasDigit
}
