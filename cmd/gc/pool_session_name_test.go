package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPoolSessionName(t *testing.T) {
	tests := []struct {
		template string
		beadID   string
		want     string
	}{
		{"gascity/claude", "mc-xyz", "claude-mc-xyz"},
		{"claude", "mc-abc", "claude-mc-abc"},
		{"myrig/codex", "mc-123", "codex-mc-123"},
		{"control-dispatcher", "mc-wfc", "control-dispatcher-mc-wfc"},
		{"gs.polecat", "mc-dot", "gs__polecat-mc-dot"},
		{"myrig/gs.polecat", "mc-rigdot", "gs__polecat-mc-rigdot"},
	}
	for _, tt := range tests {
		got := PoolSessionName(tt.template, tt.beadID)
		if got != tt.want {
			t.Errorf("PoolSessionName(%q, %q) = %q, want %q", tt.template, tt.beadID, got, tt.want)
		}
	}
}

func TestGCSweepSessionBeads_ClosesOrphans(t *testing.T) {
	store := beads.NewMemStore()

	// Session bead with no assigned work.
	orphan, _ := store.Create(beads.Bead{Title: "orphan session", Type: "session"})

	// Session bead with assigned work.
	active, _ := store.Create(beads.Bead{Title: "active session", Type: "session"})
	workBead, _ := store.Create(beads.Bead{
		Title:    "work item",
		Assignee: active.ID,
		Status:   "in_progress",
	})
	_ = workBead

	sessionBeads := []beads.Bead{orphan, active}
	allWork := []beads.Bead{workBead}

	closed := GCSweepSessionBeads(store, sessionBeads, allWork)

	if len(closed) != 1 {
		t.Fatalf("closed %d beads, want 1", len(closed))
	}
	if closed[0] != orphan.ID {
		t.Errorf("closed %q, want %q", closed[0], orphan.ID)
	}

	// Verify the orphan is actually closed in the store.
	got, _ := store.Get(orphan.ID)
	if got.Status != "closed" {
		t.Errorf("orphan status = %q, want closed", got.Status)
	}

	// Active session should still be open.
	got, _ = store.Get(active.ID)
	if got.Status == "closed" {
		t.Error("active session was closed, should stay open")
	}
}

func TestGCSweepSessionBeads_KeepsBlockedAssigned(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{
		Title:  "session",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"state": "active",
		},
	})

	// Work bead is open (blocked) but assigned to this session.
	blocked, _ := store.Create(beads.Bead{
		Title:    "blocked work",
		Assignee: sess.ID,
		Status:   "open",
	})
	_ = blocked

	sessionBeads := []beads.Bead{sess}
	allWork := []beads.Bead{blocked}

	closed := GCSweepSessionBeads(store, sessionBeads, allWork)

	if len(closed) != 0 {
		t.Errorf("closed %d beads, want 0 (blocked work keeps session alive)", len(closed))
	}
	got, err := store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active when sweep skips close", got.Metadata["state"])
	}
}

func TestGCSweepSessionBeads_ClosesWhenAllWorkClosed(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{Title: "session", Type: "session"})

	// Work bead is closed — session has no remaining work.
	done, _ := store.Create(beads.Bead{
		Title:    "done work",
		Assignee: sess.ID,
	})
	_ = store.Close(done.ID)
	done, _ = store.Get(done.ID)

	sessionBeads := []beads.Bead{sess}
	allWork := []beads.Bead{done}

	closed := GCSweepSessionBeads(store, sessionBeads, allWork)

	if len(closed) != 1 {
		t.Errorf("closed %d beads, want 1 (all work done)", len(closed))
	}
}

func TestGCSweepSessionBeads_SkipsAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()

	sess, _ := store.Create(beads.Bead{Title: "session", Type: "session"})
	_ = store.Close(sess.ID)
	sess, _ = store.Get(sess.ID)

	sessionBeads := []beads.Bead{sess}

	closed := GCSweepSessionBeads(store, sessionBeads, nil)

	if len(closed) != 0 {
		t.Errorf("closed %d beads, want 0 (already closed)", len(closed))
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensMissingPoolAssignee(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
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
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		nil,
		[]beads.Bead{work},
	)
	if len(released) != 1 || released[0] != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_KeepsOpenSessionOwnership(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "live pool work",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "worker"},
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
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		[]beads.Bead{session},
		[]beads.Bead{work},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-live" {
		t.Fatalf("assignee = %q, want worker-live", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensStaleDirectAssigneeForNamedBackedTemplate(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stale direct-session work",
		Assignee: "mc-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
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

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(store, cfg, nil, []beads.Bead{work})
	if len(released) != 1 || released[0] != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_PreservesCanonicalNamedIdentity(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "named owner work",
		Assignee: "reviewer",
		Metadata: map[string]string{"gc.routed_to": "worker"},
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

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignments(store, cfg, nil, []beads.Bead{work})
	if len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "reviewer" {
		t.Fatalf("assignee = %q, want reviewer", got.Assignee)
	}
}
