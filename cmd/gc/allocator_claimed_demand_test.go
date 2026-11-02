package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// claimedSeatRow is pool seat gc-1, minted for w-1, awake and fresh: the
// protected stand-in for its trigger until it claims.
func claimedSeatRow() beads.Bead {
	return poolRow("gc-1", "worker", 1, "awake", "state_reason", "creation_complete",
		"creation_complete_at", ago(14*time.Second), "gc.trigger_bead_id", "w-1")
}

func claimedDemandCity() *config.City {
	return &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{poolAgent("worker", "", intPtr(8), 0)}}
}

// claimedGather is one v2 pass's demand over a lane-fed leg whose cache holds
// the work as it is now, while the external-reads lane's recording holds the
// open rows as it last read them, a few seconds earlier.
func claimedGather(t *testing.T, reads func(*beads.CachingStore) demandReads, now []beads.Bead) demandView {
	t.Helper()
	cache, _ := newDemandCache(t, false, now...)
	v, err := gatherDemand(demandGatherEnv{
		Cfg: claimedDemandCity(), CityStore: cache,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, reads(cache))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	return v
}

// recordedOpen is a v2 reads whose recording of cache's leg lists open.
func recordedOpen(open ...beads.Bead) func(*beads.CachingStore) demandReads {
	return func(cache *beads.CachingStore) demandReads {
		rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
			keyOf(sourceDemand, cache): {EndedAt: allocNow.Add(-5 * time.Second), sourcePayload: sourcePayload{Leg: legRecording{RawOpen: open, ReadyAll: open}}},
		}}
		return newV2DemandReads(allocNow, rec)
	}
}

func claimedBy(id, assignee string) beads.Bead {
	b := routedDemandBead(id)
	b.Status, b.Assignee = "in_progress", assignee
	return b
}

// Kills: a seat's own claim minting a second seat for the same work, through
// the readers that produced it in production (gc-ent4, 2026-11-02): gc-1 was
// minted for w-1 and claimed it; the leg's cache shows the claim, while the
// lane's recording, a few seconds older, still lists w-1 open and unassigned.
// The claim puts gc-1 on the resume tier, where it no longer stands in for
// w-1 as protected capacity, so counting the recorded row minted a duplicate
// that drained with no work. Gather now cuts the row; the decide plans no
// seat for it, traces why, and still plans one for new work routed meanwhile.
func TestAllocator_ClaimAheadOfTheDemandRecordingMintsNoSecondSeat(t *testing.T) {
	v := claimedGather(t, recordedOpen(routedDemandBead("w-1")), []beads.Bead{claimedBy("w-1", "gc-1")})
	if cut := v.ClaimedDemand["worker"]; cut.Read != 1 || !slices.Equal(cut.WorkBeadIDs, []string{"w-1"}) {
		t.Fatalf("claimed demand = %+v, want the recorded w-1 cut", v.ClaimedDemand)
	}
	if got := v.Collected.DefaultCounts["worker"]; got != 0 {
		t.Fatalf("worker default demand = %d, want 0 once the claimed row is cut", got)
	}

	f := newAllocFixture(t, claimedDemandCity()).sessions(claimedSeatRow()).alive("s-gc-1", InventoryAttrs{})
	f.in.Demand = v
	d := f.decide()
	if len(d.Plans) != 0 {
		t.Fatalf("plans %v, want none: gc-1 holds w-1 (trace %v)", planWork(d), d.Trace)
	}
	if got := d.Snapshot.PoolDesired["worker"]; got != 1 {
		t.Fatalf("PoolDesired = %d, want 1 (gc-1 resumes its claim)", got)
	}
	var traced []string
	for _, r := range d.Trace {
		if strings.HasPrefix(r.Reason, allocTraceClaimedDemand) {
			traced = append(traced, r.Template+"/"+r.Reason)
		}
	}
	if !slices.Equal(traced, []string{"worker/claimed-demand:w-1"}) {
		t.Fatalf("claimed-demand trace = %v, want [worker/claimed-demand:w-1]", traced)
	}

	// New work routed meanwhile still gets its seat.
	f.in.Demand = claimedGather(t, recordedOpen(routedDemandBead("w-1"), routedDemandBead("w-2")),
		[]beads.Bead{claimedBy("w-1", "gc-1"), routedDemandBead("w-2")})
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-2]" {
		t.Fatalf("new work behind the claim: plans %v, want [w-2]", got)
	}
}

// Legacy reads take no cut: its probe is a live read taken after its claim
// read, so where the two disagree the live probe is the newer one. Here the
// leg's cache still shows gc-1's claim on w-1 after the backing released it;
// the live Ready lists w-1 again, and that row stays demand.
func TestGatherDemandCutsClaimedWorkOnlyUnderV2Reads(t *testing.T) {
	cache, backing := newDemandCache(t, false, claimedBy("w-1", "gc-1"))
	open, unassigned := "open", ""
	if err := backing.Update("w-1", beads.UpdateOpts{Status: &open, Assignee: &unassigned}); err != nil {
		t.Fatalf("release w-1 behind the cache: %v", err)
	}
	v, err := gatherDemand(demandGatherEnv{
		Cfg: claimedDemandCity(), CityStore: cache,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, nil)
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	if got := ids(v.AssignedWork); !slices.Equal(got, []string{"w-1"}) {
		t.Fatalf("assigned work = %v, want the cache's stale claim on w-1", got)
	}
	if v.ClaimedDemand != nil || v.Collected.DefaultCounts["worker"] != 1 {
		t.Fatalf("legacy reads: claimed demand %v, worker demand %d; want no cut and the live read's w-1", v.ClaimedDemand, v.Collected.DefaultCounts["worker"])
	}
}

// The planner's pass record counts each cut row once, on the first pass that
// cuts it: a row the recording keeps listing is one lagging claim, not one
// per pass, and a row cut again after a pass without it is a new one.
func TestPlannerCountsClaimedDemandCutOncePerClaim(t *testing.T) {
	p := &planner{metrics: newPassMetrics()}
	cut := func(rows map[string][]string) demandView {
		v := demandView{ClaimedDemand: make(map[string]claimedDemandCut)}
		for template, ids := range rows {
			v.ClaimedDemand[template] = claimedDemandCut{WorkBeadIDs: ids}
		}
		return v
	}
	for i, pass := range []struct {
		rows map[string][]string
		want uint64
	}{
		{map[string][]string{"worker": {"w-1", "w-2"}, "builder": {"w-3"}}, 3},
		{map[string][]string{"worker": {"w-1", "w-2"}, "builder": {"w-3"}}, 3},
		{map[string][]string{"worker": {"w-2", "w-4"}}, 4},
		{nil, 4},
		{map[string][]string{"worker": {"w-2"}}, 5},
	} {
		p.observeClaimedDemand(cut(pass.rows))
		if got := p.metrics.series[claimedDemandSeries]; got != pass.want {
			t.Fatalf("pass %d: %s = %d, want %d", i+1, claimedDemandSeries, got, pass.want)
		}
	}
}
