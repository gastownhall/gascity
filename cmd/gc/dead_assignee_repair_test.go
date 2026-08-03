package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// strandedRepairFixture creates a session bead plus an in_progress work bead
// assigned to it (by session ID), the stranded-pool-worker shape: the runtime
// is gone but the bead still holds the session as assignee.
func strandedRepairFixture(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Metadata: map[string]string{"session_name": "worker-mc-dead", "pool_managed": "true"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "stranded work",
		Type:     "task",
		Assignee: session.ID,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	work, _ = store.Get(work.ID)
	return store, session, work
}

// A confirmed-stranded pool worker (marker aged past the confirmation window)
// has its in_progress work unassigned + reopened and its session bead closed.
func TestRepairStrandedPoolWorkerBead_ReopensAfterConfirmationWindow(t *testing.T) {
	store, session, work := strandedRepairFixture(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// Diagnostic first observed the strand well past the confirmation window.
	session.Metadata[strandedEventEmittedKey] = now.Add(-strandedRepairConfirmGrace - time.Minute).Format(time.RFC3339)

	var stderr bytes.Buffer
	repaired := repairStrandedPoolWorkerBead(store, nil, seedSessionInfo(session), "worker", &clock.Fake{Time: now}, &stderr)
	if !repaired {
		t.Fatalf("expected repair to close the session bead; stderr=%q", stderr.String())
	}

	gotWork, _ := store.Get(work.ID)
	if gotWork.Status != "open" {
		t.Fatalf("work status = %q, want open", gotWork.Status)
	}
	if gotWork.Assignee != "" {
		t.Fatalf("work assignee = %q, want empty", gotWork.Assignee)
	}
	gotSession, _ := store.Get(session.ID)
	if gotSession.Status != "closed" {
		t.Fatalf("session status = %q, want closed", gotSession.Status)
	}
}

// A single not-alive observation must never trigger the destructive clear: the
// marker is fresh (inside the window), so the work and session stay untouched.
func TestRepairStrandedPoolWorkerBead_DefersInsideConfirmationWindow(t *testing.T) {
	store, session, work := strandedRepairFixture(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	session.Metadata[strandedEventEmittedKey] = now.Format(time.RFC3339) // just observed

	var stderr bytes.Buffer
	if repairStrandedPoolWorkerBead(store, nil, seedSessionInfo(session), "worker", &clock.Fake{Time: now}, &stderr) {
		t.Fatalf("must not repair inside the confirmation window")
	}
	gotWork, _ := store.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
		t.Fatalf("work should be untouched, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	gotSession, _ := store.Get(session.ID)
	if gotSession.Status != "open" {
		t.Fatalf("session should stay open, got %q", gotSession.Status)
	}
}

// updateFailStore lists work normally but fails every Update, modeling a store
// where the unassign (ReleaseWorkBead → Update) cannot land.
type updateFailStore struct {
	beads.Store
}

func (s updateFailStore) Update(string, beads.UpdateOpts) error {
	return fmt.Errorf("simulated update failure")
}

// A partial failure (unassign does not land) must NOT be reported as a repair:
// the session bead stays open and the work stays claimed, so the stale-assignee
// item is left for the next-tick sweep rather than masked behind a "repaired"
// close. Surfaces the failure on stderr for distinct observability.
func TestRepairStrandedPoolWorkerBead_DefersAndKeepsSessionOpenWhenUnassignFails(t *testing.T) {
	base, session, work := strandedRepairFixture(t)
	store := updateFailStore{Store: base}
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// Marker aged well past the confirmation window — the window is satisfied;
	// only the failed unassign should hold the repair back.
	session.Metadata[strandedEventEmittedKey] = now.Add(-strandedRepairConfirmGrace - time.Minute).Format(time.RFC3339)

	var stderr bytes.Buffer
	if repairStrandedPoolWorkerBead(store, nil, seedSessionInfo(session), "worker", &clock.Fake{Time: now}, &stderr) {
		t.Fatal("repair must return false when an unassign does not land")
	}
	gotWork, _ := base.Get(work.ID)
	if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
		t.Fatalf("work must stay claimed after a failed unassign, got status=%q assignee=%q", gotWork.Status, gotWork.Assignee)
	}
	gotSession, _ := base.Get(session.ID)
	if gotSession.Status != "open" {
		t.Fatalf("session must stay open after a failed unassign, got %q", gotSession.Status)
	}
	if !strings.Contains(stderr.String(), "unassign(s) failed") {
		t.Fatalf("stderr must surface the failed unassign, got %q", stderr.String())
	}
}

// Without a stranded marker the leak has not been confirmed this generation, so
// the repair defers even if the caller reached it — the diagnostic gates the
// destructive clear.
func TestRepairStrandedPoolWorkerBead_DefersWithoutStrandedMarker(t *testing.T) {
	store, session, work := strandedRepairFixture(t)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	var stderr bytes.Buffer
	if repairStrandedPoolWorkerBead(store, nil, seedSessionInfo(session), "worker", &clock.Fake{Time: now}, &stderr) {
		t.Fatalf("must not repair without a stranded marker")
	}
	gotWork, _ := store.Get(work.ID)
	if gotWork.Status != "in_progress" {
		t.Fatalf("work should be untouched, got status=%q", gotWork.Status)
	}
}

// A live named session's assigned work must survive the reopen sweep: an open
// session bead owning the identity means the session is not gone. Guards the
// conservative liveness primitive (open session bead exists → skip).
func TestReleaseOrphanedPoolAssignments_SkipsLiveAssigneeStaysAssigned(t *testing.T) {
	store := beads.NewMemStore()
	live, err := store.Create(beads.Bead{
		Title:    "live worker",
		Type:     sessionBeadType,
		Status:   "open",
		Metadata: map[string]string{"session_name": "worker-mc-live"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "routed work",
		Assignee: live.Metadata["session_name"],
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	work, _ = store.Get(work.ID)

	released := releaseOrphanedPoolAssignments(
		store,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		"",
		sessionInfosFromBeads([]beads.Bead{live}),
		[]beads.Bead{work},
		nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("live assignee must not be released, got %v", released)
	}
	got, _ := store.Get(work.ID)
	if got.Assignee == "" {
		t.Fatalf("live assignee cleared — should stay assigned")
	}
}

// emitDeadAssigneeReopenedEvents records one typed event per reopened bead,
// carrying the dead assignee and route read off the pre-filter snapshot.
func TestEmitDeadAssigneeReopenedEvents_EmitsTypedPayload(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "w-1", Assignee: "worker-mc-dead", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}},
		{ID: "w-2"}, // not released
	}
	released := []releasedPoolAssignment{{ID: "w-1", Index: 0}}
	rec := &capturingRecorder{}

	emitDeadAssigneeReopenedEvents(rec, assigned, released, time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	if len(rec.events) != 1 {
		t.Fatalf("event count = %d, want 1", len(rec.events))
	}
	e := rec.events[0]
	if e.Type != events.BeadDeadAssigneeReopened {
		t.Fatalf("type = %q, want %q", e.Type, events.BeadDeadAssigneeReopened)
	}
	if e.Subject != "w-1" {
		t.Fatalf("subject = %q, want w-1", e.Subject)
	}
	var p api.BeadDeadAssigneeReopenedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.BeadID != "w-1" || p.DeadAssignee != "worker-mc-dead" || p.RoutedTo != "worker" {
		t.Fatalf("payload = %+v, want bead_id=w-1 dead_assignee=worker-mc-dead routed_to=worker", p)
	}
}

// A nil recorder or empty release list is a no-op — no panic, no events.
func TestEmitDeadAssigneeReopenedEvents_NoOpOnEmpty(t *testing.T) {
	emitDeadAssigneeReopenedEvents(nil, nil, []releasedPoolAssignment{{ID: "x"}}, time.Now())
	rec := &capturingRecorder{}
	emitDeadAssigneeReopenedEvents(rec, nil, nil, time.Now())
	if len(rec.events) != 0 {
		t.Fatalf("expected no events, got %d", len(rec.events))
	}
}
