package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// AC-1 (ga-s5l1ms): with no spawn-churn allowance recorded for a template,
// a custom scale_check count larger than its named work beads merges and
// plans exactly as before the guard existed.
func TestMergeCollectedDemandWithoutBlindSpawnAllowanceKeepsCustomCount(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "claude"}}}
	in := collectedDemand{
		CustomCounts:  map[string]int{"claude": 3},
		DefaultProbed: true,
		DefaultCounts: map[string]int{"claude": 1},
		DefaultDemand: map[string]scaleCheckDemand{"claude": {Count: 1, WorkBeadIDs: []string{"bd-1"}}},
	}
	in.BlindSpawnAllowance = nil
	merged := mergeCollectedDemand(cfg, in)
	if got := merged.ScaleCheckCounts["claude"]; got != 3 {
		t.Fatalf("ScaleCheckCounts[claude] = %d, want 3: no allowance must not clamp a custom count", got)
	}
	if got := merged.ScaleCheckDemand["claude"].Count; got != 1 {
		t.Fatalf("ScaleCheckDemand[claude].Count = %d, want 1: the demand's count follows its named beads, unchanged", got)
	}
	if got := len(merged.ScaleCheckDemand["claude"].WorkBeadIDs); got != 1 {
		t.Fatalf("ScaleCheckDemand[claude] names %d work beads, want the 1 the default probe named", got)
	}
}

// AC-1 (ga-s5l1ms): with no churned closes to answer for, the planner emits
// the blind tail of a custom count over its named beads unchanged.
func TestPoolPlannerWithoutChurnEmitsBlindTail(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)}}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, nil,
		map[string]int{"claude": 3},
		map[string]scaleCheckDemand{"claude": {Count: 3, WorkBeadIDs: []string{"bd-1"}}},
		now, nil, nil)
	var bound, blind int
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Tier != "new" {
				continue
			}
			if r.WorkBeadID == "" {
				blind++
			} else {
				bound++
			}
		}
	}
	if bound != 1 || blind != 2 {
		t.Fatalf("new requests: %d bound, %d blind; want 1 bound (bd-1) and 2 blind (count 3 over 1 named bead)", bound, blind)
	}
}

// AC-2 (ga-s5l1ms): a template the allowance holds at 0 unnamed seats has its
// custom scale_check count clamped to its named work beads in the merge — and
// its demand's count with it — without the merge editing the caller's rows.
func TestMergeCollectedDemandClampsBlindCountToNamedBeads(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "claude"}}}
	in := collectedDemand{
		CustomCounts:  map[string]int{"claude": 3},
		DefaultProbed: true,
		DefaultCounts: map[string]int{"claude": 1},
		DefaultDemand: map[string]scaleCheckDemand{"claude": {Count: 1, WorkBeadIDs: []string{"bd-1"}}},
	}
	in.BlindSpawnAllowance = map[string]int{"claude": 0}
	merged := mergeCollectedDemand(cfg, in)
	if got := merged.ScaleCheckCounts["claude"]; got != 1 {
		t.Fatalf("ScaleCheckCounts[claude] = %d, want 1: a 0 allowance clamps the count to its named work beads", got)
	}
	if got := merged.ScaleCheckDemand["claude"].Count; got != 1 {
		t.Fatalf("ScaleCheckDemand[claude].Count = %d, want 1: the demand's count is clamped the same way", got)
	}
	if got := len(merged.ScaleCheckDemand["claude"].WorkBeadIDs); got != 1 {
		t.Fatalf("ScaleCheckDemand[claude] names %d work beads, want the 1 named bead kept", got)
	}
	if got := in.CustomCounts["claude"]; got != 3 {
		t.Fatalf("in.CustomCounts[claude] = %d after the merge, want 3: the merge never edits its inputs", got)
	}
	// The planner, fed the merged counts, binds the named bead and emits no
	// blind request.
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, nil,
		merged.ScaleCheckCounts, merged.ScaleCheckDemand,
		time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), nil, nil)
	var blind int
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Tier == "new" && r.WorkBeadID == "" {
				blind++
			}
		}
	}
	if blind != 0 {
		t.Fatalf("planner emitted %d blind new requests from merged counts, want 0", blind)
	}
}

// churnedClosedRow is one closed churned blind seat of template, like the
// checkpoint repro's rows: pool-managed, drained, never bound, no claim.
func churnedClosedRow(id, template string, created time.Time) sessionpkg.Info {
	return sessionpkg.Info{
		ID:          id,
		Type:        sessionBeadType,
		Template:    template,
		State:       sessionpkg.StateDrained,
		Closed:      true,
		PoolManaged: true,
		SleepReason: "drained",
		CreatedAt:   created,
		LastActive:  created.Add(20 * time.Second),
	}
}

// churnedClosedRows is the three churned closes that trip the guard's hold,
// ending just before now and spaced far enough apart for its minimum span.
func churnedClosedRows(template string, now time.Time) []sessionpkg.Info {
	var rows []sessionpkg.Info
	for i := 0; i < 3; i++ {
		rows = append(rows, churnedClosedRow(fmt.Sprintf("churn-%s-%d", template, i), template, now.Add(-time.Duration(3-i)*3*time.Minute)))
	}
	return rows
}

// countNewRequests splits a template's new-tier requests into bound, blind
// (no work bead, no floor guarantee) and floor ones.
func countNewRequests(states []PoolDesiredState, template string) (bound, blind, floor int) {
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Template != template || r.Tier != "new" {
				continue
			}
			switch {
			case r.FloorGuarantee:
				floor++
			case r.WorkBeadID != "":
				bound++
			default:
				blind++
			}
		}
	}
	return bound, blind, floor
}

// AC-3 (ga-s5l1ms): a held template's named work beads keep their bound
// requests; only the blind remainder over them is withheld.
func TestPoolPlannerChurnHoldKeepsNamedBoundRequests(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)}}
	sessions := churnedClosedRows("claude", now)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{"claude": 3},
		map[string]scaleCheckDemand{"claude": {Count: 2, WorkBeadIDs: []string{"bd-1", "bd-2"}}},
		now, nil, nil)
	bound, blind, _ := countNewRequests(states, "claude")
	if bound != 2 || blind != 0 {
		t.Fatalf("new requests under a hold: %d bound, %d blind; want 2 bound (bd-1, bd-2) and 0 blind", bound, blind)
	}
}

// AC-3: a min_active_sessions floor is emitted unchanged under a hold: the
// floor is warm-capacity policy, not blind pull.
func TestPoolPlannerChurnHoldKeepsMinActiveFloor(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", nil, 2)}}
	sessions := churnedClosedRows("claude", now)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{"claude": 5}, nil, now, nil, nil)
	bound, blind, floor := countNewRequests(states, "claude")
	if floor != 2 || blind != 0 || bound != 0 {
		t.Fatalf("new requests under a hold with min_active_sessions 2: %d bound, %d blind, %d floor; want 0 bound, 0 blind, 2 floor", bound, blind, floor)
	}
}

// AC-3: a protected seat is reused under a hold: its request is capacity the
// pool already spent, not new blind demand.
func TestPoolPlannerChurnHoldKeepsProtectedSeatReuse(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)}}
	sessions := churnedClosedRows("claude", now)
	sessions = append(sessions, sessionInfosFromBeads([]beads.Bead{protectedPoolSessionBeadAt("sess-1", now.Add(-time.Minute))})...)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{"claude": 3}, nil, now, nil, nil)
	var reused int
	blind := 0
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Template != "claude" || r.Tier != "new" {
				continue
			}
			if r.SessionBeadID == "sess-1" {
				reused++
			} else if r.WorkBeadID == "" && !r.FloorGuarantee {
				blind++
			}
		}
	}
	if reused != 1 || blind != 0 {
		t.Fatalf("new requests under a hold with a protected seat: %d reusing sess-1, %d blind; want 1 reused and 0 blind", reused, blind)
	}
}

// AC-3: an in-flight seat inside its pending-create lease keeps its slot
// under a hold.
func TestPoolPlannerChurnHoldKeepsInFlightSeat(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0)}}
	sessions := churnedClosedRows("claude", now)
	sessions = append(sessions, sessionInfosFromBeads([]beads.Bead{pendingPoolSessionBeadAt("sess-2", now.Add(-10*time.Second))})...)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{"claude": 3}, nil, now, nil, nil)
	var kept int
	blind := 0
	for _, s := range states {
		for _, r := range s.Requests {
			if r.Template != "claude" || r.Tier != "new" {
				continue
			}
			if r.SessionBeadID == "sess-2" {
				kept++
			} else if r.WorkBeadID == "" && !r.FloorGuarantee {
				blind++
			}
		}
	}
	if kept != 1 || blind != 0 {
		t.Fatalf("new requests under a hold with an in-flight seat: %d keeping sess-2, %d blind; want 1 kept and 0 blind", kept, blind)
	}
}

// AC-3: a control dispatcher's demand never clamps — it is the controller's
// own machinery, not a blind pull — at the planner and in the merge.
func TestChurnHoldNeverWithholdsControlDispatcher(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	disp := config.Agent{Name: config.ControlDispatcherAgentName, StartCommand: "gc convoy control --serve"}
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "", intPtr(5), 0), disp}}
	sessions := churnedClosedRows(config.ControlDispatcherAgentName, now)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{config.ControlDispatcherAgentName: 2}, nil, now, nil, nil)
	_, blind, _ := countNewRequests(states, config.ControlDispatcherAgentName)
	if blind != 2 {
		t.Fatalf("control dispatcher's blind new requests under a hold = %d, want 2: its demand never clamps", blind)
	}

	in := collectedDemand{CustomCounts: map[string]int{config.ControlDispatcherAgentName: 3}}
	in.BlindSpawnAllowance = map[string]int{config.ControlDispatcherAgentName: 0}
	merged := mergeCollectedDemand(cfg, in)
	if got := merged.ScaleCheckCounts[config.ControlDispatcherAgentName]; got != 3 {
		t.Fatalf("merged control-dispatcher count under a 0 allowance = %d, want 3 unchanged", got)
	}
}

// AC-7: a hold is one template's: the planner still emits another
// template's blind requests in full.
func TestPoolPlannerChurnHoldLeavesOtherTemplatesBlindDemandAlone(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cfg := &config.City{Agents: []config.Agent{
		poolAgent("claude", "", intPtr(5), 0),
		poolAgent("cursor", "", intPtr(5), 0),
	}}
	sessions := churnedClosedRows("claude", now)
	states := computePoolDesiredStatesParkedAt(
		cfg, &assignedWorkDemand{}, sessions,
		map[string]int{"claude": 1, "cursor": 2}, nil, now, nil, nil)
	if _, blind, _ := countNewRequests(states, "claude"); blind != 0 {
		t.Fatalf("claude's blind new requests under its hold = %d, want 0", blind)
	}
	if _, blind, _ := countNewRequests(states, "cursor"); blind != 2 {
		t.Fatalf("cursor's blind new requests under claude's hold = %d, want 2 unchanged", blind)
	}
}
