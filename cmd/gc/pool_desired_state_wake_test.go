package main

import (
	"testing"
	"time"

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
// an in-progress work bead assigned to a configured, non-suspended pool
// template produces a "wake-known-identity" request when no live session owns
// it.
//
// This is the canonical "orphan recovery" case: a pool agent claimed work,
// the city restarted (or the session was killed), and the session bead is now
// closed — but the template is still live. The reconciler must revive the
// template rather than leaving the work stranded.
func TestComputePoolDesiredStates_WakeKnownIdentityForClosedSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "rig/claude", "in_progress", 5),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil)

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
// verifies that two work beads both assigned to the same configured template
// deduplicate to exactly one wake-known-identity request, not two.
func TestComputePoolDesiredStates_WakeKnownIdentityDedupsMultipleBeadsForSameSession(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", nil, 0)},
	}
	work := []beads.Bead{
		workBead("w1", "rig/claude", "rig/claude", "in_progress", 5),
		workBead("w2", "rig/claude", "rig/claude", "open", 3),
	}
	closed := closedPoolSessionBead("sess-1", "rig/claude")

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{closed}), nil)

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

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

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
func TestApplyNestedCaps_WakeKnownIdentityRanksBeforeNew(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(1), 0)},
	}
	// New request is listed first so current sort preserves it ahead of
	// wake-known-identity. After the fix, wake-known-identity wins.
	requests := []SessionRequest{
		{Template: "claude", Tier: "new", BeadPriority: 5},
		{Template: "claude", Tier: "wake-known-identity", SessionBeadID: "sess-closed", BeadPriority: 5},
	}

	result := applyNestedCaps(cfg, requests, nil, nil)

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

// TestComputePoolDesiredStates_FreshWakeSkipsAsleepResume verifies the
// wake_mode="fresh" guard for stale asleep pool sessions
// (gastownhall/gascity#4849).
//
// Before the guard, computePoolDesiredStates emitted a Tier:"resume" request for
// any assigned work bead whose assignee resolved to a non-closed session bead —
// without consulting the agent's wake_mode or the session's asleep-ness. So a
// wake_mode="fresh" pool agent with a stale asleep session (left over from before
// a city stop) was told to resume a session it would never reuse, leaving the
// assigned work bound to a row the pool cannot materialize.
//
// The table pins the discriminating branches: a fresh-wake agent must SKIP the
// asleep resume and plan a clean session bound to the same work
// (Tier:"wake-known-identity" with no SessionBeadID) — including under the
// default min_active_sessions=0, where no min-fill request exists to cover it;
// a default/resume-wake agent must STILL resume the asleep session (the guard
// must not over-fire); and a fresh-wake agent must still resume a LIVE session
// (the guard is asleep-specific, not a blanket fresh-agent block).
func TestComputePoolDesiredStates_FreshWakeSkipsAsleepResume(t *testing.T) {
	const sessionID = "sess-1"
	cases := []struct {
		name           string
		wakeMode       string
		sessionState   string
		minSessions    int
		wantTier       string
		wantSessionID  string // resume target; "" when a clean session is expected
		wantWorkBeadID string
	}{
		{
			name:           "fresh wake skips asleep resume and plans a clean session",
			wakeMode:       "fresh",
			sessionState:   "asleep",
			minSessions:    1,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			// The default config: min_active_sessions unset (0) means no
			// min-fill request exists, so the guard itself must carry the
			// replacement or the work is stranded.
			name:           "fresh wake plans a clean session at default min_active_sessions",
			wakeMode:       "fresh",
			sessionState:   "asleep",
			minSessions:    0,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			// normalizeInfoState folds "drained" into StateAsleep, so a
			// drained session takes the same path.
			name:           "fresh wake treats drained as asleep",
			wakeMode:       "fresh",
			sessionState:   "drained",
			minSessions:    0,
			wantTier:       "wake-known-identity",
			wantSessionID:  "",
			wantWorkBeadID: "w1",
		},
		{
			name:           "resume wake still resumes asleep session",
			wakeMode:       "", // unset → EffectiveWakeMode defaults to "resume"
			sessionState:   "asleep",
			minSessions:    1,
			wantTier:       "resume",
			wantSessionID:  sessionID,
			wantWorkBeadID: "w1",
		},
		{
			name:           "fresh wake still resumes a live session",
			wakeMode:       "fresh",
			sessionState:   "active",
			minSessions:    1,
			wantTier:       "resume",
			wantSessionID:  sessionID,
			wantWorkBeadID: "w1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := poolAgent("claude", "", intPtr(5), tc.minSessions)
			agent.WakeMode = tc.wakeMode
			cfg := &config.City{Agents: []config.Agent{agent}}
			sessions := []beads.Bead{poolSessionBeadWithState(sessionID, tc.sessionState, "")}
			work := []beads.Bead{workBead("w1", "claude", sessionID, "in_progress", 2)}

			result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), nil)

			if len(result) != 1 {
				t.Fatalf("len(result) = %d, want 1: %#v", len(result), result)
			}
			reqs := result[0].Requests
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want 1: %#v", len(reqs), reqs)
			}
			if reqs[0].Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", reqs[0].Tier, tc.wantTier)
			}
			if reqs[0].SessionBeadID != tc.wantSessionID {
				t.Errorf("SessionBeadID = %q, want %q", reqs[0].SessionBeadID, tc.wantSessionID)
			}
			if reqs[0].WorkBeadID != tc.wantWorkBeadID {
				t.Errorf("WorkBeadID = %q, want %q — the replacement session must stay bound to the assigned work", reqs[0].WorkBeadID, tc.wantWorkBeadID)
			}
			// The doomed-resume regression: a fresh-wake agent must never carry a
			// resume for the stale asleep session.
			if tc.wakeMode == "fresh" && tc.sessionState == "asleep" {
				for _, r := range reqs {
					if r.Tier == "resume" && r.SessionBeadID == sessionID {
						t.Errorf("fresh-wake agent resumed stale asleep session %s: %#v", sessionID, r)
					}
				}
			}
		})
	}
}

// freshWakeRequests runs the parked planner for one fresh-wake pool agent with
// one pool session and returns the pool's requests.
func freshWakeRequests(t *testing.T, sessionState string, minSessions int, work []beads.Bead, ready []bool) []SessionRequest {
	t.Helper()
	return poolWakeRequests(t, "fresh", sessionState, minSessions, work, ready)
}

// poolWakeRequests runs the parked planner for one pool agent with the given
// wake mode and one pool session in sessionState, and returns the pool's
// requests.
func poolWakeRequests(t *testing.T, wakeMode, sessionState string, minSessions int, work []beads.Bead, ready []bool) []SessionRequest {
	t.Helper()
	agent := poolAgent("claude", "", intPtr(5), minSessions)
	agent.WakeMode = wakeMode
	cfg := &config.City{Agents: []config.Agent{agent}}
	sessions := []beads.Bead{poolSessionBeadWithState("sess-1", sessionState, "")}
	states := computePoolDesiredStatesParkedAt(cfg, work, ready, sessionInfosFromBeads(sessions), nil, nil, time.Time{}, nil, nil)
	var reqs []SessionRequest
	for _, ds := range states {
		reqs = append(reqs, ds.Requests...)
	}
	return reqs
}

// TestComputePoolDesiredStates_FreshWakeSkipsUnreadyOpenStep pins the
// readiness gate on the fresh+asleep branch (gastownhall/gascity#8172): an open
// step assigned to a wake_mode="fresh" agent's asleep session, with no
// store-scoped readiness proof, must neither plan a clean session for that step
// nor resume the stale asleep session. Before the gate, every tick planned a
// fresh session that could not claim the blocked step, drained, and respawned.
func TestComputePoolDesiredStates_FreshWakeSkipsUnreadyOpenStep(t *testing.T) {
	cases := []struct {
		name         string
		sessionState string
		minSessions  int
		ready        []bool
	}{
		{name: "flag false", sessionState: "asleep", ready: []bool{false}},
		{name: "empty non-nil flags deny unproven open work", sessionState: "asleep", ready: []bool{}},
		{name: "drained normalizes to asleep", sessionState: "drained", ready: []bool{false}},
		{name: "min fill is not bound to the blocked step", sessionState: "asleep", minSessions: 1, ready: []bool{false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := []beads.Bead{workBead("w1", "claude", "sess-1", "open", 2)}
			reqs := freshWakeRequests(t, tc.sessionState, tc.minSessions, work, tc.ready)
			for _, req := range reqs {
				if req.WorkBeadID == "w1" {
					t.Errorf("request %+v binds the unready open step w1, want none", req)
				}
				if req.SessionBeadID == "sess-1" {
					t.Errorf("request %+v resumes the stale asleep session, want none", req)
				}
			}
			if tc.minSessions == 0 && len(reqs) != 0 {
				t.Errorf("requests = %+v, want none at min_active_sessions=0", reqs)
			}
			if tc.minSessions > 0 && len(reqs) == 0 {
				t.Errorf("min-fill demand disappeared with the blocked step, want an unbound floor request")
			}
		})
	}
}

// TestComputePoolDesiredStates_FreshWakeGatesOnReadiness pins the actionable
// side of the fresh-wake readiness gate: proven-ready open work and claimed
// in-progress work still get exactly one clean wake-known-identity request
// (#4849's recovery), an unready open bead listed first does not consume the
// template's single wake slot, and nil readiness keeps the actionable-input
// wrappers' behavior.
func TestComputePoolDesiredStates_FreshWakeGatesOnReadiness(t *testing.T) {
	cases := []struct {
		name     string
		work     []beads.Bead
		ready    []bool
		wantWork string
	}{
		{
			name:     "ready open step",
			work:     []beads.Bead{workBead("w1", "claude", "sess-1", "open", 2)},
			ready:    []bool{true},
			wantWork: "w1",
		},
		{
			name:     "in-progress step without a readiness flag stays actionable",
			work:     []beads.Bead{workBead("w1", "claude", "sess-1", "in_progress", 2)},
			ready:    []bool{false},
			wantWork: "w1",
		},
		{
			name: "blocked first does not consume the wake slot of a ready second",
			work: []beads.Bead{
				workBead("w-blocked", "claude", "sess-1", "open", 2),
				workBead("w-ready", "claude", "sess-1", "open", 2),
			},
			ready:    []bool{false, true},
			wantWork: "w-ready",
		},
		{
			name: "ready first step wakes even when a later step is unproven",
			work: []beads.Bead{
				workBead("w-ready", "claude", "sess-1", "open", 2),
				workBead("w-blocked", "claude", "sess-1", "open", 2),
			},
			ready:    []bool{true, false},
			wantWork: "w-ready",
		},
		{
			name:     "nil readiness keeps actionable-input behavior",
			work:     []beads.Bead{workBead("w1", "claude", "sess-1", "open", 2)},
			ready:    nil,
			wantWork: "w1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := freshWakeRequests(t, "asleep", 0, tc.work, tc.ready)
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v, want exactly one", reqs)
			}
			req := reqs[0]
			if req.Tier != "wake-known-identity" || req.SessionBeadID != "" || req.WorkBeadID != tc.wantWork {
				t.Errorf("request = {Tier:%q SessionBeadID:%q WorkBeadID:%q}, want {wake-known-identity \"\" %q}",
					req.Tier, req.SessionBeadID, req.WorkBeadID, tc.wantWork)
			}
		})
	}
}

// TestComputePoolDesiredStates_FreshWakeGateLeavesResumesUnchanged pins the
// gate's scope: it applies only to a fresh-wake agent's asleep assignee. A
// resume-mode agent with an asleep session, and a fresh-wake agent whose
// session is still live, keep resuming that session for unproven open work.
func TestComputePoolDesiredStates_FreshWakeGateLeavesResumesUnchanged(t *testing.T) {
	cases := []struct {
		name         string
		wakeMode     string
		sessionState string
	}{
		{name: "resume-mode agent still resumes for not-ready open work", wakeMode: "", sessionState: "asleep"},
		{name: "fresh wake still resumes a live session for not-ready open work", wakeMode: "fresh", sessionState: "active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := []beads.Bead{workBead("w1", "claude", "sess-1", "open", 2)}
			reqs := poolWakeRequests(t, tc.wakeMode, tc.sessionState, 0, work, []bool{false})
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v, want exactly one", reqs)
			}
			req := reqs[0]
			if req.Tier != "resume" || req.SessionBeadID != "sess-1" || req.WorkBeadID != "w1" {
				t.Errorf("request = {Tier:%q SessionBeadID:%q WorkBeadID:%q}, want {resume \"sess-1\" \"w1\"}",
					req.Tier, req.SessionBeadID, req.WorkBeadID)
			}
		})
	}
}
