package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// liveGetWorkStore wraps the recording write store with a controllable Get so
// tests can model the live backing view diverging from the (possibly stale)
// enumeration snapshot the caller passed to ReleaseWorkBead.
type liveGetWorkStore struct {
	*recordingWriteWorkStore
	live map[string]beads.Bead
}

func (s *liveGetWorkStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.live[id]; ok {
		return b, nil
	}
	return beads.Bead{}, beads.ErrNotFound
}

// TestWorkAssignmentReleaseWorkBead_SkipsFreshlyClosedBead is the regression
// test for the claim/reopen stall loop: the caller's enumeration answered from
// the CachingStore's stale view and handed release a bead that read
// in_progress, while the live store already had it CLOSED (the worker closed
// it honestly moments before its session was torn down). Release must emit NO
// write at all — resetting it to open would destroy the completed close and
// re-queue finished work forever.
func TestWorkAssignmentReleaseWorkBead_SkipsFreshlyClosedBead(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	store := &liveGetWorkStore{recordingWriteWorkStore: rec, live: map[string]beads.Bead{
		"w-done": {ID: "w-done", Status: "closed", Assignee: "keeper-s1"},
	}}
	wa := workAssignmentForStore(beads.WorkStore{Store: store})

	stale := beads.Bead{ID: "w-done", Status: "in_progress", Assignee: "keeper-s1"}
	if err := wa.ReleaseWorkBead(stale, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 0 {
		t.Fatalf("expected NO Update on a live-closed bead, got %#v", rec.updates)
	}
	if len(rec.metaSets) != 0 {
		t.Fatalf("expected NO SetMetadata on a live-closed bead, got %#v", rec.metaSets)
	}
}

// TestWorkAssignmentReleaseWorkBead_LiveStatusGovernsReset asserts the
// in_progress→open decision keys off the live view, not the stale snapshot:
// a bead the live store reads as open (worker already released it back) gets
// the assignee/affinity clear but NO redundant status write, even when the
// snapshot still said in_progress.
func TestWorkAssignmentReleaseWorkBead_LiveStatusGovernsReset(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	store := &liveGetWorkStore{recordingWriteWorkStore: rec, live: map[string]beads.Bead{
		"w-live-open": {ID: "w-live-open", Status: "open", Assignee: "keeper-s1"},
	}}
	wa := workAssignmentForStore(beads.WorkStore{Store: store})

	stale := beads.Bead{ID: "w-live-open", Status: "in_progress", Assignee: "keeper-s1"}
	if err := wa.ReleaseWorkBead(stale, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	if got.opts.Status != nil {
		t.Fatalf("Status should be nil when the live view is already open, got %q", *got.opts.Status)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
}

// TestWorkAssignmentReleaseWorkBead_LiveReadFailureFallsBack asserts a failed
// live read releases from the snapshot unchanged (the pre-fix behavior):
// skipping on error would strand a genuinely stuck bead assigned to a dead
// session, which is the exact condition release exists to repair.
func TestWorkAssignmentReleaseWorkBead_LiveReadFailureFallsBack(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	store := &liveGetWorkStore{recordingWriteWorkStore: rec, live: map[string]beads.Bead{}}
	wa := workAssignmentForStore(beads.WorkStore{Store: store})

	stale := beads.Bead{ID: "w-stuck", Status: "in_progress", Assignee: "keeper-s1"}
	if err := wa.ReleaseWorkBead(stale, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	if got.opts.Status == nil || *got.opts.Status != "open" {
		t.Fatalf("Status = %v, want open (snapshot fallback)", got.opts.Status)
	}
}
