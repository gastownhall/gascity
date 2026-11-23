package main

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// A cross-store demand leg's claim view (#5857).
//
// The controller counts a city-store row as demand for a rig pool on the
// premise that a seat's claim can win it. The claim's CAS resolves the row's
// id in the pool's own store — `bd update <id> --claim` in the seat's rig
// directory — so a row whose owner copy there is closed, in progress or
// assigned is refused before any write, and counting it mints a seat that
// reads it, is refused, drains and is replaced, every tick, for as long as the
// stale replica stays ready. The claim view is that pool's own store: asking
// it Get(id) answers the claim's own question, because on a bd-backed store
// Get is `bd show <id>` in the same directory, which resolves ids the same
// way (local first, then prefix route) as the claim's update does.
//
// The leg carries the view as a beads.StoreReader, the read interface a
// store passes as: demand only reads it, and the typed store boundary
// (storeclass) keeps raw stores in declared positions outside the residency
// layer on a shrink-only baseline. The view a pass actually carries is not
// the bare store but its federated work leg, minted by the residency layer
// (demandClaimView): the probe's by-id reads are residence probes — the row
// was counted from another store's leg, so the pool's own store may hold no
// copy of it — and on a split city the split-strict gate reads a federated
// leg's reads exactly so.

// crossStoreDemandLegs stamps the city legs of one rig pool's cross-store
// probe with that pool's claim view: the store a seat of the leg's template
// resolves the leg's ids in when it claims. A leg without one (an own leg, a
// city-scoped pool's leg, a store-scoped control dispatcher's, or a pool
// whose view could not be minted) keeps today's behavior unchanged.
func crossStoreDemandLegs(legs []defaultScaleCheckTarget, ownStore beads.StoreReader) []defaultScaleCheckTarget {
	if ownStore == nil || len(legs) == 0 {
		return legs
	}
	out := make([]defaultScaleCheckTarget, len(legs))
	copy(out, legs)
	for i := range out {
		out[i].claimView = ownStore
	}
	return out
}

// demandClaimView mints one pool's claim view over the epoch scopeStores
// builds from the pool's own store: the WorkScope plan's leg, a read-only
// federated leg the residency layer minted (storeref.EachTypedLeg), so the
// probe's by-id reads of the pool's own store are the residence probes the
// split-strict gate filters rather than class misroutes — the row was
// counted from another store's leg, and this store may hold no copy of it,
// whatever its class. A plan that cannot be made mints no view: the pool's
// cross-store legs keep today's counting, which may count a stale replica but
// never drops a row the pass could not prove unclaimable.
func demandClaimView(stores storeref.Stores) beads.StoreReader {
	plan, err := stores.Plan(storeref.WorkFederation{Kind: storeref.WorkScope, Scope: storeref.WorkRef})
	if err != nil {
		return nil
	}
	var view beads.StoreReader
	storeref.EachTypedLeg(plan, func(l storeref.TypedLeg) {
		if view == nil {
			view = l.Reader
		}
	})
	return view
}

// claimViewVerdict is what a row's claim view said about it.
type claimViewVerdict int

const (
	// claimViewWin: the claim view holds the row open and unassigned, or holds
	// no copy of it at all, so the claim can win it.
	claimViewWin claimViewVerdict = iota
	// claimViewShadow: the claim view holds the row closed, in progress or
	// assigned, so the claim CAS will refuse it; the row is not demand this
	// pool's seat can claim.
	claimViewShadow
	// claimViewUnknown: the claim view could not answer, so the row's
	// claimability is unproven.
	claimViewUnknown
)

// claimViewKey identifies one question the pass might ask twice: which view,
// which row. The store groups a template's demand legs share a claim view, so
// the same (view, id) can be asked about per template; the probe reads it once.
type claimViewKey struct {
	view beads.StoreReader
	id   string
}

// claimViewAnswer is one view's answer to one such question, cached for the
// rest of the pass.
type claimViewAnswer struct {
	verdict claimViewVerdict
	err     error
}

// claimViewProbe asks one demand pass's claim views whether the claim can win
// each cross-store row it counts.
type claimViewProbe struct {
	// stats is the pass's summary for the demand-pass trace.
	stats demandClaimViewStats
	// shadowedIDs records which ids stats.ShadowedIDs already names.
	shadowedIDs map[string]struct{}
	// answers caches each (view, id) the pass has already asked, so a row two
	// of a pool's demand legs both count costs one claim-view read, not two
	// (the design's per-pass cost bound).
	answers map[claimViewKey]claimViewAnswer
}

// demandClaimViewStats summarizes one demand pass's claim-view probes: how
// many reads it issued, how many rows it withheld as shadowed, and which ids
// those were. It is recorded in the demand-pass trace so a standing stale
// replica is visible to `gc trace` without stderr noise on every tick.
type demandClaimViewStats struct {
	// Probed is how many claim-view reads the pass issued.
	Probed int
	// Shadowed is how many cross-store rows the pass withheld because the
	// claim view holds them in a state the claim cannot win.
	Shadowed int
	// ShadowedIDs names the withheld rows.
	ShadowedIDs []string
}

// check returns the claim view's verdict for id: whether the row's claim —
// which resolves the id in this view — can win it. ErrNotFound is a win: the
// view is the pool's own store and it holds no row to refuse the claim, so the
// row is city-delivered work the claim takes as new. An open, unassigned row
// is a win too; any other held state the claim CAS refuses before any write.
// A read that fails answers nothing (claimViewUnknown, the error carried for
// the pass's partial marking). Each (view, id) is read at most once per pass.
func (p *claimViewProbe) check(view beads.StoreReader, id string) (claimViewVerdict, error) {
	if p == nil || view == nil {
		return claimViewWin, nil
	}
	key := claimViewKey{view: view, id: id}
	if answer, cached := p.answers[key]; cached {
		return answer.verdict, answer.err
	}
	b, err := view.Get(id)
	p.stats.Probed++
	answer := claimViewAnswer{verdict: claimViewUnknown, err: err}
	switch {
	case errors.Is(err, beads.ErrNotFound):
		answer.verdict, answer.err = claimViewWin, nil
	case err == nil && strings.EqualFold(strings.TrimSpace(b.Status), "open") && strings.TrimSpace(b.Assignee) == "":
		answer.verdict = claimViewWin
	case err == nil:
		answer.verdict = claimViewShadow
	}
	if p.answers == nil {
		p.answers = make(map[claimViewKey]claimViewAnswer)
	}
	p.answers[key] = answer
	return answer.verdict, answer.err
}

// noteShadowed records that the pass withheld id as shadowed.
func (p *claimViewProbe) noteShadowed(id string) {
	if p == nil {
		return
	}
	if p.shadowedIDs == nil {
		p.shadowedIDs = make(map[string]struct{})
	}
	if _, seen := p.shadowedIDs[id]; !seen {
		p.shadowedIDs[id] = struct{}{}
		p.stats.ShadowedIDs = append(p.stats.ShadowedIDs, id)
	}
	p.stats.Shadowed++
}
