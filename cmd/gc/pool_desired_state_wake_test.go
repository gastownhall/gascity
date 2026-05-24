package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// closedPoolSessionBead creates a closed pool-managed session bead whose
// template metadata matches the given qualified template name. Used to
// construct "session bead closed but template still configured" scenarios.
func closedPoolSessionBead(id, template string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "closed",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             template,
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityForClosedSession verifies that
// an in-progress work bead assigned to a closed pool session whose template is
// still configured and non-suspended produces a "wake-known-identity" request.
//
// This is the canonical "orphan recovery" case: a pool agent claimed work,
// the city restarted (or the session was killed), and the session bead is now
// closed — but the template is still live. The reconciler must revive the
// session rather than leaving the work stranded.
//
// This test FAILS on current code because computePoolDesiredStates skips
// all closed session beads without producing any request.
func TestComputePoolDesiredStates_WakeKnownIdentityForClosedSession(t *testing.T) {
	t.Skip("ga-6do4y.1: remove Skip and implement wake-known-identity tier in computePoolDesiredStates")
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "sess-1", "in_progress", 5),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, []beads.Bead{closed}, nil)

	wakeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "wake-known-identity" {
				wakeCount++
			}
		}
	}
	if wakeCount != 1 {
		t.Errorf("wake-known-identity count = %d, want 1 — closed session with known template must produce a wake request", wakeCount)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityUnknownAssigneeProducesNoRequest
// verifies that a work bead whose assignee does not match any session bead
// (open or closed) produces no request. An unknown assignee cannot be mapped
// to a known identity, so it remains orphaned.
func TestComputePoolDesiredStates_WakeKnownIdentityUnknownAssigneeProducesNoRequest(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "unknown-session-id", "in_progress", 5),
	}
	// No session beads at all — assignee doesn't resolve.
	result := ComputePoolDesiredStates(cfg, work, nil, nil)

	total := 0
	for _, ds := range result {
		total += len(ds.Requests)
	}
	if total != 0 {
		t.Errorf("total requests = %d, want 0 — unknown assignee must produce no request", total)
	}
}

// TestComputePoolDesiredStates_WakeKnownIdentityDedupsMultipleBeadsForSameSession
// verifies that two work beads both assigned to the same closed session
// deduplicate to exactly one wake-known-identity request, not two.
//
// This test FAILS on current code because zero requests are produced.
func TestComputePoolDesiredStates_WakeKnownIdentityDedupsMultipleBeadsForSameSession(t *testing.T) {
	t.Skip("ga-6do4y.1: remove Skip and implement wake-known-identity dedup in computePoolDesiredStates")
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "sess-1", "in_progress", 5),
		workBead("w2", "rig/claude", "sess-1", "open", 3),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, []beads.Bead{closed}, nil)

	wakeCount := 0
	for _, ds := range result {
		for _, req := range ds.Requests {
			if req.Tier == "wake-known-identity" {
				wakeCount++
			}
		}
	}
	if wakeCount != 1 {
		t.Errorf("wake-known-identity count = %d, want 1 — two beads for the same closed session must deduplicate to one wake request", wakeCount)
	}
}

// TestComputePoolDesiredStates_LiveSessionContinuesAsResumeTier verifies that
// open sessions still produce Tier="resume" and are not reclassified as
// wake-known-identity. Closed-session recovery must not touch live sessions.
func TestComputePoolDesiredStates_LiveSessionContinuesAsResumeTier(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "sess-live", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-live", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessions, nil)

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("expected 1 request, got %#v", result)
	}
	req := result[0].Requests[0]
	if req.Tier != "resume" {
		t.Errorf("tier = %q, want resume — live session must stay in resume tier", req.Tier)
	}
	if req.SessionBeadID != "sess-live" {
		t.Errorf("SessionBeadID = %q, want sess-live", req.SessionBeadID)
	}
}

// TestApplyNestedCaps_WakeKnownIdentityRanksBeforeNew verifies that when a cap
// admits only one request and both a wake-known-identity request and a new
// request have equal priority, wake-known-identity is accepted. The sort
// comparator in applyNestedCaps must treat "wake-known-identity" as a
// resume-like tier that ranks ahead of "new" at the same bead priority.
//
// This test FAILS on current code: the comparator only checks
// requests[i].Tier == "resume", so "wake-known-identity" and "new" are
// treated as equal, and the first input (new) is accepted instead.
func TestApplyNestedCaps_WakeKnownIdentityRanksBeforeNew(t *testing.T) {
	t.Skip("ga-6do4y.1: remove Skip and fix applyNestedCaps sort to prefer wake-known-identity over new")
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	// New request is listed first so current sort preserves it ahead of
	// wake-known-identity. After the fix, wake-known-identity wins.
	requests := []SessionRequest{
		{Template: "claude", Tier: "new", BeadPriority: 5},
		{Template: "claude", Tier: "wake-known-identity", SessionBeadID: "sess-closed", BeadPriority: 5},
	}

	result := applyNestedCaps(cfg, requests, nil)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if len(result[0].Requests) != 1 {
		t.Fatalf("accepted = %d, want 1 (cap=1)", len(result[0].Requests))
	}
	if result[0].Requests[0].Tier != "wake-known-identity" {
		t.Errorf("accepted tier = %q, want wake-known-identity — must rank before new at same priority", result[0].Requests[0].Tier)
	}
}
