package main

import (
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The default probe's pairing guard: work a v2 pass reads as claimed is
// never also new pool demand.
//
// A v2 pass reads the same work twice. Its assigned-work read (the cache
// tier) sees a claim within about a second and puts the claiming seat on the
// resume tier, where it no longer stands in for the bead as in-flight or
// protected capacity (computePoolDesiredStatesParkedAt). Its default probe
// reads the ready rows, which on a leg the pass serves from the external-reads
// lane's recording can be up to three patrols old. Between a claim and the
// next recording the probe still lists the claimed bead as open and
// unassigned, so the bead counted twice: its claimer resumed it, and a second
// seat was minted for it, started, found nothing to claim and drained. That
// was the dominant shape of session.demand_claim_divergence in production
// (ga-ylpbsy.84).
//
// claimsAfterRun is this guard for a custom scale_check, which publishes a
// bare count and so can only be cut by count. The default probe names its
// rows, so this cut drops exactly the rows the claim read shows assigned: the
// serving rules count only unassigned rows (demandRowServable), so where the
// probe's read is the older one, a row in both reads is its copy of a claim,
// and a row both reads agree is unassigned is never dropped. Legacy needs no
// cut: its probe is a live read taken after its claim read.
//
// A claim holds a row only in the scope it resides in (claimStoreRef): a
// rig's store, or the city, whose work ledger and class bindings a split city
// reads as one scope. A same-ID bead in another scope is still unassigned
// work. A row the probe read from a class binding is held only by a binding's
// claim. `gc storage migrate` carries a bead into the binding under its
// work-ledger ID and can leave the ledger a frozen copy of it
// (class_store_cache.go): a release, reopen or reassign in the binding leaves
// the row claimable while that copy still reads assigned.
//
// The cut trusts the claim read, so where that read lags the probe instead
// it holds a just-released row back until the claim read catches up: a cache
// sees a write that emitted no event at its next re-scan, every 30 to 120 s.
// While the stale claimer's session lasts, the same stale read keeps it on the
// resume tier, so nothing drains meanwhile (ga-ylpbsy.88 would compare the two
// reads' update times instead).

// demandClaims is a pass's claims: its assigned-work rows that carry an
// assignee, live or not, which the serving rules never count as demand, by
// the scope the claim resides in (claimStoreRef) and bead ID. A claim is true
// when a class binding holds it. A claim whose claimant died stays out of
// demand until the dead-assignee sweep releases it; the next read counts it
// then.
type demandClaims map[storeScopedBeadKey]bool

// demandClaimsOf returns the claims among a pass's assigned-work rows; refs
// holds each row's store ref by index (demandView.AssignedStoreRefs).
func demandClaimsOf(assigned []beads.Bead, refs []string) demandClaims {
	var claims demandClaims
	for i, wb := range assigned {
		id := strings.TrimSpace(wb.ID)
		if id == "" || strings.TrimSpace(wb.Assignee) == "" {
			continue
		}
		ref := ""
		if i < len(refs) {
			ref = refs[i]
		}
		if claims == nil {
			claims = make(demandClaims)
		}
		key := storeScopedBeadKey{StoreRef: claimStoreRef(ref), ID: id}
		claims[key] = claims[key] || storeref.IsClassRef(ref)
	}
	return claims
}

// holds reports whether a claim holds entry's probe row id: a claim on the
// same bead in the scope the probe read the row from, and for a row read from
// a class binding, one a binding holds.
func (c demandClaims) holds(entry scaleCheckDemand, id string) bool {
	inBinding, claimed := c[storeScopedBeadKey{StoreRef: claimStoreRef(entry.StoreRefs[id]), ID: strings.TrimSpace(id)}]
	return claimed && (inBinding || !entry.BindingResident[id])
}

// claimedDemandCut is the default-probe demand one template lost to claims
// in a pass: the claimed rows in demand order, and the count before and after.
type claimedDemandCut struct {
	WorkBeadIDs []string
	Read        int
	Unclaimed   int
}

// withoutClaimedDemand returns the default probe's counts and demand without
// the rows claimed holds, and what it cut by template. It runs on the probe's
// own read, where a template's count is its row count. It never edits its
// arguments; with nothing to cut it returns them unchanged.
func withoutClaimedDemand(counts map[string]int, demand map[string]scaleCheckDemand, claimed demandClaims) (map[string]int, map[string]scaleCheckDemand, map[string]claimedDemandCut) {
	if len(claimed) == 0 {
		return counts, demand, nil
	}
	var cuts map[string]claimedDemandCut
	outCounts, outDemand := counts, demand
	for template, entry := range demand {
		var keep, cut []string
		for i, id := range entry.WorkBeadIDs {
			if !claimed.holds(entry, id) {
				if cut != nil {
					keep = append(keep, id)
				}
				continue
			}
			if cut == nil {
				keep = append(make([]string, 0, len(entry.WorkBeadIDs)-1), entry.WorkBeadIDs[:i]...)
			}
			cut = append(cut, strings.TrimSpace(id))
		}
		if cut == nil {
			continue
		}
		if cuts == nil {
			cuts = make(map[string]claimedDemandCut)
			outCounts, outDemand = make(map[string]int, len(counts)), cloneScaleCheckDemandMap(demand)
			maps.Copy(outCounts, counts)
		}
		read := counts[template]
		entry.WorkBeadIDs = keep
		entry.Count = max(entry.Count-len(cut), 0)
		outDemand[template] = entry
		outCounts[template] = max(read-len(cut), 0)
		cuts[template] = claimedDemandCut{WorkBeadIDs: cut, Read: read, Unclaimed: outCounts[template]}
	}
	return outCounts, outDemand, cuts
}
