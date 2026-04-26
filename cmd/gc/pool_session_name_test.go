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

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

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

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

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

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

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

	closed := GCSweepSessionBeads(store, nil, sessionBeads)

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
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
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

func TestReleaseOrphanedPoolAssignments_UpdatesRigStoreFallback(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "rig/worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "rig", Prefix: "ga"}},
			Agents: []config.Agent{{Name: "worker", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		nil,
		[]beads.Bead{work},
		nil,
		map[string]beads.Store{"rig": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("rig status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("rig assignee = %q, want empty", got.Assignee)
	}
	if _, err := cityStore.Get(work.ID); err == nil {
		t.Fatalf("city store unexpectedly contains rig work bead %s", work.ID)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensRigStoreMissingPoolAssignee(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	citySession, err := cityStore.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Assignee: "worker-live",
		Metadata: map[string]string{
			"session_name":         "worker-dead",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create city session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}
	if citySession.ID != work.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", citySession.ID, work.ID)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
	gotSession, err := cityStore.Get(citySession.ID)
	if err != nil {
		t.Fatalf("Get city session bead: %v", err)
	}
	if gotSession.Type != sessionBeadType {
		t.Fatalf("city session type = %q, want %q", gotSession.Type, sessionBeadType)
	}
	if gotSession.Assignee != "worker-live" {
		t.Fatalf("city session assignee = %q, want worker-live", gotSession.Assignee)
	}
	if gotSession.Metadata["session_name"] != "worker-dead" ||
		gotSession.Metadata["template"] != "worker" ||
		gotSession.Metadata[poolManagedMetadataKey] != boolMetadata(true) {
		t.Fatalf("city session metadata changed: %#v", gotSession.Metadata)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensCrossStoreIDCollisions(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cityWork, err := cityStore.Create(beads.Bead{
		Title:    "orphaned city pool work",
		Assignee: "worker-city-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create city work bead: %v", err)
	}
	if err := cityStore.Update(cityWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set city work status: %v", err)
	}
	cityWork, err = cityStore.Get(cityWork.ID)
	if err != nil {
		t.Fatalf("Reload city work bead: %v", err)
	}
	rigWork, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-rig-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	rigWork, err = rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}
	if cityWork.ID != rigWork.ID {
		t.Fatalf("test setup expected overlapping city/rig IDs, got city %q rig %q", cityWork.ID, rigWork.ID)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		nil,
		[]beads.Bead{cityWork, rigWork},
		[]beads.Store{cityStore, rigStore},
		nil,
	)
	if len(released) != 2 || released[0].ID != cityWork.ID || released[1].ID != rigWork.ID {
		t.Fatalf("released = %v, want [%s %s]", released, cityWork.ID, rigWork.ID)
	}
	gotCity, err := cityStore.Get(cityWork.ID)
	if err != nil {
		t.Fatalf("Get city work bead: %v", err)
	}
	if gotCity.Status != "open" || gotCity.Assignee != "" {
		t.Fatalf("city work = status %q assignee %q, want open/unassigned", gotCity.Status, gotCity.Assignee)
	}
	gotRig, err := rigStore.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if gotRig.Status != "open" || gotRig.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", gotRig.Status, gotRig.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsStoreAwareEntryWithoutOwnerStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "orphaned rig pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set rig work status: %v", err)
	}
	work, err = rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload rig work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		cityStore,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		nil,
		[]beads.Bead{work},
		[]beads.Store{nil},
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none without owner store", released)
	}
	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-dead" {
		t.Fatalf("rig work = status %q assignee %q, want unchanged in_progress/worker-dead", got.Status, got.Assignee)
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
		nil,
		nil,
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

	released := releaseOrphanedPoolAssignments(store, cfg, nil, []beads.Bead{work}, nil, nil)
	if len(released) != 1 || released[0].ID != work.ID {
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

	released := releaseOrphanedPoolAssignments(store, cfg, nil, []beads.Bead{work}, nil, nil)
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
