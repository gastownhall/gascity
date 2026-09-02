package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestRecordPoolSpawnChurn_TripsAfterThresholdAndCoolsDown(t *testing.T) {
	resetPoolSpawnChurnBreakerForTest()
	now := time.Date(2026, 8, 5, 3, 14, 23, 0, time.UTC)

	for i := 1; i < poolSpawnChurnThreshold; i++ {
		tripped, consecutive, _ := recordPoolSpawnChurn("gascity/novices", now)
		if tripped {
			t.Fatalf("cycle %d: tripped early (threshold=%d)", i, poolSpawnChurnThreshold)
		}
		if consecutive != i {
			t.Fatalf("cycle %d: consecutive = %d, want %d", i, consecutive, i)
		}
		now = now.Add(24 * time.Second)
	}

	tripped, consecutive, cooldownUntil := recordPoolSpawnChurn("gascity/novices", now)
	if !tripped {
		t.Fatalf("consecutive=%d, want the threshold-th call to trip", consecutive)
	}
	if consecutive != poolSpawnChurnThreshold {
		t.Fatalf("consecutive = %d, want %d", consecutive, poolSpawnChurnThreshold)
	}
	if !cooldownUntil.After(now) {
		t.Fatalf("cooldownUntil = %v, want after %v", cooldownUntil, now)
	}

	// Re-tripping while already tripped must not re-report tripped=true —
	// the caller (recordPoolSpawnChurnForClosedSession) uses this to emit
	// the cooling-down event exactly once per trip, not once per churned
	// spawn while already cooling down.
	tripped2, consecutive2, _ := recordPoolSpawnChurn("gascity/novices", now.Add(time.Second))
	if tripped2 {
		t.Fatalf("second call after trip reported tripped=true, want false (already cooling down)")
	}
	if consecutive2 != poolSpawnChurnThreshold+1 {
		t.Fatalf("consecutive2 = %d, want %d", consecutive2, poolSpawnChurnThreshold+1)
	}

	cooldowns := poolSpawnChurnCooldownTemplates(now.Add(time.Second))
	if !cooldowns["gascity/novices"] {
		t.Fatalf("cooldowns = %v, want gascity/novices cooling down", cooldowns)
	}

	afterCooldown := cooldownUntil.Add(time.Second)
	cooldowns = poolSpawnChurnCooldownTemplates(afterCooldown)
	if cooldowns["gascity/novices"] {
		t.Fatalf("cooldowns = %v, want gascity/novices clear once the cooldown elapses", cooldowns)
	}
}

func TestRecordPoolSpawnChurn_StaleGapRestartsStreak(t *testing.T) {
	resetPoolSpawnChurnBreakerForTest()
	now := time.Date(2026, 8, 5, 3, 14, 23, 0, time.UTC)

	for i := 0; i < poolSpawnChurnThreshold*3; i++ {
		tripped, consecutive, _ := recordPoolSpawnChurn("gascity/novices", now)
		if tripped {
			t.Fatalf("iteration %d: tripped despite a stale gap every time (consecutive=%d)", i, consecutive)
		}
		if consecutive != 1 {
			t.Fatalf("iteration %d: consecutive = %d, want 1 (streak must restart after the gap)", i, consecutive)
		}
		now = now.Add(poolSpawnChurnWindow + time.Minute)
	}
}

func TestRecordPoolSpawnChurnForClosedSession_OnlyCountsNeverBoundPoolSessions(t *testing.T) {
	tests := []struct {
		name        string
		info        sessionpkg.Info
		wantCounted bool
	}{
		{
			name:        "pool-managed, never bound a trigger — churn",
			info:        sessionpkg.Info{ID: "nicola-1", PoolManaged: true, TriggerBeadID: ""},
			wantCounted: true,
		},
		{
			name:        "pool-managed, bound and did real work — not churn",
			info:        sessionpkg.Info{ID: "nicola-2", PoolManaged: true, TriggerBeadID: "ra-t49y6i"},
			wantCounted: false,
		},
		{
			name:        "not pool-managed (named/manual session) — never counted",
			info:        sessionpkg.Info{ID: "primary", PoolManaged: false, TriggerBeadID: ""},
			wantCounted: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetPoolSpawnChurnBreakerForTest()
			clk := &clock.Fake{Time: time.Date(2026, 8, 5, 3, 14, 23, 0, time.UTC)}
			recordPoolSpawnChurnForClosedSession("gascity/novices", tc.info, clk, events.Discard)

			// A single occurrence never trips the threshold-3 breaker on its
			// own, so drive it to poolSpawnChurnThreshold total calls and
			// check whether the breaker tripped — the only externally
			// observable signal that a call was counted at all.
			if tc.wantCounted {
				// Two more identical closes should trip the breaker if (and
				// only if) the first one was counted.
				recordPoolSpawnChurnForClosedSession("gascity/novices", tc.info, clk, events.Discard)
				recordPoolSpawnChurnForClosedSession("gascity/novices", tc.info, clk, events.Discard)
				cooldowns := poolSpawnChurnCooldownTemplates(clk.Now())
				if !cooldowns["gascity/novices"] {
					t.Fatalf("cooldowns = %v, want gascity/novices tripped after %d churned closes", cooldowns, poolSpawnChurnThreshold)
				}
			} else {
				recordPoolSpawnChurnForClosedSession("gascity/novices", tc.info, clk, events.Discard)
				recordPoolSpawnChurnForClosedSession("gascity/novices", tc.info, clk, events.Discard)
				cooldowns := poolSpawnChurnCooldownTemplates(clk.Now())
				if cooldowns["gascity/novices"] {
					t.Fatalf("cooldowns = %v, want gascity/novices never tripped (not churn)", cooldowns)
				}
			}
		})
	}
}

func TestRecordPoolSpawnChurnForClosedSession_EmitsEventOnlyOnceAtTrip(t *testing.T) {
	resetPoolSpawnChurnBreakerForTest()
	fake := events.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 8, 5, 3, 14, 23, 0, time.UTC)}
	blind := sessionpkg.Info{ID: "nicola-blind", PoolManaged: true, TriggerBeadID: ""}

	for i := 0; i < poolSpawnChurnThreshold+2; i++ {
		recordPoolSpawnChurnForClosedSession("gascity/novices", blind, clk, fake)
		clk.Advance(24 * time.Second)
	}

	matches := 0
	var matched *events.Event
	for i := range fake.Events {
		if fake.Events[i].Type == events.PoolSpawnChurnCoolingDown {
			matches++
			matched = &fake.Events[i]
		}
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1 (only the trip, not every churned close after)", events.PoolSpawnChurnCoolingDown, matches)
	}
	if matched.Subject != "gascity/novices" {
		t.Errorf("event subject = %q, want gascity/novices", matched.Subject)
	}
}

// TestPoolSpawnChurn_ReproducesThenStops is the ra-co9epr acceptance
// regression in one place: it reproduces the overnight loop shape (a pool
// template that keeps spawning blind sessions which claim no work, at the
// measured ~24s cadence) end to end through the two production hooks —
// recordPoolSpawnChurnForClosedSession (the close-side detector) and
// ComputePoolDesiredStatesWithDemandChurnTraced (the spawn-side gate) — and
// shows the fix actually stops it, not just that each half works in
// isolation.
func TestPoolSpawnChurn_ReproducesThenStops(t *testing.T) {
	resetPoolSpawnChurnBreakerForTest()
	const template = "novices"
	cfg := &config.City{Agents: []config.Agent{poolAgent("novices", "", intPtr(5), 0)}}
	scaleCheck := map[string]int{template: 1}
	// ra-76ojgf's exact shape: routed+ready+unassigned per the spawn-side
	// count, but no candidate WorkBeadID the demand-tracking probe could
	// resolve (mint-time vs. update-time gc.routed_to divergence) — so every
	// "new" request for this template is blind.
	demand := map[string]scaleCheckDemand{}
	clk := &clock.Fake{Time: time.Date(2026, 8, 5, 3, 14, 23, 0, time.UTC)}
	fake := events.NewFake()

	// REPRODUCE: before any churn has been observed, the spawn side has no
	// reason to withhold — it spawns blind, exactly as it did 46 times
	// overnight.
	before := ComputePoolDesiredStatesWithDemandChurnTraced(cfg, nil, nil, scaleCheck, demand, poolSpawnChurnCooldownTemplates(clk.Now()), nil)
	if len(before) != 1 || len(before[0].Requests) != 1 || before[0].Requests[0].WorkBeadID != "" {
		t.Fatalf("before fix engages: requests = %+v, want exactly one blind (WorkBeadID=\"\") request reproducing the pre-fix spawn", before)
	}

	// Drive poolSpawnChurnThreshold blind-spawned sessions to closing having
	// claimed no work, at the measured ~24s cadence.
	blind := sessionpkg.Info{ID: "nicola-churn", PoolManaged: true, TriggerBeadID: ""}
	for i := 0; i < poolSpawnChurnThreshold; i++ {
		recordPoolSpawnChurnForClosedSession(template, blind, clk, fake)
		clk.Advance(24 * time.Second)
	}

	// STOPS: the next spawn decision, fed the breaker's live cooldown set,
	// withholds the blind request instead of materializing session #4.
	after := ComputePoolDesiredStatesWithDemandChurnTraced(cfg, nil, nil, scaleCheck, demand, poolSpawnChurnCooldownTemplates(clk.Now()), nil)
	for _, state := range after {
		if len(state.Requests) != 0 {
			t.Fatalf("after fix engages: %s requests = %+v, want none (blind spawn withheld once churn was observed)", state.Template, state.Requests)
		}
	}

	sawCoolingDown := false
	for _, ev := range fake.Events {
		if ev.Type == events.PoolSpawnChurnCoolingDown && ev.Subject == template {
			sawCoolingDown = true
		}
	}
	if !sawCoolingDown {
		t.Fatalf("events = %v, want a %s event for %s", fake.Events, events.PoolSpawnChurnCoolingDown, template)
	}
}
