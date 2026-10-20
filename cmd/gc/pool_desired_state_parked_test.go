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
