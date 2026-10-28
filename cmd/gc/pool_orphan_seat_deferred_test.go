package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// noConditionalWritesStore delegates every Store method but stands outside
// the conditional-write capability interfaces, so a fenced write resolves no
// way to land and is refused rather than written blind.
type noConditionalWritesStore struct{ beads.Store }

var errSeatHandoverPredecessorRead = fmt.Errorf("simulated session-store read failure")

// seatHandoverClaimedFixture seeds the AC-1 shape — a closed seat row holding
// in_progress work under its bead ID, with the seat's unique successor awake —
// and returns everything the subtests need, including the sweep's fresh work
// snapshot.
func seatHandoverClaimedFixture(t *testing.T) (store *beads.MemStore, predecessor, successor, work beads.Bead) {
	t.Helper()
	store = beads.NewMemStore()
	predecessor = seatRow(t, store, "predecessor", "worker-mc-old", "baker")
	successor = seatRow(t, store, "successor", "worker-mc-new", "baker")
	var err error
	work, err = store.Create(beads.Bead{
		Title:    "work claimed under the predecessor's bead ID",
		Type:     "task",
		Assignee: predecessor.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   predecessor.ID,
			beadmeta.SessionNameMetadataKey: "worker-mc-old",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	closed := "closed"
	if err := store.Update(predecessor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close predecessor: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	return store, predecessor, successor, work
}

// TestSeatHandover_SkipsWhenAssignmentMovedAfterSnapshot is AC-5's lost-fence
// arm: the work bead's assignee changes between the sweep's snapshot and the
// hand-over write, so the fenced reassign reads "moved" and the bead is
// neither reassigned nor released — it stays with whoever took it.
func TestSeatHandover_SkipsWhenAssignmentMovedAfterSnapshot(t *testing.T) {
	store, _, successor, snapshot := seatHandoverClaimedFixture(t)
	moved := "someone-else"
	if err := store.Update(snapshot.ID, beads.UpdateOpts{Assignee: &moved}); err != nil {
		t.Fatalf("move assignee after the snapshot: %v", err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
		sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{snapshot},
		nil, nil, nil, nil, nil, nil)

	if len(released) != 0 {
		t.Fatalf("released %v, want none: the bead moved after the snapshot, so nothing here may write it", released)
	}
	got, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Assignee != moved {
		t.Fatalf("work assignee = %q, want %q (neither reassigned to %s nor released)", got.Assignee, moved, successor.ID)
	}
	if got.Status != "in_progress" {
		t.Fatalf("work status = %q, want in_progress", got.Status)
	}
	if !strings.Contains(logBuf.String(), snapshot.ID) {
		t.Fatalf("log = %q, want one line naming the bead %s", logBuf.String(), snapshot.ID)
	}
}

// TestSeatHandover_DefersWhenStoreCannotWriteConditionally is AC-5's
// refused-write arm: the owner store offers no conditional write, so the
// reassign is refused (never written blind), the bead keeps the closed
// identity this pass, and the pass logs one line naming the bead.
func TestSeatHandover_DefersWhenStoreCannotWriteConditionally(t *testing.T) {
	store, _, successor, snapshot := seatHandoverClaimedFixture(t)
	wrapped := noConditionalWritesStore{Store: store}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, seatHandoverConfig(), "",
		sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{snapshot},
		[]beads.Store{wrapped}, nil, nil, nil, nil, nil)

	if len(released) != 0 {
		t.Fatalf("released %v, want none: a refused hand-over keeps the claim this pass", released)
	}
	got, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Assignee != snapshot.Assignee || got.Status != "in_progress" {
		t.Fatalf("work = assignee %q status %q, want unchanged %q/in_progress (no blind write)", got.Assignee, got.Status, snapshot.Assignee)
	}
	deferred := strings.Count(logBuf.String(), "deferred")
	if deferred != 1 || !strings.Contains(logBuf.String(), snapshot.ID) {
		t.Fatalf("log = %q, want exactly one deferred hand-over line naming %s", logBuf.String(), snapshot.ID)
	}
}

// TestSeatHandover_KeepsClaimWhenPredecessorReadFails is AC-5's read-error
// arm: the session-store read of the assignee's own row fails, so the
// predecessor's fate is unknown and the claim is kept this pass with one log
// line naming the identity — uncertainty is not permission to reopen
// (gc-ft31x). The base sweep, which never reads the row, releases the bead.
func TestSeatHandover_KeepsClaimWhenPredecessorReadFails(t *testing.T) {
	store, predecessor, successor, snapshot := seatHandoverClaimedFixture(t)
	failing := &failingGetStore{Store: store, failID: predecessor.ID, err: errSeatHandoverPredecessorRead}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: failing}, seatHandoverConfig(), "",
		sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{snapshot},
		nil, nil, nil, nil, nil, nil)

	if len(released) != 0 {
		t.Fatalf("released %v, want none: the predecessor read failed, so the claim stays this pass", released)
	}
	got, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Assignee != predecessor.ID || got.Status != "in_progress" {
		t.Fatalf("work = assignee %q status %q, want unchanged %q/in_progress", got.Assignee, got.Status, predecessor.ID)
	}
	kept := strings.Count(logBuf.String(), "stay assigned this pass")
	if kept != 1 || !strings.Contains(logBuf.String(), predecessor.ID) {
		t.Fatalf("log = %q, want exactly one kept-claim line naming %s", logBuf.String(), predecessor.ID)
	}
}
