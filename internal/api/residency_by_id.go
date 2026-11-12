package api

// The API plane's by-id residency, resolved through internal/storeref.
//
// One question — "which store holds this bead?" — asked once, by the residency
// resolver, for every by-id handler on this surface. Before this, the answer was
// a hand-rolled candidate list this package built and eight handler loops
// probed, each restating the ordering, the first-answer-wins rule and the
// fail-fast policy. The list was also binding-BLIND outside a reserved
// namespace: `gc storage migrate` preserves ids, so a bead relocated into the
// class binding kept its work-era prefix, the prefix arms routed it to a work
// store that holds at most the migration's frozen copy, and every read AND
// write of it 404'd or landed on the stale row (ga-axin6, #5245).
//
// # What the resolver owns, and what this plane still owns
//
// The BINDING axis is the resolver's: the binding leads for an id inside its
// reserved namespace because it is the sole minter, and is a tolerated
// residence PROBE for an id outside every namespace because minting is not the
// only way to hold.
//
// The WORK axis stays here, because this plane routes it rather than probing
// it: longest configured prefix, then a routes.jsonl lookup, then a scan of
// every work store. storeref models a probed work axis instead, and swapping
// one for the other would have made a routes.jsonl-routed id and an id matching
// no configured prefix unreachable, and would have put the city work store
// ahead of the rig store that mints the id — so a city-store outage would answer
// for reads the rig was serving. apiWorkAxis declares this plane's answer to the
// resolver (storeref.ByID.WorkAxis) instead, and is consulted only on the arm
// where it is the right question.

import (
	"errors"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// resolveBeadOwner pins the store that holds id and reads the bead from it.
//
// It is the whole by-id front door of this surface: every handler calls it,
// gets back the store to write through and the row it just read, and returns
// the error verbatim. Eight handlers each restating "probe in order, stop at
// the first answer, a read failure is not a miss" is what the residency
// contract exists to end, and the restatement is the part that used to drift.
//
// Resolution has two miss shapes and this collapses both to one 404. When the
// plan ends in the work residual the store comes back UNPROBED and the Get
// below is the only read — which is what keeps a single-store city's read count
// and error message unchanged. When the plan ends in a leg the resolver read, a
// clean miss everywhere is already beads.ErrNotFound and there is no store to
// hand back.
//
// Anything else is a store that could not be read, and it surfaces as a 500
// with the store named: an unreachable store reported as a missing bead is the
// root-loss shape this lane exists to prevent.
func (s *Server) resolveBeadOwner(id string) (beads.ResidentStore, beads.Bead, error) {
	if strings.TrimSpace(id) == "" {
		// The router never dispatches a by-id handler with an empty path
		// segment, so this is unreachable in practice. It answers "absent"
		// rather than surfacing the resolver's malformed-intent error because
		// that is what the pre-resolver candidate scan produced, and a defensive
		// branch is a poor place to change a status code.
		return beads.ResidentStore{}, beads.Bead{}, apierr.BeadNotFound.Msg("bead " + id + " not found")
	}
	st := s.state.Stores()
	plan, err := storeref.Plan(storeref.ByID{ID: id, WorkAxis: apiWorkAxis{s: s, st: st}}, st.Topology())
	if err != nil {
		return beads.ResidentStore{}, beads.Bead{}, apierr.Internal.Msg(err.Error())
	}
	owner, err := storeref.ResolveOwnerRow(plan, id)
	if err != nil {
		return beads.ResidentStore{}, beads.Bead{}, beadOwnerError(id, err)
	}
	if owner.Read {
		// A probe answered, and its read is the read this handler needed. Doing
		// it again would double every by-id operation against the binding.
		return owner.Resident, owner.Bead, nil
	}
	b, err := owner.Resident.Get(id)
	if err != nil {
		return beads.ResidentStore{}, beads.Bead{}, beadOwnerError(id, err)
	}
	return owner.Resident, b, nil
}

// apiWorkAxis is this plane's by-id work-axis router, declared to the resolver.
//
// It is consulted for one arm only — an id no binding's reserved namespace
// claims — and that is what makes it correct as well as cheap. Inside a
// namespace the binding is the authority and the work stores are a probe list
// behind it, so running the prefix arms there would let a rig configured with a
// reserved prefix capture an id the binding mints; it would also pay a
// routes.jsonl disk read on every molecule-id read of a relocated city.
//
// It routes over st, the epoch the plan is planned over, so every ref it
// answers names a leg of that plan's topology.
type apiWorkAxis struct {
	s  *Server
	st storeref.Stores
}

// WorkLegsForID returns the work legs that can hold id, by ref, in this
// plane's own order: the store whose configured prefix owns the id, else the
// store its routes.jsonl entry names, else the full scan — the city/HQ store
// ahead of every rig work store.
//
// This is the pre-resolver candidate list. It is stated as refs, which the
// resolver maps to the epoch's legs, so a partial answer, a failure and a
// persisted census row can all name which store they came from.
func (a apiWorkAxis) WorkLegsForID(id string) []storeref.StoreRef {
	loaded := loadedWorkLegs(a.st)
	if ref, ok := resolveLegByConfiguredIDPrefix(id, configuredPrefixRoutes(a.s.state.Config(), loaded)); ok {
		return []storeref.StoreRef{ref}
	}
	if prefix := beadPrefix(id); prefix != "" {
		if ref, ok := a.s.resolveRefByPrefix(prefix, loaded); ok {
			return []storeref.StoreRef{ref}
		}
	}
	return unroutedWorkRefs(loaded)
}

// loadedWorkLegs is the set of work legs st's epoch holds a store for, by ref:
// the city's when it has a work store, and every rig the epoch opened.
func loadedWorkLegs(st storeref.Stores) map[storeref.StoreRef]bool {
	topo := st.Topology()
	loaded := make(map[storeref.StoreRef]bool, len(topo.Rigs)+1)
	if topo.Work.Store != nil {
		loaded[storeref.WorkRef] = true
	}
	for _, rig := range st.Rigs() {
		loaded[storeref.RigRef(rig.Name)] = true
	}
	return loaded
}

// unroutedWorkRefs is the legacy scan: the city/HQ store first, then every rig
// work store in rig-name order. The city is listed once: it is not a rig,
// though State.BeadStores() lists it under the city's name (E4).
//
// It is reached by an id that matches no configured prefix and no routes.jsonl
// entry — a routes-era id, a hand-minted one, an id from a rig whose prefix was
// later changed — and dropping it would make every such bead unreachable by id.
// That is why this plane declares its work axis to the resolver instead of
// taking the resolver's own rule, which keeps only the rigs whose CONFIGURED
// prefix covers the id and would keep none of these. Rigs sharing one store
// are read once: the plan keeps the first leg over a store (storeref's
// dedupe), as the scan kept the first rig name.
func unroutedWorkRefs(loaded map[storeref.StoreRef]bool) []storeref.StoreRef {
	refs := make([]storeref.StoreRef, 0, len(loaded))
	for ref := range loaded {
		if ref != storeref.WorkRef {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	if loaded[storeref.WorkRef] {
		refs = append([]storeref.StoreRef{storeref.WorkRef}, refs...)
	}
	return refs
}

// prefixRoute is a loaded work leg and the id prefix it was CONFIGURED with.
type prefixRoute struct {
	ref    storeref.StoreRef
	prefix string
}

// configuredPrefixRoutes returns the loaded work legs paired with the id
// prefixes they were CONFIGURED with — the city/HQ prefix first, then each rig
// in config order.
//
// Only legs that are actually loaded are listed: a configured prefix whose
// store is missing must not win a slot, so a shorter loaded prefix can still own
// an id (and otherwise the id is left to the scan).
func configuredPrefixRoutes(cfg *config.City, loaded map[storeref.StoreRef]bool) []prefixRoute {
	if cfg == nil {
		return nil
	}
	routes := make([]prefixRoute, 0, len(cfg.Rigs)+1)
	if loaded[storeref.WorkRef] {
		routes = append(routes, prefixRoute{ref: storeref.WorkRef, prefix: strings.TrimSpace(config.EffectiveHQPrefix(cfg))})
	}
	for _, rig := range cfg.Rigs {
		if ref := storeref.RigRef(rig.Name); loaded[ref] {
			routes = append(routes, prefixRoute{ref: ref, prefix: strings.TrimSpace(rig.EffectivePrefix())})
		}
	}
	return routes
}

// resolveLegByConfiguredIDPrefix routes an id on the configured (rig/HQ)
// prefixes with a longest-prefix, exact-or-hyphen match
// (storeref.IDInNamespace). It resolves against the configured prefixes, not
// each store's own IDPrefix, and requires the longest configured prefix to win
// — so it is not the namespace-only, first-match storeref.PrefixOwner. Ties keep
// config order (HQ first), which cannot arise between distinct prefixes: two
// prefixes of equal length never cover the same id.
func resolveLegByConfiguredIDPrefix(id string, configured []prefixRoute) (storeref.StoreRef, bool) {
	if strings.TrimSpace(id) == "" {
		return "", false
	}
	var best storeref.StoreRef
	bestLen := -1
	for _, candidate := range configured {
		if !storeref.IDInNamespace(id, candidate.prefix) || len(candidate.prefix) <= bestLen {
			continue
		}
		best = candidate.ref
		bestLen = len(candidate.prefix)
	}
	return best, bestLen >= 0
}

// beadOwnerError renders a resolution failure as this surface's error.
//
// Absence is the 404 every by-id handler already produced; anything else is a
// store that could not be read, and it must not be reported as a missing bead.
func beadOwnerError(id string, err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return apierr.BeadNotFound.Msg("bead " + id + " not found")
	}
	return apierr.Internal.Msg(err.Error())
}
