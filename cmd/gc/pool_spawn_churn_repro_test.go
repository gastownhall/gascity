package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ga-s5l1ms (PR #6941): scale_check demand the planner cannot bind to an
// identified work bead is materialized as blind "new" requests, one per unit
// of demand, with no bound on how often they churn. Once a pool has closed a
// run of blind seats in quick succession — each drained with no work, never
// bound to a trigger bead — the planner should withhold further blind
// requests for that template (bound requests unaffected) until a cooldown.
func TestPoolBlindSpawnsWithheldAfterChurnedClosesRepro(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)}}

	// Ten pool seats of "claude", each created and closed within the last
	// few minutes, drained with no work and never bound a trigger bead.
	var sessions []sessionpkg.Info
	for i := 0; i < 10; i++ {
		created := now.Add(-time.Duration(10-i) * 30 * time.Second)
		sessions = append(sessions, sessionpkg.Info{
			ID:          fmt.Sprintf("churn-%d", i),
			Type:        sessionBeadType,
			Template:    "claude",
			State:       sessionpkg.StateDrained,
			Closed:      true,
			PoolManaged: true,
			PoolSlot:    fmt.Sprint(i + 1),
			SleepReason: "drained",
			CreatedAt:   created,
			LastActive:  created.Add(20 * time.Second),
		})
	}

	// scale_check still reports 3 units of demand, none identified.
	scale := map[string]int{"claude": 3}
	states := computePoolDesiredStatesParkedAt(cfg, nil, sessions, scale, nil, now, nil, nil)

	var blind []SessionRequest
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Tier == "new" && r.WorkBeadID == "" {
				blind = append(blind, r)
			}
		}
	}
	if len(blind) != 0 {
		t.Fatalf("planner emitted %d blind new requests for \"claude\" after 10 consecutive churned (drained, no work, never bound) closes; want 0 while the template cools down: %+v", len(blind), blind)
	}
}
