package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// seedOwnerStoreReleaseFixture builds the two-store shape these tests share: a
// pool session bead carrying alias "nux" plus the in_progress work bead claimed
// under that alias, both in sessionStore/workStore as the caller directs.
//
// openSessionInfos is deliberately nil at every call site. That models the arm
// where the store-ref index misses — the work is recorded under one store-ref
// while the owning session is indexed under another, so openSessionOwnsWork
// returns false and neither fail-open sentinel fires. The liveness re-read is
// then the only thing standing between a live worker and having its claim
// yanked.
func seedOwnerStoreReleaseFixture(t *testing.T, sessionStore, workStore beads.Store, sessionStatus string) beads.Bead {
	t.Helper()

	sess, err := sessionStore.Create(beads.Bead{
		Title:    "pool session",
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "worker-1", "alias": "nux"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if sessionStatus != "open" {
		if err := sessionStore.Update(sess.ID, beads.UpdateOpts{Status: &sessionStatus}); err != nil {
			t.Fatalf("set session bead status %q: %v", sessionStatus, err)
		}
	}

	work, err := workStore.Create(beads.Bead{
		Title:    "work claimed by the live pool session",
		Type:     "task",
		Status:   "open",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := workStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}
	return work
}

// TestReleaseOrphanedPoolAssignmentsReadsLivenessFromWorkOwnerStore is the
// repro. The orphan-release liveness probe read only the primary store, but a
// city that relocates a class keeps session beads elsewhere: graph-resident run
// sessions (gcg-session-*) are written into the same store as the work they
// drive, and the primary store then serves zero of them.
//
// liveOpenSessionAssignmentExists issues its gc:session label query against a
// store that cannot answer, gets an empty result with no error, and reads
// empty-success as "this assignee is dead" — only an actual query error fails
// closed. The controller then reopens live work under a live worker, and the
// session drains as orphaned moments later.
//
// The work bead's own owner store is exactly where such a session bead lives,
// so probing it after the primary probe misses closes the gap without
// enumerating every attached store.
func TestReleaseOrphanedPoolAssignmentsReadsLivenessFromWorkOwnerStore(t *testing.T) {
	primaryStore := beads.NewMemStore() // serves no relocated session beads
	ownerStore := beads.NewMemStore()   // holds the session bead AND its work
	work := seedOwnerStoreReleaseFixture(t, ownerStore, ownerStore, "open")

	released := releaseOrphanedPoolAssignments(
		primaryStore,
		beads.SessionStore{Store: primaryStore},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{ownerStore},
		nil,
		nil,
		nil,
	)

	if len(released) != 0 {
		t.Fatalf("released %v, want none — alias \"nux\" belongs to an open session bead co-resident with the "+
			"work in its owner store, so its holder is alive. Releasing it means liveness was read only from the "+
			"primary store, which serves no session beads here.", released)
	}
	got, err := ownerStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "nux" {
		t.Fatalf("live worker's claim was dropped: status=%q assignee=%q, want in_progress/nux", got.Status, got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignmentsReleasesWhenNoStoreHoldsTheSession is the
// control: with no session bead in EITHER store the claim is genuinely
// orphaned and must still be recovered, so the extra probe cannot have traded
// a false release for claims stuck forever on sessions that really are gone.
func TestReleaseOrphanedPoolAssignmentsReleasesWhenNoStoreHoldsTheSession(t *testing.T) {
	primaryStore := beads.NewMemStore()
	ownerStore := beads.NewMemStore()
	work, err := ownerStore.Create(beads.Bead{
		Title:    "work whose holder is gone",
		Type:     "task",
		Status:   "open",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := ownerStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	if work, err = ownerStore.Get(work.ID); err != nil {
		t.Fatalf("reload work bead: %v", err)
	}

	released := releaseOrphanedPoolAssignments(
		primaryStore,
		beads.SessionStore{Store: primaryStore},
		testPoolReleaseConfig(),
		"",
		nil,
		[]beads.Bead{work},
		[]beads.Store{ownerStore},
		nil,
		nil,
		nil,
	)

	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] — no store holds a session bead naming alias \"nux\"", released, work.ID)
	}
}
