package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// sweepNamedFallbackConfig is the AC-7 city: a named session "baker" backed by
// template "namedworker", plus an unrelated pool-capable template "poolworker"
// the work bead routes to (so the named-route keep gate deliberately misses),
// and a session_template that puts the named identity's runtime name at
// "mc-baker" — different from the identity itself, so only the awake set's
// configured-named fallback can credit the successor row with the identity.
func sweepNamedFallbackConfig() *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "mc", SessionTemplate: "mc-{{.Name}}"},
		NamedSessions: []config.NamedSession{{Name: "baker", Template: "namedworker", Mode: "on_demand"}},
		Agents: []config.Agent{
			{Name: "namedworker"},
			{Name: "poolworker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}
}

// TestReleaseOrphanedPoolAssignments_CreditsConfiguredNamedFallbackIdentity is
// the Part A regression for issue #6362 (Gap 2): an open configured-named row
// that lacks configured_named_identity is credited with its named identity by
// the awake set's predicate (sessionAssigneeMatches), so the dead-assignee
// sweep must credit it too. The bead is assigned to the bare identity "baker"
// and routed to a template other than baker's backing template, so the
// named-route gate misses by construction; without the fallback in the sweep's
// identity sets the controller can wake this row for the bead in one tick and
// reopen the same bead in the next.
func TestReleaseOrphanedPoolAssignments_CreditsConfiguredNamedFallbackIdentity(t *testing.T) {
	cfg := sweepNamedFallbackConfig()
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "baker")
	if runtimeName == "baker" {
		t.Fatalf("runtime name %q equals the identity; the fixture needs them distinct so only the fallback credits the row", runtimeName)
	}

	store := beads.NewMemStore()
	successor, err := store.Create(beads.Bead{
		Title:  "named successor",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":          runtimeName,
			"template":              "namedworker",
			"state":                 "awake",
			namedSessionMetadataKey: "true",
		},
	})
	if err != nil {
		t.Fatalf("create successor session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "work held under the named identity",
		Type:     "task",
		Assignee: "baker",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "poolworker",
			"gc.session_id":              successor.ID,
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	// Refresh the snapshot after the status write: the sweep fences its release
	// on the snapshot's revision, and a stale pre-update read is refused as
	// "moved" rather than released.
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		cfg,
		"",
		sessionInfosFromBeads([]beads.Bead{successor}),
		[]beads.Bead{work},
		nil, nil, nil, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %v, want none: the awake set credits %q with open row %s (session_name %q is its runtime name, template matches its backing template), so the sweep must credit it too",
			released, "baker", successor.ID, runtimeName)
	}
	got, _ := store.Get(work.ID)
	if got.Status != "in_progress" || got.Assignee != "baker" {
		t.Fatalf("work after sweep = status %q assignee %q, want in_progress/baker (the claim is live)", got.Status, got.Assignee)
	}
}
