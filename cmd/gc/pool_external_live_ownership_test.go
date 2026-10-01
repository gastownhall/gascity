package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// A live session's own bead is already represented by its resume request,
// even when the bead's recorded worktree is also reported live.
func TestComputePoolDesiredStatesWithLivenessCountsOwnedWorkOnce(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)}}
	workDir := t.TempDir()
	owned := workBead("owned", "rig/claude", "session-1", "in_progress", 2)
	owned.Metadata[beadmeta.WorkDirMetadataKey] = workDir
	trace := newPoolDesiredStateTestTrace("rig/claude")

	result := ComputePoolDesiredStatesWithLiveness(
		cfg,
		[]beads.Bead{owned},
		sessionInfosFromBeads([]beads.Bead{sessionBead("session-1", "open")}),
		map[string]int{"rig/claude": 1},
		map[string]scaleCheckDemand{"rig/claude": {WorkBeadIDs: []string{"queued"}}},
		trace,
		map[string]bool{pathutil.NormalizePathForCompare(workDir): true},
	)
	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("desired requests = %+v, want one resume and one new request", result)
	}
	if got := result[0].Requests[0]; got.Tier != "resume" || got.WorkBeadID != "owned" || got.SessionBeadID != "session-1" {
		t.Errorf("first request = %+v, want the live session's own resume", got)
	}
	if got := result[0].Requests[1]; got.Tier != "new" || got.WorkBeadID != "queued" {
		t.Errorf("second request = %+v, want one new session for queued work", got)
	}
	if got := admittedExternalWorkBeads(t, trace); len(got) != 0 {
		t.Errorf("owned bead was also charged as external occupancy: %v", got)
	}
}

// A genuinely external live worktree still uses one of the two pool slots.
func TestComputePoolDesiredStatesWithLivenessCountsExternalWork(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)}}
	workDir := t.TempDir()
	external := workBead("external", "rig/claude", "", "in_progress", 2)
	external.Metadata[beadmeta.WorkDirMetadataKey] = workDir
	trace := newPoolDesiredStateTestTrace("rig/claude")

	result := ComputePoolDesiredStatesWithLiveness(
		cfg,
		[]beads.Bead{external},
		nil,
		map[string]int{"rig/claude": 2},
		map[string]scaleCheckDemand{"rig/claude": {WorkBeadIDs: []string{"queued-1", "queued-2"}}},
		trace,
		map[string]bool{pathutil.NormalizePathForCompare(workDir): true},
	)
	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("desired requests = %+v, want exactly one new request beside the external occupant", result)
	}
	if got := result[0].Requests[0]; got.Tier != "new" || got.WorkBeadID != "queued-1" {
		t.Errorf("request = %+v, want the first queued bead in the remaining slot", got)
	}
	if got := admittedExternalWorkBeads(t, trace); len(got) != 1 || got[0] != "external" {
		t.Errorf("external occupancy admitted %v, want [external]", got)
	}
}
