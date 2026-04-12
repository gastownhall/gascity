package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestSessionHasActiveBeads(t *testing.T) {
	session := beads.Bead{
		ID: "sess-1",
		Metadata: map[string]string{
			"session_name":              "rig/mayor",
			"configured_named_identity": "rig/mayor-alias",
		},
	}

	t.Run("no beads", func(t *testing.T) {
		if sessionHasActiveBeads(session, nil) {
			t.Error("expected false with no beads")
		}
	})

	t.Run("in_progress bead assigned by session name", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "in_progress", Assignee: "rig/mayor"},
		}
		if !sessionHasActiveBeads(session, wbs) {
			t.Error("expected true for in_progress bead assigned by session name")
		}
	})

	t.Run("in_progress bead assigned by bead ID", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "in_progress", Assignee: "sess-1"},
		}
		if !sessionHasActiveBeads(session, wbs) {
			t.Error("expected true for in_progress bead assigned by bead ID")
		}
	})

	t.Run("in_progress bead assigned by named identity", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "in_progress", Assignee: "rig/mayor-alias"},
		}
		if !sessionHasActiveBeads(session, wbs) {
			t.Error("expected true for in_progress bead assigned by named identity")
		}
	})

	t.Run("open bead not counted", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "open", Assignee: "rig/mayor"},
		}
		if sessionHasActiveBeads(session, wbs) {
			t.Error("expected false for open (not in_progress) bead")
		}
	})

	t.Run("in_progress bead assigned to different session", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "in_progress", Assignee: "rig/polecat"},
		}
		if sessionHasActiveBeads(session, wbs) {
			t.Error("expected false for bead assigned to different session")
		}
	})

	t.Run("empty assignee ignored", func(t *testing.T) {
		wbs := []beads.Bead{
			{ID: "wb-1", Status: "in_progress", Assignee: ""},
		}
		if sessionHasActiveBeads(session, wbs) {
			t.Error("expected false for empty assignee")
		}
	})
}

func TestDrainTrackerLifecycleDeferral(t *testing.T) {
	dt := newDrainTracker()
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	t.Run("no deferral returns zero time", func(t *testing.T) {
		if got := dt.lifecycleDeferredSince("sess-1"); !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("first deferral records timestamp", func(t *testing.T) {
		dt.deferLifecycle("sess-1", now)
		if got := dt.lifecycleDeferredSince("sess-1"); got != now {
			t.Errorf("expected %v, got %v", now, got)
		}
	})

	t.Run("subsequent deferral preserves original timestamp", func(t *testing.T) {
		later := now.Add(30 * time.Second)
		dt.deferLifecycle("sess-1", later)
		if got := dt.lifecycleDeferredSince("sess-1"); got != now {
			t.Errorf("expected %v (original), got %v", now, got)
		}
	})

	t.Run("clear removes deferral", func(t *testing.T) {
		dt.clearLifecycleDeferral("sess-1")
		if got := dt.lifecycleDeferredSince("sess-1"); !got.IsZero() {
			t.Errorf("expected zero time after clear, got %v", got)
		}
	})

	t.Run("independent sessions", func(t *testing.T) {
		dt.deferLifecycle("sess-a", now)
		dt.deferLifecycle("sess-b", now.Add(time.Minute))
		if got := dt.lifecycleDeferredSince("sess-a"); got != now {
			t.Errorf("sess-a: expected %v, got %v", now, got)
		}
		if got := dt.lifecycleDeferredSince("sess-b"); got != now.Add(time.Minute) {
			t.Errorf("sess-b: expected %v, got %v", now.Add(time.Minute), got)
		}
	})
}
