package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// These replay the shapes the production event log showed for demand-spawned
// seats that started, found nothing in `gc hook --claim`, and drained
// (session.demand_claim_divergence, 2026-10-31..11-02). In each, the default
// probe still listed a bead as open and unassigned after the pass's
// cache-tier assigned-work read showed it claimed: the probe's rows on a
// recorded leg lag a claim by up to a patrol, while the claim puts its seat on
// the resume tier at once, where it no longer stands in for the bead as
// protected capacity. Each test runs a v2 pass's order: gather's cut, the
// merge, then pool demand.

var claimedWorkNow = time.Date(2026, 11, 2, 12, 30, 55, 0, time.UTC)

// claimedProbe is one pass's lagging default-probe read for the claude pool,
// one row per listed work bead.
func claimedProbe(ids ...string) collectedDemand {
	d := scaleCheckDemand{Count: len(ids), StoreRefs: make(map[string]string, len(ids))}
	for _, id := range ids {
		d.WorkBeadIDs = append(d.WorkBeadIDs, id)
		d.StoreRefs[id] = "city"
	}
	return collectedDemand{
		DefaultProbed: true,
		DefaultCounts: map[string]int{"claude": len(ids)},
		DefaultDemand: map[string]scaleCheckDemand{"claude": d},
	}
}

// seatA is pool seat sess-a, which completed creation at createdAt and was
// minted for trigger: protected capacity until it claims.
func seatA(trigger string, createdAt time.Time) beads.Bead {
	seat := protectedPoolSessionBeadAt("sess-a", createdAt)
	seat.Metadata[beadmeta.TriggerBeadIDMetadataKey] = trigger
	return seat
}

func claimedWorkCity() *config.City {
	return &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(8), 0)}}
}

// claimedMerge is gather's cut of collected by work's claims, then the merge.
func claimedMerge(work []beads.Bead, collected collectedDemand) mergedDemand {
	collected.DefaultCounts, collected.DefaultDemand, _ = withoutClaimedDemand(collected.DefaultCounts, collected.DefaultDemand, demandClaimsOf(work, nil))
	return mergeCollectedDemand(claimedWorkCity(), collected)
}

// passPoolDesired is a pass's pool demand over its cut and merged demand.
func passPoolDesired(work, sessions []beads.Bead, collected collectedDemand) []PoolDesiredState {
	merged := claimedMerge(work, collected)
	return computePoolDesiredStatesAt(claimedWorkCity(), work, sessionInfosFromBeads(sessions), merged.ScaleCheckCounts, merged.ScaleCheckDemand, claimedWorkNow, nil)
}

// mintedSeatWork is the work each claude request that would mint a seat
// names, in order: a "new" request with no concrete session behind it.
func mintedSeatWork(states []PoolDesiredState) []string {
	var out []string
	for _, state := range states {
		if state.Template != "claude" {
			continue
		}
		for _, r := range state.Requests {
			if r.Tier == "new" && r.SessionBeadID == "" {
				out = append(out, r.WorkBeadID)
			}
		}
	}
	return out
}

// The dominant shape (94% of the no-work seats on 2026-11-02): seat A was
// minted for w-1 and claimed it 17 s after creation; 4.4 s later the
// controller minted seat B for the same w-1, because the recording still
// listed it.
func TestClaimedWork_SeatClaimingItsTriggerMintsNoSecondSeat(t *testing.T) {
	work := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	states := passPoolDesired(work, []beads.Bead{seatA("w-1", claimedWorkNow.Add(-21*time.Second))}, claimedProbe("w-1"))

	if got := mintedSeatWork(states); len(got) != 0 {
		t.Fatalf("new seats for %v, want none: sess-a claimed w-1, and a lagging read listing it is not new work", got)
	}
	if got := PoolDesiredCounts(states)["claude"]; got != 1 {
		t.Fatalf("PoolDesired[claude] = %d, want 1 (sess-a resumes its claim)", got)
	}
}

// The second shape (4-11%): seat A was minted for w-2, but pull handed it
// w-1, so w-1 was claimed before any seat named it. w-2 is still unclaimed
// and still needs its seat.
func TestClaimedWork_ClaimOfAnotherSeatsTriggerMintsOnlyForUnclaimedWork(t *testing.T) {
	work := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	states := passPoolDesired(work, []beads.Bead{seatA("w-2", claimedWorkNow.Add(-30*time.Second))}, claimedProbe("w-1", "w-2"))

	if got := mintedSeatWork(states); !slices.Equal(got, []string{"w-2"}) {
		t.Fatalf("new seats for %v, want exactly [w-2]", got)
	}
}

// A real backlog behind a claim still scales: only the claimed row goes.
func TestClaimedWork_BacklogBehindAClaimStillScales(t *testing.T) {
	work := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	states := passPoolDesired(work, []beads.Bead{seatA("w-1", claimedWorkNow.Add(-21*time.Second))}, claimedProbe("w-1", "w-2", "w-3"))

	got := mintedSeatWork(states)
	slices.Sort(got)
	if !slices.Equal(got, []string{"w-2", "w-3"}) {
		t.Fatalf("new seats for %v, want [w-2 w-3]", got)
	}
	if got := PoolDesiredCounts(states)["claude"]; got != 3 {
		t.Fatalf("PoolDesired[claude] = %d, want 3 (one resume, two new)", got)
	}
}

// A row assigned but still open (pinned to a live seat, or pre-assigned to a
// continuation group's seat) is not unassigned work, and neither is a claim
// whose claimant is gone: the dead-assignee sweep releases that one, and the
// next read counts it.
func TestClaimedWork_AssignedOrOrphanClaimIsNotNewDemand(t *testing.T) {
	for name, tc := range map[string]struct {
		work     []beads.Bead
		sessions []beads.Bead
	}{
		"open-assigned-to-live-seat": {
			work:     []beads.Bead{workBead("w-1", "claude", "sess-a", "open", 2)},
			sessions: []beads.Bead{seatA("w-0", claimedWorkNow.Add(-90*time.Second))},
		},
		"in-progress-claimant-closed": {
			work: []beads.Bead{workBead("w-1", "claude", "sess-gone", "in_progress", 2)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			states := passPoolDesired(tc.work, tc.sessions, claimedProbe("w-1"))
			if got := mintedSeatWork(states); len(got) != 0 {
				t.Fatalf("new seats for %v, want none: w-1 is not unassigned work", got)
			}
		})
	}
}

// The cut runs before the merge's clamps and leaves a custom scale_check
// count alone: that count names no rows, and claimsAfterRun is its guard.
func TestClaimedWork_CutPrecedesTheClampsAndSparesCustomCounts(t *testing.T) {
	work := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	clamped := func(ids ...string) collectedDemand {
		c := claimedProbe(ids...)
		c.ColdWakeTemplates = map[string]bool{"claude": true}
		return c
	}
	for name, tc := range map[string]struct {
		in   collectedDemand
		want int
	}{
		"custom-count":              {collectedDemand{CustomCounts: map[string]int{"claude": 2}}, 2},
		"clamp-keeps-unclaimed-row": {clamped("w-1", "w-2"), 1},
		"clamp-all-rows-claimed":    {clamped("w-1"), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := claimedMerge(work, tc.in).ScaleCheckCounts["claude"]; got != tc.want {
				t.Fatalf("merged count = %d, want %d", got, tc.want)
			}
		})
	}
}

// The ready routed work the idle-claim nudge reads follows the cut demand:
// no seat is nudged toward a bead another seat holds.
func TestClaimedWork_ReadyRoutedDropsClaimedRows(t *testing.T) {
	work := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	in := claimedProbe("w-1", "w-2")
	in.UnassignedRouted = []beads.Bead{workBead("w-1", "claude", "", "open", 2), workBead("w-2", "claude", "", "open", 2)}
	in.UnassignedRoutedRefs = []string{"city", "city"}

	if got := ids(claimedMerge(work, in).ReadyUnassignedRouted); !slices.Equal(got, []string{"w-2"}) {
		t.Fatalf("ready routed work = %v, want [w-2]", got)
	}
}

// The cut trusts the claim read. Where that read lags a release the probe
// already saw (a write that emitted no event reaches a cache at its next
// re-scan, every 30-120 s), the released row waits for the claim read to
// catch up; the same stale read keeps the stale claimer resumed, so nothing
// drains meanwhile.
func TestClaimedWork_ReleaseTheClaimReadHasNotSeenWaitsForIt(t *testing.T) {
	staleClaim := []beads.Bead{workBead("w-1", "claude", "sess-a", "in_progress", 2)}
	states := passPoolDesired(staleClaim, []beads.Bead{seatA("w-1", claimedWorkNow.Add(-90*time.Second))}, claimedProbe("w-1"))

	if got := mintedSeatWork(states); len(got) != 0 {
		t.Fatalf("new seats for %v, want none until the claim read shows the release", got)
	}
	if got := PoolDesiredCounts(states)["claude"]; got != 1 {
		t.Fatalf("PoolDesired[claude] = %d, want 1: the stale claimer stays resumed, not drained", got)
	}
}

// The cut never edits its arguments: the same collected view feeds the rest
// of the pass.
func TestClaimedWork_CutLeavesItsInputUntouched(t *testing.T) {
	in := claimedProbe("w-1", "w-2")
	wantCounts, wantIDs := maps.Clone(in.DefaultCounts), slices.Clone(in.DefaultDemand["claude"].WorkBeadIDs)

	counts, demand, cut := withoutClaimedDemand(in.DefaultCounts, in.DefaultDemand, demandClaims{{StoreRef: "city", ID: "w-1"}: false})

	if !maps.Equal(in.DefaultCounts, wantCounts) || !reflect.DeepEqual(in.DefaultDemand["claude"].WorkBeadIDs, wantIDs) {
		t.Fatalf("input edited: counts %v ids %v, want %v %v", in.DefaultCounts, in.DefaultDemand["claude"].WorkBeadIDs, wantCounts, wantIDs)
	}
	if counts["claude"] != 1 || !slices.Equal(demand["claude"].WorkBeadIDs, []string{"w-2"}) || demand["claude"].Count != 1 {
		t.Fatalf("cut demand = %d %v (Count %d), want 1 [w-2]", counts["claude"], demand["claude"].WorkBeadIDs, demand["claude"].Count)
	}
	if c := cut["claude"]; !slices.Equal(c.WorkBeadIDs, []string{"w-1"}) || c.Read != 2 || c.Unclaimed != 1 {
		t.Fatalf("cut[claude] = %+v, want w-1 cut from 2 to 1", c)
	}
}

// Only assigned rows count as claimed, mirroring the serving rules, which
// count only unassigned rows; status plays no part.
func TestClaimedWork_IDsAreAssignedRowsOnly(t *testing.T) {
	claims := demandClaimsOf([]beads.Bead{
		workBead("in-progress", "claude", "sess-a", "in_progress", 2),
		workBead("open-assigned", "claude", "sess-a", "open", 2),
		workBead("in-progress-unassigned", "claude", "", "in_progress", 2),
		workBead("open-unassigned", "claude", "", "open", 2),
	}, nil)
	var got []string
	for k := range claims {
		got = append(got, k.ID)
	}
	slices.Sort(got)
	if want := []string{"in-progress", "open-assigned"}; !slices.Equal(got, want) {
		t.Fatalf("claimed = %v, want %v", got, want)
	}
}

// With nothing claimed the cut hands its maps back as they are.
func TestClaimedWork_NothingClaimedReturnsTheSameMaps(t *testing.T) {
	in := claimedProbe("w-1", "w-2")
	counts, demand, cut := withoutClaimedDemand(in.DefaultCounts, in.DefaultDemand, demandClaims{{StoreRef: "city", ID: "w-9"}: false})
	if reflect.ValueOf(counts).Pointer() != reflect.ValueOf(in.DefaultCounts).Pointer() ||
		reflect.ValueOf(demand).Pointer() != reflect.ValueOf(in.DefaultDemand).Pointer() || cut != nil {
		t.Fatalf("cut with nothing claimed copied or cut: %v %v %v", counts, demand, cut)
	}
}

// cutsW1 reports whether the cut drops the probe's row w-1, read under
// probeRef (from a class binding when inBinding), when the assigned-work read
// shows w-1 claimed under each of claimRefs.
func cutsW1(probeRef string, inBinding bool, claimRefs ...string) bool {
	d := scaleCheckDemand{Count: 1, WorkBeadIDs: []string{"w-1"}, StoreRefs: map[string]string{"w-1": probeRef}}
	if inBinding {
		d.BindingResident = map[string]bool{"w-1": true}
	}
	work := make([]beads.Bead, 0, len(claimRefs))
	for range claimRefs {
		work = append(work, workBead("w-1", "claude", "sess-a", "in_progress", 2))
	}
	_, _, cut := withoutClaimedDemand(map[string]int{"claude": 1}, map[string]scaleCheckDemand{"claude": d}, demandClaimsOf(work, claimRefs))
	return slices.Equal(cut["claude"].WorkBeadIDs, []string{"w-1"})
}

// A claim holds a row only in the scope it resides in, however each read
// spells the store: the census names the city work ledger "" and a rig by its
// bare name, the probe says "city" and "rig:<name>", or a bare rig name on the
// cold-wake probe. A same-ID bead in another scope is still unassigned work.
func TestClaimedWork_ClaimHoldsOnlyItsOwnScope(t *testing.T) {
	for name, tc := range map[string]struct {
		probeRef, claimRef string
		cut                bool
	}{
		"city-row-ledger-claim":       {"city", "", true},
		"city-row-binding-claim":      {"city", "class:graph", true},
		"rig-row-rig-claim":           {"rig:alpha", "alpha", true},
		"cold-wake-rig-row-rig-claim": {"alpha", "alpha", true},
		"city-row-rig-claim":          {"city", "alpha", false},
		"rig-row-ledger-claim":        {"rig:alpha", "", false},
		"rig-row-binding-claim":       {"rig:alpha", "class:graph", false},
		"rig-row-other-rig-claim":     {"rig:alpha", "bravo", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cutsW1(tc.probeRef, false, tc.claimRef); got != tc.cut {
				t.Fatalf("probe row under %q, claim under %q: cut = %t, want %t", tc.probeRef, tc.claimRef, got, tc.cut)
			}
		})
	}
}

// A row the probe read from a class binding is held only by a claim a binding
// holds. The work ledger's frozen copy of a migrated bead keeps its ID and
// the assignee it had then, which says nothing about the binding's row.
func TestClaimedWork_BindingRowIsHeldOnlyByABindingClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		claimRefs []string
		cut       bool
	}{
		"binding-claim":                  {[]string{"class:graph"}, true},
		"frozen-ledger-copy":             {[]string{""}, false},
		"frozen-ledger-copy-and-binding": {[]string{"", "class:graph"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cutsW1("city", true, tc.claimRefs...); got != tc.cut {
				t.Fatalf("binding row, claims under %q: cut = %t, want %t", tc.claimRefs, got, tc.cut)
			}
		})
	}
}
