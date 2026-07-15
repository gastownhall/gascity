package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const testDetachedPoolProbeSpec = "tmux:gascity:soak-loop"

func TestSessionBeadAssigneeIdentities(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "empty bead produces no identities",
			bead: beads.Bead{},
			want: []string{},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "mc-xyz"},
			want: []string{"mc-xyz"},
		},
		{
			name: "session_name only",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "worker-mc-live"}},
			want: []string{"worker-mc-live"},
		},
		{
			name: "configured_named_identity only",
			bead: beads.Bead{Metadata: map[string]string{"configured_named_identity": "reviewer"}},
			want: []string{"reviewer"},
		},
		{
			name: "alias only",
			bead: beads.Bead{Metadata: map[string]string{"alias": "nux"}},
			want: []string{"nux"},
		},
		{
			name: "alias_history single entry",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "previous"}},
			want: []string{"previous"},
		},
		{
			name: "alias_history multiple entries",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "first,second,third"}},
			want: []string{"first", "second", "third"},
		},
		{
			name: "all fields populated",
			bead: beads.Bead{
				ID: "mc-xyz",
				Metadata: map[string]string{
					"session_name":              "worker-mc-live",
					"configured_named_identity": "reviewer",
					"alias":                     "rictus",
					"alias_history":             "nux",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "rictus", "nux"},
		},
		{
			name: "whitespace-only values are trimmed and skipped",
			bead: beads.Bead{
				ID: "  ",
				Metadata: map[string]string{
					"session_name":              "   ",
					"configured_named_identity": "\t",
					"alias":                     " ",
					"alias_history":             "  ,  , real ,  ",
				},
			},
			want: []string{"real"},
		},
		{
			name: "values with surrounding whitespace are trimmed",
			bead: beads.Bead{
				ID: "  mc-xyz  ",
				Metadata: map[string]string{
					"session_name":              "  worker-mc-live  ",
					"configured_named_identity": "  reviewer  ",
					"alias":                     "  nux  ",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "nux"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionBeadAssigneeIdentities(tt.bead)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d identities %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, id := range got {
				if id != tt.want[i] {
					t.Errorf("identity[%d] = %q, want %q (full got=%v, want=%v)", i, id, tt.want[i], got, tt.want)
				}
			}
		})
	}
}

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

	closed := gcSweepSessionBeadsFromBeads(store, sessionBeads)

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

	closed := gcSweepSessionBeadsFromBeads(store, sessionBeads)

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

	closed := gcSweepSessionBeadsFromBeads(store, sessionBeads)

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

	closed := gcSweepSessionBeadsFromBeads(store, sessionBeads)

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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
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

func TestReleaseOrphanedPoolAssignments_SkipsUnassignedWorkflowRoot(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:  "workflow root",
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "workflow",
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create workflow root: %v", err)
	}
	if err := store.Update(root.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set workflow root status: %v", err)
	}
	root, err = store.Get(root.ID)
	if err != nil {
		t.Fatalf("Reload workflow root: %v", err)
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{root},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want workflow root preserved", released)
	}

	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get workflow root: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensEphemeralPoolAssignee(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:     "orphaned pool wisp",
		Assignee:  "worker-dead",
		Ephemeral: true,
		Metadata:  map[string]string{"gc.routed_to": "worker"},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
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

func TestReleaseOrphanedPoolAssignments_ReopensLegacyWorkflowRunTarget(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "legacy orphaned workflow root",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
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

func TestIsRecoverableUnassignedInProgressPoolWorkUsesLegacyWorkflowRunTarget(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	work := beads.Bead{
		ID:     "legacy-unassigned-workflow-root",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
	}

	if !isRecoverableUnassignedInProgressPoolWork(cfg, work) {
		t.Fatalf("isRecoverableUnassignedInProgressPoolWork = false, want true for legacy workflow run_target")
	}
}

func TestReleaseOrphanedPoolAssignments_DetachedProbeAliveSkipsRelease(t *testing.T) {
	resetDetachedProbeErrorCountsForTest()
	store := beads.NewMemStore()
	work := createDetachedOrphanedPoolWork(t, store)
	installFakeTmux(t, "exit 0")
	var logs bytes.Buffer
	restore := captureLogOutput(&logs)
	defer restore()

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none while detached probe is alive", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-dead" {
		t.Fatalf("assignee = %q, want worker-dead", got.Assignee)
	}
	if got.Metadata[detachedProbeMetadataKey] != testDetachedPoolProbeSpec {
		t.Fatalf("gc.detached = %q, want preserved", got.Metadata[detachedProbeMetadataKey])
	}
	if !strings.Contains(logs.String(), "detached probe alive") {
		t.Fatalf("logs = %q, want detached probe alive diagnostic", logs.String())
	}
}

func TestReleaseOrphanedPoolAssignments_DetachedProbeDeadReleasesAndClears(t *testing.T) {
	resetDetachedProbeErrorCountsForTest()
	store := beads.NewMemStore()
	work := createDetachedOrphanedPoolWork(t, store)
	installFakeTmux(t, "exit 1")

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		nil,
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
	if got.Metadata[detachedProbeMetadataKey] != "" {
		t.Fatalf("gc.detached = %q, want cleared", got.Metadata[detachedProbeMetadataKey])
	}
}

func TestReleaseOrphanedPoolAssignments_DetachedProbeDeadPreservesGuardWhenReleaseFails(t *testing.T) {
	resetDetachedProbeErrorCountsForTest()
	base := beads.NewMemStore()
	work := createDetachedOrphanedPoolWork(t, base)
	store := failReleaseUpdateStore{Store: base, failID: work.ID}
	installFakeTmux(t, "exit 1")

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none when release update fails", released)
	}
	got, err := base.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-dead" {
		t.Fatalf("assignee = %q, want worker-dead", got.Assignee)
	}
	if got.Metadata[detachedProbeMetadataKey] != testDetachedPoolProbeSpec {
		t.Fatalf("gc.detached = %q, want preserved after failed release", got.Metadata[detachedProbeMetadataKey])
	}
}

func TestReleaseOrphanedPoolAssignments_DetachedProbeErrorsReleaseOnThirdTick(t *testing.T) {
	resetDetachedProbeErrorCountsForTest()
	store := beads.NewMemStore()
	work := createDetachedOrphanedPoolWork(t, store)
	installFakeTmux(t, "exit 2")

	for tick := 1; tick <= 2; tick++ {
		released := releaseOrphanedPoolAssignmentsFromBeads(
			store,
			testPoolReleaseConfig(),
			"",
			nil,
			[]beads.Bead{work},
			nil,
			nil,
			nil,
		)
		if len(released) != 0 {
			t.Fatalf("tick %d released = %v, want none before third error", tick, released)
		}
		got, err := store.Get(work.ID)
		if err != nil {
			t.Fatalf("tick %d Get work bead: %v", tick, err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("tick %d status = %q, want in_progress", tick, got.Status)
		}
		if _, ok := got.Metadata["error_count"]; ok {
			t.Fatalf("tick %d persisted error_count metadata: %+v", tick, got.Metadata)
		}
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		nil,
		nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("third tick released = %v, want [%s]", released, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Metadata[detachedProbeMetadataKey] != "" {
		t.Fatalf("gc.detached = %q, want cleared", got.Metadata[detachedProbeMetadataKey])
	}
	if _, ok := got.Metadata["error_count"]; ok {
		t.Fatalf("persisted error_count metadata after release: %+v", got.Metadata)
	}
}

func createDetachedOrphanedPoolWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to":           "worker",
			detachedProbeMetadataKey: testDetachedPoolProbeSpec,
		},
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
	return work
}

func testPoolReleaseConfig() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
}

type failReleaseUpdateStore struct {
	beads.Store
	failID string
}

func (s failReleaseUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID && opts.Status != nil && *opts.Status == "open" && opts.Assignee != nil && *opts.Assignee == "" {
		return errors.New("release update failed")
	}
	return s.Store.Update(id, opts)
}

func captureLogOutput(buf *bytes.Buffer) func() {
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}
}

type sessionListMissStore struct {
	beads.Store
	directSessions map[string]beads.Bead
}

func (s sessionListMissStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.directSessions[id]; ok {
		return b, nil
	}
	return s.Store.Get(id)
}

func (s sessionListMissStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && query.Label == sessionBeadLabel {
		return nil, nil
	}
	return s.Store.List(query)
}

func TestLiveSessionBeadExistsByIdentity_SkipsClosedSessionBead(t *testing.T) {
	// Regression: directSessionBeadIDCandidates can resolve to a session
	// bead that has since been closed. liveSessionBeadExistsByIdentity must
	// skip closed beads via the early continue so the caller falls through
	// to the live-list fallback instead of claiming the dead session is alive.
	base := beads.NewMemStore()
	closed, err := base.Create(beads.Bead{
		Title:  "closed session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	if err := base.Close(closed.ID); err != nil {
		t.Fatalf("Close session bead: %v", err)
	}
	closed, err = base.Get(closed.ID)
	if err != nil {
		t.Fatalf("Reload closed session: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("closed status = %q, want closed", closed.Status)
	}

	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"worker-mc-dead": closed},
	}

	if liveSessionBeadExistsByIdentity(store, "worker-mc-dead") {
		t.Error("liveSessionBeadExistsByIdentity = true, want false for closed session bead")
	}
}

func TestLiveSessionBeadExistsByIdentity_SkipsNonSessionBead(t *testing.T) {
	// Regression: directSessionBeadIDCandidates can resolve to a bead that
	// is not a session (e.g. a work bead whose ID collides with the
	// assignee string). liveSessionBeadExistsByIdentity must skip such
	// beads instead of treating them as live session owners.
	base := beads.NewMemStore()
	notSession, err := base.Create(beads.Bead{
		Title: "not a session",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("Create non-session bead: %v", err)
	}
	if notSession.Type == sessionBeadType {
		t.Fatalf("test setup: bead type = %q, want non-session", notSession.Type)
	}
	for _, label := range notSession.Labels {
		if label == sessionBeadLabel {
			t.Fatalf("test setup: bead has session label %q", label)
		}
	}

	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"worker-mc-task": notSession},
	}

	if liveSessionBeadExistsByIdentity(store, "worker-mc-task") {
		t.Error("liveSessionBeadExistsByIdentity = true, want false for non-session bead")
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionMissingFromSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: sessionBead.Metadata["session_name"],
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for live session", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != sessionBead.Metadata["session_name"] {
		t.Fatalf("assignee = %q, want %q", got.Assignee, sessionBead.Metadata["session_name"])
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionWhenLiveSessionListMissesIt(t *testing.T) {
	base := beads.NewMemStore()
	sessionBead, err := base.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := base.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: sessionBead.Metadata["session_name"],
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := base.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = base.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	store := sessionListMissStore{
		Store:          base,
		directSessions: map[string]beads.Bead{"mc-live": sessionBead},
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for directly resolvable live session", released)
	}

	got, err := base.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != sessionBead.Metadata["session_name"] {
		t.Fatalf("assignee = %q, want %q", got.Assignee, sessionBead.Metadata["session_name"])
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionAssignedByAlias(t *testing.T) {
	// Regression: polecat pool sessions carry their human-readable identity
	// in Metadata["alias"] (e.g. "nux"), separate from session_name
	// ("polecat-gc-vi6hhp"). Work claimed by a polecat is often assigned
	// under the alias, so orphan-release must recognize alias-owned work as
	// belonging to a live session.
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{sessionBead},
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live polecat owns work via alias", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "nux" {
		t.Fatalf("assignee = %q, want nux", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionAssignedByAliasHistory(t *testing.T) {
	// Regression: a polecat may have been rebranded (alias rotated) while
	// retaining ownership of work assigned under the prior alias. The
	// previous alias is preserved in Metadata["alias_history"], so
	// orphan-release must consult history before deciding the assignee is
	// dead.
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "rictus",
			"alias_history":        "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{sessionBead},
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live polecat owns work via prior alias", released)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsLiveSessionByAliasViaLiveList(t *testing.T) {
	// Even without an upstream session snapshot, the fallback live-list
	// path must recognize alias-owned work. This covers ticks where the
	// session snapshot is missing or stale (e.g. partial reads).
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "polecat-gc-vi6hhp",
			"alias":                "nux",
			"template":             "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "nux",
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — live-list fallback must resolve alias", released)
	}
}

func TestReleaseOrphanedPoolAssignments_SkipsWorkReassignedAfterCandidateSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "claimed pool work",
		Assignee: "worker-old",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	candidate, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload candidate work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Assignee: stringPtr("worker-new")}); err != nil {
		t.Fatalf("Reassign work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{candidate},
		[]beads.Store{store},
		nil,
		nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for reassigned work", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Assignee != "worker-new" {
		t.Fatalf("assignee = %q, want worker-new", got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_ReopensUnassignedInProgressPoolWork(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stranded pool work",
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
	if work.Assignee != "" {
		t.Fatalf("test setup assignee = %q, want empty", work.Assignee)
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		nil,
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

// TestCollectAndReleaseOrphanPoolStepBead_Issue2793 pins the end-to-end fix
// for issue #2793: a graph.v2 step bead that is status=open with a
// dead-session long-form assignee (e.g. "<rig>--<pool>__coder...") and
// gc.routed_to=<pool template> must flow from collectAssignedWorkBeads
// into releaseOrphanedPoolAssignments and have its assignee cleared.
// Before the fix, collect's two passes (in_progress / Ready by live
// assignee) both missed the bead, so release never saw it and pool
// demand stayed at 0 across reconcile ticks.
func TestCollectAndReleaseOrphanPoolStepBead_Issue2793(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "graph.v2 step bead orphaned by dead session",
		Type:     "task",
		Status:   "open",
		Assignee: "rig--pool__coder-gc-session-deadbeef",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create orphaned step bead: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("Block orphaned step bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	found, foundStores, foundStoreRefs, _, partial := collectAssignedWorkBeadsWithStores(cfg, store, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(found) != 1 || found[0].ID != work.ID {
		t.Fatalf("collect missed the orphaned step bead: got %#v, want [%s]", found, work.ID)
	}

	// Empty openSessionBeads — the assignee's session is dead.
	released := releaseOrphanedPoolAssignmentsFromBeads(store, cfg, "", nil, found, foundStores, foundStoreRefs, nil)
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
		t.Fatalf("assignee = %q, want empty after release", got.Assignee)
	}
}

func TestCollectAndReleaseOrphanWorkflowRunTargetBead(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "legacy workflow root orphaned by dead session",
		Type:     "task",
		Status:   "open",
		Assignee: "rig--pool__coder-gc-session-deadbeef",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create workflow run-target bead: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("Block workflow run-target bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	found, foundStores, foundStoreRefs, _, partial := collectAssignedWorkBeadsWithStores(cfg, store, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(found) != 1 || found[0].ID != work.ID {
		t.Fatalf("collect missed the workflow run-target bead: got %#v, want [%s]", found, work.ID)
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(store, cfg, "", nil, found, foundStores, foundStoreRefs, nil)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get workflow run-target bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty after release", got.Assignee)
	}
}

func TestCollectAndReleaseNonWorkflowRunTargetBeadStaysAssigned(t *testing.T) {
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create blocker bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "control retry bead",
		Type:     "task",
		Status:   "open",
		Assignee: "gascity--control-dispatcher",
		Metadata: map[string]string{
			"gc.kind":       "retry",
			"gc.run_target": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create non-workflow run-target bead: %v", err)
	}
	if err := store.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("Block non-workflow run-target bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	found, foundStores, foundStoreRefs, _, partial := collectAssignedWorkBeadsWithStores(cfg, store, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(found) != 0 {
		t.Fatalf("collectAssignedWorkBeadsWithStores returned %#v, want none for non-workflow gc.run_target", found)
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(store, cfg, "", nil, found, foundStores, foundStoreRefs, nil)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none for non-workflow gc.run_target", released)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get non-workflow run-target bead: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Assignee != "gascity--control-dispatcher" {
		t.Fatalf("assignee = %q, want original control dispatcher", got.Assignee)
	}
}

func TestCollectAssignedWorkBeadsIncludesUnassignedInProgressPoolWorkForRecovery(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "stranded pool work",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}

	found, stores, _, _, partial := collectAssignedWorkBeadsWithStores(
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		store,
		nil,
		nil,
		nil,
	)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	if len(found) != 1 || found[0].ID != work.ID {
		t.Fatalf("found = %#v, want stranded work %s", found, work.ID)
	}
	if len(stores) != 1 || stores[0] != store {
		t.Fatalf("stores = %#v, want owner store", stores)
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "rig", Prefix: "ga"}},
			Agents: []config.Agent{{Name: "worker", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{work},
		nil,
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		nil,
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{
			Rigs:   []config.Rig{{Name: "repo"}},
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{cityWork, rigWork},
		[]beads.Store{cityStore, rigStore},
		nil,
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

func TestReleaseOrphanedPoolAssignments_ClearsSessionAffinityOnRelease(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.continuation_group": "main",
			"gc.routed_to":          "worker",
			"gc.session_affinity":   "require",
		},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{
			Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
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
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	if got.Metadata["gc.session_affinity"] != "" {
		t.Fatalf("gc.session_affinity still present after release: %#v", got.Metadata)
	}
	if got.Metadata["gc.continuation_group"] != "" {
		t.Fatalf("gc.continuation_group still present after release: %#v", got.Metadata)
	}
	if got.Metadata["gc.routed_to"] != "worker" {
		t.Fatalf("gc.routed_to = %q, want worker", got.Metadata["gc.routed_to"])
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{nil},
		nil,
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		[]beads.Bead{session},
		[]beads.Bead{work},
		nil,
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

func TestReleaseOrphanedPoolAssignments_ReleasesRigWorkAssignedToUnreachableOpenSession(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session, err := cityStore.Create(beads.Bead{
		Title:  "city worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create city session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "misassigned rig pool work",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "repo/worker"},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{
			Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
			Agents: []config.Agent{
				{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
				{Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			},
		},
		cityPath,
		[]beads.Bead{session},
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	gotSession, err := cityStore.Get(session.ID)
	if err != nil {
		t.Fatalf("Get city session bead: %v", err)
	}
	if gotSession.Status != "open" || gotSession.Metadata["session_name"] != "worker-live" {
		t.Fatalf("city session changed: status=%q metadata=%#v", gotSession.Status, gotSession.Metadata)
	}
}

// A live, cross-store-eligible (city-scoped, Scope="city") open session
// legitimately owns rig-routed work whose bead lives in a rig store — city
// agents federate across every store (vp-kvp). The release path must recognize
// that ownership and NOT reopen the bead. This is the exact inverse of
// ReleasesRigWorkAssignedToUnreachableOpenSession, where the holder is
// rig-scoped and genuinely cannot reach the work; here the holder can. Reopening
// a live city holder's claim is the #3453 root cause: openSessionOwnsWork
// returns false on a store-ref mismatch, demand reappears, and a backup worker
// is minted on the same in_progress bead (duplicate token burn + double-write).
func TestReleaseOrphanedPoolAssignments_KeepsCrossStoreEligibleHolderRigWork(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session, err := cityStore.Create(beads.Bead{
		Title:  "city worker",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name":         "worker-live",
			"template":             "worker",
			"agent_name":           "worker",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create city session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title:    "rig pool work owned by a live city session",
		Assignee: "worker-live",
		Metadata: map[string]string{"gc.routed_to": "repo/worker"},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		&config.City{
			Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
			Agents: []config.Agent{
				// The session's "worker" template resolves to this city-scoped
				// agent (cross-store eligible). The rig agent below exists only so
				// the work's routed "repo/worker" template resolves and the bead
				// reaches the ownership gate instead of being skipped earlier.
				{Name: "worker", Scope: "city", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
				{Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			},
		},
		cityPath,
		[]beads.Bead{session},
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"}, // work store-ref "repo" != city agent ref "" — the mismatch the fix must federate over
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — a live city-scoped session owns rig-routed work across stores (vp-kvp)", released)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-live" {
		t.Fatalf("rig work = status %q assignee %q, want in_progress/worker-live (claim preserved)", got.Status, got.Assignee)
	}
}

func TestStoreForPoolAssignment_UsesConfiguredHyphenatedIDPrefix(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "pieces",
			Prefix: "Pieces-Annotator",
			Path:   t.TempDir(),
		}},
	}
	work := beads.Bead{
		ID:       "pieces-annotator-x8o",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}

	got := storeForPoolAssignment(cfg, cityStore, map[string]beads.Store{"pieces": rigStore}, work)
	if got != rigStore {
		t.Fatalf("storeForPoolAssignment() = %p, want rig store %p", got, rigStore)
	}
}

func TestReleaseOrphanedPoolAssignments_KeepsSameStoreScopedOpenSessionOwnership(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		cityPath,
		[]beads.Bead{session},
		[]beads.Bead{work},
		[]beads.Store{store},
		[]string{""},
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

	released := releaseOrphanedPoolAssignmentsFromBeads(store, cfg, "", nil, []beads.Bead{work}, nil, nil, nil)
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

	released := releaseOrphanedPoolAssignmentsFromBeads(store, cfg, "", nil, []beads.Bead{work}, nil, nil, nil)
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

func TestReleaseOrphanedPoolAssignments_ReleasesNamedIdentityForUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "misassigned named work",
		Assignee: "reviewer",
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

	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		cfg,
		cityPath,
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"},
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("rig work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
}

// A live, cross-store-eligible (city-scoped, Scope="city") NAMED session
// legitimately owns rig-routed work whose bead lives in a rig store (vp-kvp).
// assigneePreservesNamedSessionRoute must preserve that claim instead of letting
// the bead be released — the named-route analog of the pool-worker
// openSessionOwnsWork cross-store fix (#3453). Without it a backup worker is
// minted on the same in_progress bead. Contrast
// ReleasesNamedIdentityForUnreachableStore, where the named agent is rig-scoped
// and genuinely cannot reach the work, so release is still correct.
func TestReleaseOrphanedPoolAssignments_PreservesCrossStoreEligibleNamedIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title:    "rig work owned by a city-scoped named session",
		Assignee: "reviewer",
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

	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "repo", Path: t.TempDir()}},
		Agents: []config.Agent{{Name: "worker", Scope: "city", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{
			Name:     "reviewer",
			Template: "worker",
			Mode:     "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}

	released := releaseOrphanedPoolAssignmentsFromBeads(
		cityStore,
		cfg,
		cityPath,
		nil,
		[]beads.Bead{work},
		[]beads.Store{rigStore},
		[]string{"repo"}, // rig store-ref != city named agent ref "" — the mismatch the fix must federate over
		map[string]beads.Store{"repo": rigStore},
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — a live city-scoped named session owns rig-routed work across stores (vp-kvp)", released)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get rig work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "reviewer" {
		t.Fatalf("rig work = status %q assignee %q, want in_progress/reviewer (claim preserved)", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_PreservesNamedIdentityForSameStore(t *testing.T) {
	cityPath := t.TempDir()
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

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store,
		cfg,
		cityPath,
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		[]string{""},
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
	if got.Assignee != "reviewer" {
		t.Fatalf("assignee = %q, want reviewer", got.Assignee)
	}
}

// conditionalReleaseProbeStore wraps a MemStore for the orphan-release TOCTOU
// tests. It records the store writes the release path performs, can report the
// conditional release unsupported (forcing the recheck fallback), and can
// inject a concurrent re-claim at controlled points: right after the
// pre-release live-work gate (a claim landing between the staleness check and
// the release write) or right after the release write (a claim that survives
// the race and should be observable in the verify-after read).
type conditionalReleaseProbeStore struct {
	beads.Store
	t   *testing.T
	mem *beads.MemStore

	releaseUnsupported bool
	claimID            string
	claimAssignee      string
	claimAfterLiveGate bool
	claimAfterWrite    bool

	releaseCalls      []releaseProbeCall
	assignmentUpdates []beads.UpdateOpts
	liveWorkLists     int
}

type releaseProbeCall struct {
	id       string
	assignee string
}

func newConditionalReleaseProbeStore(t *testing.T) (*conditionalReleaseProbeStore, beads.Bead) {
	t.Helper()
	return newConditionalReleaseProbeStoreWithMetadata(t, nil)
}

// newConditionalReleaseProbeStoreWithMetadata builds the orphan-release probe
// store with extra metadata merged onto the default routed/affinity fixture, so
// a test can add routing vectors (e.g. gc.continuation_group) without
// duplicating the setup. A nil extra map reproduces the default fixture exactly.
func newConditionalReleaseProbeStoreWithMetadata(t *testing.T, extra map[string]string) (*conditionalReleaseProbeStore, beads.Bead) {
	t.Helper()
	mem := beads.NewMemStore()
	metadata := map[string]string{
		"gc.routed_to":        "worker",
		"gc.session_affinity": "require",
	}
	for k, v := range extra {
		metadata[k] = v
	}
	work, err := mem.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := mem.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	work, err = mem.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	return &conditionalReleaseProbeStore{
		Store:         mem,
		t:             t,
		mem:           mem,
		claimID:       work.ID,
		claimAssignee: "worker-live",
	}, work
}

func (s *conditionalReleaseProbeStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if s.releaseUnsupported {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	s.releaseCalls = append(s.releaseCalls, releaseProbeCall{id: id, assignee: expectedAssignee})
	return s.mem.ReleaseIfCurrent(id, expectedAssignee)
}

func (s *conditionalReleaseProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	out, err := s.Store.List(query)
	if query.Live && query.Status == "in_progress" && query.Label == "" {
		s.liveWorkLists++
		if s.claimAfterLiveGate && s.liveWorkLists == 1 {
			s.reclaim()
		}
	}
	return out, err
}

func (s *conditionalReleaseProbeStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Assignee != nil || opts.Status != nil {
		s.assignmentUpdates = append(s.assignmentUpdates, opts)
	}
	err := s.Store.Update(id, opts)
	if err == nil && s.claimAfterWrite && opts.Assignee != nil && *opts.Assignee == "" {
		s.claimAfterWrite = false
		s.reclaim()
	}
	return err
}

func (s *conditionalReleaseProbeStore) reclaim() {
	s.t.Helper()
	if err := s.mem.Update(s.claimID, beads.UpdateOpts{
		Assignee: stringPtr(s.claimAssignee),
		Status:   stringPtr("in_progress"),
	}); err != nil {
		s.t.Fatalf("injecting concurrent re-claim: %v", err)
	}
}

func releaseProbeAssignments(store *conditionalReleaseProbeStore, work beads.Bead) []releasedPoolAssignment {
	return releaseOrphanedPoolAssignments(
		store,
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{store},
		nil,
		nil,
	)
}

func TestReleaseOrphanedPoolAssignments_UsesConditionalReleaseWhenSupported(t *testing.T) {
	store, work := newConditionalReleaseProbeStore(t)

	released := releaseProbeAssignments(store, work)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}

	if len(store.releaseCalls) != 1 {
		t.Fatalf("ReleaseIfCurrent calls = %v, want exactly one", store.releaseCalls)
	}
	if call := store.releaseCalls[0]; call.id != work.ID || call.assignee != "worker-dead" {
		t.Fatalf("ReleaseIfCurrent call = %+v, want {%s worker-dead}", call, work.ID)
	}
	if len(store.assignmentUpdates) != 0 {
		t.Fatalf("assignment-shaped Update calls = %+v, want none when ReleaseIfCurrent is supported", store.assignmentUpdates)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	if got.Metadata["gc.session_affinity"] != "" {
		t.Fatalf("gc.session_affinity = %q, want cleared after conditional release", got.Metadata["gc.session_affinity"])
	}
}

func TestReleaseOrphanedPoolAssignments_ContinuationGroupBeadBypassesCASWindow(t *testing.T) {
	// A bead carrying the active continuation-group routing vector must NOT take
	// the two-write CAS release path: ReleaseIfCurrent swaps only status/assignee,
	// so a follow-up metadata clear would briefly expose an open, unassigned bead
	// whose gc.continuation_group is still set, letting a concurrent
	// `gc hook --claim` vacuum it (or its {root, group} siblings) onto a new
	// session via the stale group. The release must instead take the recheck
	// fallback, which clears status, assignee, and the group in a single Update —
	// so the group is never visible on a claimable bead. This is the regression
	// pin for the CAS release-then-clear ordering window.
	store, work := newConditionalReleaseProbeStoreWithMetadata(t, map[string]string{
		"gc.root_bead_id":       "root-1",
		"gc.continuation_group": "grp-1",
	})

	released := releaseProbeAssignments(store, work)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}
	if len(store.releaseCalls) != 0 {
		t.Fatalf("ReleaseIfCurrent calls = %v, want none: a continuation-group bead must bypass the CAS fast path", store.releaseCalls)
	}
	if len(store.assignmentUpdates) != 1 {
		t.Fatalf("assignment-shaped Update calls = %+v, want exactly one atomic release write", store.assignmentUpdates)
	}
	// The single release write must clear the assignment AND the continuation
	// group together, leaving no open/unassigned/group-still-set window.
	update := store.assignmentUpdates[0]
	if update.Assignee == nil || *update.Assignee != "" || update.Status == nil || *update.Status != "open" {
		t.Fatalf("release update = %+v, want assignee=\"\" and status=open", update)
	}
	if v, ok := update.Metadata[beadmeta.ContinuationGroupMetadataKey]; !ok || v != "" {
		t.Fatalf("release update metadata[%s] = %q (present=%v), want cleared in the same write", beadmeta.ContinuationGroupMetadataKey, v, ok)
	}

	final, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if final.Status != "open" || final.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", final.Status, final.Assignee)
	}
	if strings.TrimSpace(final.Metadata[beadmeta.ContinuationGroupMetadataKey]) != "" {
		t.Fatalf("gc.continuation_group = %q, want cleared after release", final.Metadata[beadmeta.ContinuationGroupMetadataKey])
	}
	if strings.TrimSpace(final.Metadata["gc.session_affinity"]) != "" {
		t.Fatalf("gc.session_affinity = %q, want cleared after release", final.Metadata["gc.session_affinity"])
	}
}

func TestReleaseOrphanedPoolAssignments_ConditionalReleaseLosesRaceNoClobber(t *testing.T) {
	store, work := newConditionalReleaseProbeStore(t)
	store.claimAfterLiveGate = true

	released := releaseProbeAssignments(store, work)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none when a concurrent claim wins the release race", released)
	}
	if len(store.assignmentUpdates) != 0 {
		t.Fatalf("assignment-shaped Update calls = %+v, want none after losing the conditional release", store.assignmentUpdates)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-live" {
		t.Fatalf("work = status %q assignee %q, want the concurrent claim preserved (in_progress/worker-live)", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_UnsupportedStoreRechecksBeforeWrite(t *testing.T) {
	store, work := newConditionalReleaseProbeStore(t)
	store.releaseUnsupported = true
	store.claimAfterLiveGate = true

	released := releaseProbeAssignments(store, work)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none when the assignee flips between check and write", released)
	}
	if len(store.assignmentUpdates) != 0 {
		t.Fatalf("assignment-shaped Update calls = %+v, want none after the recheck observes the re-claim", store.assignmentUpdates)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-live" {
		t.Fatalf("work = status %q assignee %q, want the concurrent claim preserved (in_progress/worker-live)", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_UnsupportedStoreLogsRacedClaimAfterRelease(t *testing.T) {
	store, work := newConditionalReleaseProbeStore(t)
	store.releaseUnsupported = true
	store.claimAfterWrite = true

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	released := releaseProbeAssignments(store, work)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}
	if !strings.Contains(buf.String(), "raced the orphan release") {
		t.Fatalf("log output = %q, want a loud raced-claim detection after the release write", buf.String())
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-live" {
		t.Fatalf("work = status %q assignee %q, want the surviving claim preserved (in_progress/worker-live)", got.Status, got.Assignee)
	}
}

func TestReleaseOrphanedPoolAssignments_UnsupportedStoreReleasesNormalOrphan(t *testing.T) {
	store, work := newConditionalReleaseProbeStore(t)
	store.releaseUnsupported = true

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	released := releaseProbeAssignments(store, work)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s]", released, work.ID)
	}
	if len(store.assignmentUpdates) != 1 {
		t.Fatalf("assignment-shaped Update calls = %+v, want exactly the release write", store.assignmentUpdates)
	}
	if strings.Contains(buf.String(), "raced the orphan release") {
		t.Fatalf("log output = %q, want no raced-claim detection for an uncontended release", buf.String())
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open/unassigned", got.Status, got.Assignee)
	}
	if got.Metadata["gc.session_affinity"] != "" {
		t.Fatalf("gc.session_affinity = %q, want cleared after fallback release", got.Metadata["gc.session_affinity"])
	}
}

// releaseOrphanedPoolAssignmentsFromBeads projects raw session beads to
// session.Info and calls releaseOrphanedPoolAssignments, letting the existing
// raw-bead fixtures exercise the WI-5 W4 typed signature.
func releaseOrphanedPoolAssignmentsFromBeads(
	store beads.Store,
	cfg *config.City,
	cityPath string,
	openSessionBeads []beads.Bead,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	rigStores map[string]beads.Store,
) []releasedPoolAssignment {
	var infos []session.Info
	for _, b := range openSessionBeads {
		infos = append(infos, seedSessionInfo(b))
	}
	return releaseOrphanedPoolAssignments(store, cfg, cityPath, infos, assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, rigStores)
}

// gcSweepSessionBeadsFromBeads projects raw session beads to session.Info and
// calls GCSweepSessionBeads, letting the raw-bead fixtures exercise the WI-5 W4
// typed signature.
func gcSweepSessionBeadsFromBeads(store beads.Store, sessionBeads []beads.Bead) []string {
	var infos []session.Info
	for _, b := range sessionBeads {
		infos = append(infos, seedSessionInfo(b))
	}
	return GCSweepSessionBeads(store, nil, infos)
}
