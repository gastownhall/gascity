package main

import (
	"errors"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// A rig pool's default probe also reads the city legs (vp-s37 / vp-cl4), so a
// row routed to the pool but delivered to the city store wakes it. A seat
// claims that row with a CAS run in its own leg's directory, which resolves the
// id in the pool's own store first. When that store holds its own copy of the
// id — the owning store of a bead the city holds only a stale replica of — the
// CAS answers for that copy, not the city row the probe counted. A closed or
// already-held copy refuses every seat the city row spawns, and the city row
// stays ready, so demand and claim never converge (#5857).
//
// The claim view is the pool's own store, asked the claim's question: a row a
// cross-store leg counts is demand only when the claim view has no copy of the
// id (the work genuinely lives elsewhere) or holds it open and unassigned (the
// claim can win it). Get resolves an id the way the claim does (bd show and bd
// update --claim share bd's id resolution), so the probe is the claim's own
// view, not a separate rule about where a bead lives.

// crossStoreDemandLegs marks a pool's cross-store work-ledger legs with the
// store a seat of that pool resolves their ids in when it claims: the pool's
// own store. A class-binding leg stays unmarked: its rows are coordination-class
// rows no work ledger holds on a split city, so the claim resolves them in the
// binding they were read from, and asking the pool's ledger would be a misroute.
func crossStoreDemandLegs(legs []defaultScaleCheckTarget, own defaultScaleCheckTarget) []defaultScaleCheckTarget {
	out := make([]defaultScaleCheckTarget, len(legs))
	for i, leg := range legs {
		if !leg.classBinding {
			leg.claimView = own.store
			leg.claimViewKey = own.storeKey
		}
		out[i] = leg
	}
	return out
}

// demandClaimView is the store a template's seats claim a cross-store leg's
// rows in, and the store key that names it.
type demandClaimView struct {
	reader beads.StoreReader
	key    string
}

// demandClaimViewKey is one id asked of one claim view.
type demandClaimViewKey struct {
	viewKey string
	id      string
}

// demandClaimViewVerdict is the claim view's answer for one id.
type demandClaimViewVerdict struct {
	claimable bool
	err       error
}

// demandClaimViews asks the claim views of one demand pass, each (view, id)
// once, and keeps what the pass shadowed for its trace.
type demandClaimViews struct {
	verdicts map[demandClaimViewKey]demandClaimViewVerdict
	// probed and shadowed count the cross-store rows the pass asked about and
	// the ones it did not count; shadowedIDs names the latter.
	probed      int
	shadowed    int
	shadowedIDs []string
}

func newDemandClaimViews() *demandClaimViews {
	return &demandClaimViews{verdicts: make(map[demandClaimViewKey]demandClaimViewVerdict)}
}

// claimable reports whether a seat claiming id against view (named viewKey)
// can win it. A non-nil error means the view could not be read; the row is
// then not claimable either, and the caller reports the template partial.
func (v *demandClaimViews) claimable(view beads.StoreReader, viewKey, id string) (bool, error) {
	key := demandClaimViewKey{viewKey: viewKey, id: id}
	verdict, ok := v.verdicts[key]
	if !ok {
		verdict = askDemandClaimView(view, id)
		v.verdicts[key] = verdict
	}
	v.probed++
	if !verdict.claimable {
		v.shadowed++
		v.shadowedIDs = append(v.shadowedIDs, id)
	}
	return verdict.claimable, verdict.err
}

// askDemandClaimView reads id from view the way the claim resolves it.
func askDemandClaimView(view beads.StoreReader, id string) demandClaimViewVerdict {
	b, err := view.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return demandClaimViewVerdict{claimable: true}
	}
	if err != nil {
		return demandClaimViewVerdict{err: err}
	}
	return demandClaimViewVerdict{claimable: demandClaimViewAdmits(b)}
}

// demandClaimViewAdmits reports whether the claim view's own copy of a row is
// one the claim CAS accepts: open and unassigned.
func demandClaimViewAdmits(b beads.Bead) bool {
	return strings.TrimSpace(b.Status) == "open" && strings.TrimSpace(b.Assignee) == ""
}

// trace records the pass's claim-view probes on the demand trace, so a
// standing stale replica is visible to gc trace without a line on every tick.
func (v *demandClaimViews) trace(cache *readyDemandCache) {
	if v.probed == 0 {
		return
	}
	pass, point := cache.readTrace()
	if pass == nil {
		return
	}
	ids := slices.Clone(v.shadowedIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	pass.cycle.RecordControllerOperation(TraceSiteDemandSnapshot, TraceReasonRetained, TraceOutcomeComplete, "demand_snapshot.claim_view", 0, map[string]any{
		"point":    point,
		"probed":   v.probed,
		"shadowed": v.shadowed,
		"ids":      ids,
	})
}
