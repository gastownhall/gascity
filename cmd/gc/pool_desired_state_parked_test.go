package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The planner's half of a parked seat (ga-ylpbsy.16, owner decision D1): a
// parked seat stays selected, so it sleeps instead of draining as an
// orphan, but its blocked pin takes no cap headroom and is not pool demand
// until it is ready.

func parkedRequests(states []PoolDesiredState) (parked, counted []SessionRequest) {
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Parked {
				parked = append(parked, r)
			} else {
				counted = append(counted, r)
			}
		}
	}
	return parked, counted
}

func TestComputePoolDesiredStatesParkedSeatTakesNoCap(t *testing.T) {
	wsMax := 1
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Agents:    []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	work := []beads.Bead{workBead("pin-1", "claude", "sess-1", "open", 2)}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open")})
	scale := map[string]int{"claude": 1}

	t.Run("unparked, the blocked pin's resume fills the city cap", func(t *testing.T) {
		states := computePoolDesiredStatesParkedAt(cfg, work, sessions, scale, nil, parkNow, nil, nil)
		parked, counted := parkedRequests(states)
		if len(parked) != 0 || len(counted) != 1 || counted[0].SessionBeadID != "sess-1" {
			t.Fatalf("parked %+v counted %+v, want only sess-1's resume: the cap admits nothing new", parked, counted)
		}
	})
	t.Run("parked, the seat is selected and new demand takes the cap", func(t *testing.T) {
		states := computePoolDesiredStatesParkedAt(cfg, work, sessions, scale, nil, parkNow, parkedSeats{"sess-1": {Eligible: true, Parked: true}}, nil)
		parked, counted := parkedRequests(states)
		if len(parked) != 1 || parked[0].SessionBeadID != "sess-1" || parked[0].Tier != "resume" || parked[0].WorkBeadID != "pin-1" {
			t.Fatalf("parked %+v, want sess-1 kept selected on its pin", parked)
		}
		if len(counted) != 1 || counted[0].Tier != "new" {
			t.Fatalf("counted %+v, want one new request under the cap of 1", counted)
		}
		if got := PoolDesiredCounts(states)["claude"]; got != 1 {
			t.Fatalf("pool desired = %d, want 1: a parked seat is not pool demand", got)
		}
	})
	t.Run("a pin turned ready is a counted resume again", func(t *testing.T) {
		// The verdict is the pass's: a ready pin is not parked.
		states := computePoolDesiredStatesParkedAt(cfg, work, sessions, scale, nil, parkNow, parkedSeats{"sess-1": {Eligible: true}}, nil)
		parked, counted := parkedRequests(states)
		if len(parked) != 0 || len(counted) != 1 || counted[0].SessionBeadID != "sess-1" {
			t.Fatalf("parked %+v counted %+v, want sess-1's counted resume", parked, counted)
		}
	})
}

// A parked seat's resume is a parked continuation, while its pin is blocked
// and once the pin is ready, so its binding keeps the work dir its
// conversation ran in. A seat that also holds work in progress is not, and
// neither is a seat the pass did not judge.
func TestComputePoolDesiredStatesMarksAParkedContinuation(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", nil, 0)}}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open")})
	pinOnly := []beads.Bead{workBead("pin-1", "claude", "sess-1", "open", 2)}
	for _, tc := range []struct {
		name                 string
		work                 []beads.Bead
		verdict              parkedSeats
		wantParked, wantCont bool
	}{
		{name: "parked on a blocked pin", work: pinOnly, verdict: parkedSeats{"sess-1": {Eligible: true, Parked: true, Continues: true}}, wantParked: true, wantCont: true},
		{name: "waking for its ready pin", work: pinOnly, verdict: parkedSeats{"sess-1": {Eligible: true, Continues: true}}, wantCont: true},
		{name: "unparked by its pool's slots, waking for its pin", work: pinOnly, verdict: parkedSeats{"sess-1": {Continues: true}}, wantCont: true},
		{name: "holding work in progress too", work: append([]beads.Bead{workBead("claim-1", "claude", "sess-1", "in_progress", 2)}, pinOnly...), verdict: parkedSeats{"sess-1": {Eligible: true, Continues: true}}},
		{name: "no verdict", work: pinOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := computePoolDesiredStatesParkedAt(cfg, tc.work, sessions, nil, nil, parkNow, tc.verdict, nil)
			var seat []SessionRequest
			for _, s := range states {
				for _, r := range s.Requests {
					if r.SessionBeadID == "sess-1" {
						seat = append(seat, r)
					}
				}
			}
			if len(seat) != 1 || seat[0].Tier != "resume" {
				t.Fatalf("states %+v, want one resume of sess-1", states)
			}
			if seat[0].Parked != tc.wantParked || seat[0].ParkedContinuation != tc.wantCont {
				t.Fatalf("request %+v, want parked=%v parked continuation=%v", seat[0], tc.wantParked, tc.wantCont)
			}
		})
	}
}

// A parked seat's pin turned ready competes for a full city cap with the
// other existing seats' requests by bead priority, a tie keeping the pass's
// order. The seat that loses has no request at all; whether the session
// overlay still keeps it turns on its template
// (TestAllocator_ReadyPinCompetesForAFullCityCap).
func TestComputePoolDesiredStatesReadyPinCompetesForAFullCap(t *testing.T) {
	wsMax := 1
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Agents:    []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open"), sessionBead("sess-2", "open")})
	// sess-1 parked on pin-1, which is ready now: the pass's verdict is
	// eligible, not parked. sess-2 is a live seat on its claimed w-2 (P2),
	// which holds the city's one slot.
	unparked := parkedSeats{"sess-1": {Eligible: true}}
	for _, tc := range []struct {
		name        string
		pinPriority int
		pinFirst    bool
		want        string
	}{
		{name: "a pin that outranks the live seat's work takes the slot", pinPriority: 1, want: "sess-1"},
		{name: "a pin the live seat's work outranks loses, whatever the order", pinPriority: 3, pinFirst: true, want: "sess-2"},
		{name: "a tie goes to the earlier request, the live seat's", pinPriority: 2, want: "sess-2"},
		{name: "a tie goes to the earlier request, the pin's", pinPriority: 2, pinFirst: true, want: "sess-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := []beads.Bead{workBead("w-2", "claude", "sess-2", "in_progress", 2), workBead("pin-1", "claude", "sess-1", "open", tc.pinPriority)}
			if tc.pinFirst {
				work[0], work[1] = work[1], work[0]
			}
			states := computePoolDesiredStatesParkedAt(cfg, work, sessions, nil, nil, parkNow, unparked, nil)
			parked, counted := parkedRequests(states)
			if len(parked) != 0 || len(counted) != 1 || counted[0].SessionBeadID != tc.want || counted[0].Tier != "resume" {
				t.Fatalf("parked %+v counted %+v, want only %s's resume in the city's one slot", parked, counted, tc.want)
			}
			if got := PoolDesiredCounts(states)["claude"]; got != 1 {
				t.Fatalf("pool desired = %d, want 1: the cap holds", got)
			}
		})
	}
}

func TestComputePoolDesiredStatesParkedSeatsAreNotPoolDemand(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", nil, 0)}}
	work := []beads.Bead{
		workBead("pin-1", "claude", "sess-1", "open", 2),
		workBead("pin-2", "claude", "sess-1", "open", 2), // a seat may hold two pins
		workBead("pin-3", "claude", "sess-2", "open", 2),
	}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open"), sessionBead("sess-2", "open")})
	states := computePoolDesiredStatesParkedAt(cfg, work, sessions, nil, nil, parkNow,
		parkedSeats{"sess-1": {Eligible: true, Parked: true}, "sess-2": {Eligible: true, Parked: true}}, nil)
	parked, counted := parkedRequests(states)
	if len(counted) != 0 {
		t.Fatalf("counted %+v, want none", counted)
	}
	ids := map[string]bool{}
	for _, r := range parked {
		ids[r.SessionBeadID] = true
	}
	if len(parked) != 2 || !ids["sess-1"] || !ids["sess-2"] {
		t.Fatalf("parked %+v, want one request per parked seat", parked)
	}
	if got, ok := PoolDesiredCounts(states)["claude"]; ok {
		t.Fatalf("pool desired claude = %d, want no entry for a pool holding only parked seats", got)
	}
}

// A parked seat is asleep: it is not warm capacity, so it does not stand in
// for a min_active_sessions floor member.
func TestComputePoolDesiredStatesParkedSeatDoesNotFillAFloor(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", nil, 1)}}
	work := []beads.Bead{workBead("pin-1", "claude", "sess-1", "open", 2)}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open")})
	states := computePoolDesiredStatesParkedAt(cfg, work, sessions, nil, nil, parkNow, parkedSeats{"sess-1": {Eligible: true, Parked: true}}, nil)
	parked, counted := parkedRequests(states)
	if len(parked) != 1 || len(counted) != 1 || !counted[0].FloorGuarantee {
		t.Fatalf("parked %+v counted %+v, want the parked seat plus a floor request", parked, counted)
	}
}

// A seat holding any work that is not open is never parked, whatever the
// verdict map says: the classifier never parks one, and the planner parks a
// seat only when every request it makes for it is an open pin.
func TestComputePoolDesiredStatesInProgressStaysCounted(t *testing.T) {
	wsMax := 1
	cfg := &config.City{
		Workspace: config.Workspace{MaxActiveSessions: &wsMax},
		Agents:    []config.Agent{poolAgent("claude", "", nil, 0)},
	}
	work := []beads.Bead{
		workBead("pin-1", "claude", "sess-1", "open", 2),
		workBead("w-1", "claude", "sess-1", "in_progress", 2),
	}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open")})
	states := computePoolDesiredStatesParkedAt(cfg, work, sessions, map[string]int{"claude": 1}, nil, parkNow, parkedSeats{"sess-1": {Eligible: true, Parked: true}}, nil)
	parked, counted := parkedRequests(states)
	if len(parked) != 0 || len(counted) != 1 || counted[0].SessionBeadID != "sess-1" {
		t.Fatalf("parked %+v counted %+v, want sess-1's counted resume and nothing new", parked, counted)
	}
}
