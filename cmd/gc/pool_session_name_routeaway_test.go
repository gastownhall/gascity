package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestReleaseOrphanedPoolAssignments_ReleasesRoutedAwayFromLiveSourceSession is
// the regression guard for sr-wz8.3. When L1 escalates a ticket it re-routes the
// bead to l2-<family> but the bead stays ASSIGNED to the live L1 source session.
// The live-session ownership guard (openSessionOwnsWork / liveOpenSessionAssignmentExists)
// is meant for DEAD-session orphans and must NOT protect an assignment that has
// been routed AWAY to a different agent than the owning session's own agent —
// otherwise the l2 pool never gains demand and the escalation deadlocks until the
// source session is closed (the live repro's only working recipe).
func TestReleaseOrphanedPoolAssignments_ReleasesRoutedAwayFromLiveSourceSession(t *testing.T) {
	store := beads.NewMemStore()
	l1Session, err := store.Create(beads.Bead{
		Title:  "l1 source",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "l1-live",
			"template":             "l1-support",
			"agent_name":           "l1-support",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create l1 session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "escalated ticket",
		Assignee: "l1-live",
		Metadata: map[string]string{"gc.routed_to": "l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{
			{Name: "l1-support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "l2-erp", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		}},
		"",
		sessionInfosFromBeads([]beads.Bead{l1Session}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] (routed-away bead must be released even though the L1 source session is live)", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty (released so l2-erp pool can claim)", got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_KeepsRoutedToOwnAgentLiveSession guards that
// the sr-wz8.3 route-away release does NOT steal a bead a live session legitimately
// owns: when routed_to points at the owning session's OWN agent, the live-session
// ownership guard must still keep the assignment untouched.
func TestReleaseOrphanedPoolAssignments_KeepsRoutedToOwnAgentLiveSession(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "l2 owner",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "l2-live",
			"template":             "l2-erp",
			"agent_name":           "l2-erp",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "l2 in-progress work",
		Assignee: "l2-live",
		Metadata: map[string]string{"gc.routed_to": "l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{
			{Name: "l1-support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "l2-erp", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		}},
		"",
		sessionInfosFromBeads([]beads.Bead{session}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (live session owns work routed to its own agent)", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Assignee != "l2-live" {
		t.Fatalf("assignee = %q, want l2-live (must not steal legitimately-owned work)", got.Assignee)
	}
}

// qualifiedSupportAgents mirrors the real srv fleet naming: rig-scoped,
// pack-bound agents whose QualifiedName() is the dotted "st/support.<name>"
// form (Dir="st", BindingName="support"). The flat-name tests above cannot
// catch a divergence between the routed-target lookup (findAgentByTemplate on
// gc.routed_to) and the owning-session lookup (normalizedSessionTemplate on the
// session bead's template metadata) under this production naming — these two
// tests do (sr-wz8.3 guard #1: never over-release a legitimately-owned bead).
func qualifiedSupportAgents() []config.Agent {
	return []config.Agent{
		{Name: "l1-support", Dir: "st", BindingName: "support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		{Name: "l2-erp", Dir: "st", BindingName: "support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
	}
}

func TestReleaseOrphanedPoolAssignments_KeepsQualifiedOwnAgentLiveSession(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "l2 owner (qualified)",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "l2-erp-live",
			"template":             "st/support.l2-erp",
			"agent_name":           "st/support.l2-erp",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "l2 in-progress work (qualified)",
		Assignee: "l2-erp-live",
		Metadata: map[string]string{"gc.routed_to": "st/support.l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: qualifiedSupportAgents()},
		"",
		sessionInfosFromBeads([]beads.Bead{session}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (live session owns work routed to its OWN qualified agent st/support.l2-erp — must not over-release)", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Assignee != "l2-erp-live" {
		t.Fatalf("assignee = %q, want l2-erp-live", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReleasesQualifiedRoutedAwayFromLiveSource(t *testing.T) {
	store := beads.NewMemStore()
	l1Session, err := store.Create(beads.Bead{
		Title:  "l1 source (qualified)",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "l1-support-live",
			"template":             "st/support.l1-support",
			"agent_name":           "st/support.l1-support",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create l1 session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "escalated ticket (qualified)",
		Assignee: "l1-support-live",
		Metadata: map[string]string{"gc.routed_to": "st/support.l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: qualifiedSupportAgents()},
		"",
		sessionInfosFromBeads([]beads.Bead{l1Session}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] (bead routed away to st/support.l2-erp while assigned to live st/support.l1-support source must release)", released, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_KeepsWhenTargetAgentOwnerSharesAliasHistoryIdentity
// guards the over-release regression flagged in review: sessionBeadAssigneeIdentities
// includes stale alias_history entries, so one identity string can be held by two live
// sessions (one via alias_history, another as its current name — see
// TestEnsureSessionNameAvailable_AllowsLiveAliasHistoryReuse). The route-away decision
// must scan ALL matching sessions and preserve the bead when ANY store-scoped owner is
// the routed-to agent — it must not release based on the first (wrong) match.
func TestReleaseOrphanedPoolAssignments_KeepsWhenTargetAgentOwnerSharesAliasHistoryIdentity(t *testing.T) {
	store := beads.NewMemStore()
	// Session A (agent l1-support) holds "shared-id" ONLY in alias_history (renamed away).
	// Listed first so a first-match resolver would wrongly pick it.
	sessionA, err := store.Create(beads.Bead{
		Title:  "l1 stale-history holder",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "l1-current",
			"alias_history":        "shared-id",
			"template":             "l1-support",
			"agent_name":           "l1-support",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session A: %v", err)
	}
	// Session B (agent l2-erp, the routed-to target) currently holds "shared-id" and is
	// legitimately working the bead.
	sessionB, err := store.Create(beads.Bead{
		Title:  "l2 current owner",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "shared-id",
			"template":             "l2-erp",
			"agent_name":           "l2-erp",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session B: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "l2's legitimately-owned work",
		Assignee: "shared-id",
		Metadata: map[string]string{"gc.routed_to": "l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{
			{Name: "l1-support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "l2-erp", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		}},
		"",
		sessionInfosFromBeads([]beads.Bead{sessionA, sessionB}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (l2-erp session B legitimately owns work routed to l2-erp; session A's stale alias_history match must not cause over-release)", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Assignee != "shared-id" {
		t.Fatalf("assignee = %q, want shared-id (must not release legitimately-owned work)", got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_PreservesRoutedAwayWhileSourceMidTurn is the
// sr-wz8.3 liveness guard end-to-end. gc sling stamps gc.routed_to WITHOUT
// clearing the assignee or the source session's currently_processing_bead_id, so
// on the reconciler tick right after a mid-turn escalation the source still reads
// as processing the bead. Releasing then would let the l2 pool claim and start it
// while L1 is still producing output on it (racing L1's non-CAS completion write).
// The release must hold until the source has moved off the bead.
func TestReleaseOrphanedPoolAssignments_PreservesRoutedAwayWhileSourceMidTurn(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "escalated ticket (source mid-turn)",
		Assignee: "l1-live",
		Metadata: map[string]string{"gc.routed_to": "l2-erp"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	// currently_processing_bead_id still names the routed-away bead: L1 is mid-turn.
	l1Session, err := store.Create(beads.Bead{
		Title:  "l1 source (mid-turn)",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":                 "l1-live",
			"template":                     "l1-support",
			"agent_name":                   "l1-support",
			"currently_processing_bead_id": work.ID,
			poolManagedMetadataKey:         boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create l1 session bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{
			{Name: "l1-support", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
			{Name: "l2-erp", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		}},
		"",
		sessionInfosFromBeads([]beads.Bead{l1Session}),
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (source session is still mid-turn on the bead)", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Assignee != "l1-live" {
		t.Fatalf("assignee = %q, want l1-live (must not release while source is mid-turn)", got.Assignee)
	}
}

// TestAssigneeRoutedAwayFromOwnAgent_PreservesWhileSourceMidTurn pins the liveness
// gate directly, both directions: a source still processing THIS bead preserves it;
// once the source has moved to a different bead the same route-away releases.
func TestAssigneeRoutedAwayFromOwnAgent_PreservesWhileSourceMidTurn(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "l1-support"},
		{Name: "l2-erp"},
	}}
	source := sessionInfosFromBeads([]beads.Bead{{
		ID:     "s-l1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name":                 "l1-live",
			"template":                     "l1-support",
			"agent_name":                   "l1-support",
			"currently_processing_bead_id": "wb-9",
		},
	}})
	if assigneeRoutedAwayFromOwnAgent(cfg, "", source, "l1-live", "l2-erp", "", "wb-9", false) {
		t.Fatalf("source still mid-turn on wb-9 must preserve the bead")
	}
	if !assigneeRoutedAwayFromOwnAgent(cfg, "", source, "l1-live", "l2-erp", "", "wb-other", false) {
		t.Fatalf("source no longer processing this bead: route-away must release")
	}
}

// TestAssigneeRoutedAwayFromOwnAgent_CityScopedOwnerNeverHandsOff covers the
// cross-store-eligible short-circuit sjarmak flagged as untested: a city-scoped
// owner federates work across every store (vp-kvp) regardless of routed_to, so it
// is never an escalation handoff even with storeRefAware on and a differing ref.
func TestAssigneeRoutedAwayFromOwnAgent_CityScopedOwnerNeverHandsOff(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "city-worker", Scope: "city"},
			{Name: "l2-erp", Dir: "riga"},
		},
	}
	owner := sessionInfosFromBeads([]beads.Bead{{
		ID:     "s-city",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name": "city-singleton",
			"template":     "city-worker",
			"agent_name":   "city-worker",
		},
	}})
	if assigneeRoutedAwayFromOwnAgent(cfg, cityPath, owner, "city-singleton", "l2-erp", "riga", "wb-1", true) {
		t.Fatalf("city-scoped owner must never be treated as a route-away handoff")
	}
}

// TestAssigneeRoutedAwayFromOwnAgent_StoreRefAwareReleasesReachableSourceIgnoringDifferentStoreOwner
// exercises the storeRefAware scoping block over differing rig store refs: the
// reachable riga source (a different agent than the routed target) makes this a
// genuine handoff, while a same-identity session in rigb is a different concrete
// store and must be scoped out rather than preserve the bead (#3621 / #1544).
func TestAssigneeRoutedAwayFromOwnAgent_StoreRefAwareReleasesReachableSourceIgnoringDifferentStoreOwner(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb")},
		},
		Agents: []config.Agent{
			{Name: "l1-support", Dir: "riga"},
			{Name: "l2-erp", Dir: "riga"},
			{Name: "l2-erp-b", Dir: "rigb"},
		},
	}
	sessions := sessionInfosFromBeads([]beads.Bead{
		{ID: "s-riga", Status: "open", Type: sessionBeadType, Metadata: map[string]string{
			"session_name": "shared-id", "template": "l1-support", "agent_name": "l1-support",
		}},
		{ID: "s-rigb", Status: "open", Type: sessionBeadType, Metadata: map[string]string{
			"session_name": "shared-id", "template": "l2-erp-b", "agent_name": "l2-erp-b",
		}},
	})
	if !assigneeRoutedAwayFromOwnAgent(cfg, cityPath, sessions, "shared-id", "l2-erp", "riga", "wb-riga", true) {
		t.Fatalf("reachable riga source (l1-support) is a genuine handoff; the rigb same-identity session is a different store and must not preserve the bead")
	}
}
