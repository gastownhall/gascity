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
func (s *Server) resolveBeadOwner(id string) (beads.Store, beads.Bead, error) {
	if strings.TrimSpace(id) == "" {
		// The router never dispatches a by-id handler with an empty path
		// segment, so this is unreachable in practice. It answers "absent"
		// rather than surfacing the resolver's malformed-intent error because
		// that is what the pre-resolver candidate scan produced, and a defensive
		// branch is a poor place to change a status code.
		return nil, beads.Bead{}, apierr.BeadNotFound.Msg("bead " + id + " not found")
	}
	plan, err := storeref.Plan(storeref.ByID{ID: id, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil, beads.Bead{}, apierr.Internal.Msg(err.Error())
	}
	owner, err := storeref.ResolveOwnerRow(plan, id)
	if err != nil {
		return nil, beads.Bead{}, beadOwnerError(id, err)
	}
	if owner.Read {
		// A probe answered, and its read is the read this handler needed. Doing
		// it again would double every by-id operation against the binding.
		return owner.Store, owner.Bead, nil
	}
	b, err := owner.Store.Get(id)
	if err != nil {
		return nil, beads.Bead{}, beadOwnerError(id, err)
	}
	return owner.Store, b, nil
}

// apiWorkAxis is this plane's by-id work-axis router, declared to the resolver.
//
// It is consulted for one arm only — an id no binding's reserved namespace
// claims — and that is what makes it correct as well as cheap. Inside a
// namespace the binding is the authority and the work stores are a probe list
// behind it, so running the prefix arms there would let a rig configured with a
// reserved prefix capture an id the binding mints; it would also pay a
// routes.jsonl disk read on every molecule-id read of a relocated city.
type apiWorkAxis struct{ s *Server }

// WorkLegsForID returns the work stores that can hold id, in this plane's own
// order: the store whose configured prefix owns the id, else the store its
// routes.jsonl entry names, else the full scan — the city/HQ store ahead of
// every rig work store.
//
// This is the pre-resolver candidate list verbatim. It is stated as legs rather
// than stores so a partial answer, a failure and a persisted census row can all
// name which store they came from.
func (a apiWorkAxis) WorkLegsForID(id string) []storeref.Leg {
	configured := a.s.configuredPrefixLegs()
	if leg, ok := resolveLegByConfiguredIDPrefix(id, configured); ok {
		return []storeref.Leg{leg}
	}
	if prefix := beadPrefix(id); prefix != "" {
		if store := a.s.resolveStoreByPrefix(prefix); store != nil {
			return []storeref.Leg{a.s.workLegFor(store)}
		}
	}
	return a.s.unroutedWorkLegs()
}

// unroutedWorkLegs is the legacy scan: the city/HQ store first, then every rig
// work store in rig-name order.
//
// It is reached by an id that matches no configured prefix and no routes.jsonl
// entry — a routes-era id, a hand-minted one, an id from a rig whose prefix was
// later changed — and dropping it would make every such bead unreachable by id.
// That is why this plane declares its work axis to the resolver instead of
// taking the resolver's own rule, which keeps only the rigs whose CONFIGURED
// prefix covers the id and would keep none of these.
func (s *Server) unroutedWorkLegs() []storeref.Leg {
	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	legs := make([]storeref.Leg, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		legs = append(legs, storeref.Leg{Ref: storeref.WorkRef, Store: cityStore})
	}
	for _, rigName := range rigNames {
		legs = append(legs, storeref.Leg{Ref: storeref.RigRef(rigName), Store: stores[rigName]})
	}
	return legs
}

// workLegFor names a work store this plane resolved by route. The ref is for
// diagnostics — a leg that fails is reported by name — so an unrecognized store
// yields an unnamed leg rather than a failure: the store still answers.
func (s *Server) workLegFor(store beads.Store) storeref.Leg {
	for _, leg := range s.unroutedWorkLegs() {
		if leg.Store == store {
			return leg
		}
	}
	return storeref.Leg{Store: store}
}

// configuredPrefixLegs returns the loaded work stores paired with the id
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
func (s *Server) configuredPrefixLegs() []storeref.Leg {
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

// resolveLegByConfiguredIDPrefix routes an id on the configured (rig/HQ)
// prefixes with a longest-prefix, exact-or-hyphen match
// (storeref.IDInNamespace). It resolves against the configured prefixes, not
// each store's own IDPrefix, and requires the longest configured prefix to win
// — so it is not the namespace-only, first-match storeref.PrefixOwner. Ties keep
// config order (HQ first), which cannot arise between distinct prefixes: two
// prefixes of equal length never cover the same id.
func resolveLegByConfiguredIDPrefix(id string, configured []storeref.Leg) (storeref.Leg, bool) {
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
