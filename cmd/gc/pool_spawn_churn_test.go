package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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
