package main

import (
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
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
// The cut trusts the claim read, so where that read lags the probe instead
// it holds a just-released row back until the claim read catches up: a class
// binding's cache, which has no feed, re-scans every 30 to 120 s, and a
// migrated relic's frozen work-ledger copy that still reads assigned holds its
// live binding twin back while it stays so. Nothing drains meanwhile: the same
// stale read keeps the stale claimer on the resume tier (ga-ylpbsy.88 would
// compare the two copies' update times instead). Rows match by bead ID because
// a demand row and its claim can come from a split city's ledger and binding,
// which demand treats as one scope (normalizeDemandStoreRef).

// claimedWorkIDs returns the IDs of a pass's assigned-work rows that carry an
// assignee, live or not: rows the serving rules never count as demand. A
// claim whose claimant died stays out of demand until the dead-assignee sweep
// releases it; the next read counts it then.
func claimedWorkIDs(assigned []beads.Bead) map[string]struct{} {
	var ids map[string]struct{}
	for _, wb := range assigned {
		id := strings.TrimSpace(wb.ID)
		if id == "" || strings.TrimSpace(wb.Assignee) == "" {
			continue
		}
		if ids == nil {
			ids = make(map[string]struct{})
		}
		ids[id] = struct{}{}
	}
	return ids
}

// claimedDemandCut is the default-probe demand one template lost to claims
// in a pass: the claimed rows in demand order, and the count before and after.
type claimedDemandCut struct {
	WorkBeadIDs []string
	Read        int
	Unclaimed   int
}

// withoutClaimedDemand returns the default probe's counts and demand without
// the rows claimed names, and what it cut by template. It runs on the probe's
// own read, where a template's count is its row count. It never edits its
// arguments; with nothing to cut it returns them unchanged.
func withoutClaimedDemand(counts map[string]int, demand map[string]scaleCheckDemand, claimed map[string]struct{}) (map[string]int, map[string]scaleCheckDemand, map[string]claimedDemandCut) {
	if len(claimed) == 0 {
		return counts, demand, nil
	}
	var cuts map[string]claimedDemandCut
	outCounts, outDemand := counts, demand
	for template, entry := range demand {
		var keep, cut []string
		for i, id := range entry.WorkBeadIDs {
			if _, isClaimed := claimed[strings.TrimSpace(id)]; !isClaimed {
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
