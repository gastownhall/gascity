package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// seedProtectedWakeWork creates an in_progress pool-routed work bead whose
// assignee has NO open session bead — the exact snapshot-staleness shape in
// which the release arm historically reopened work the same tick's wake arm
// was about to serve (release runs first over the shared pre-tick snapshot;
// the reopened work is then filtered out of wake demand, the reconciler
// retires the now-workless slot, and the reopened work re-creates demand next
// tick — the wake/release/retire treadmill).
func seedProtectedWakeWork(t *testing.T) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "routed work",
		Assignee: "worker-mc-gone",
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
	return store, work
}

// A work bead in the protected wake set must be RETAINED even though its
// assignee has no open session bead: the wake arm of this very tick is about
// to act on it, and uncertainty about session materialization is not
// permission to reopen work.
func TestReleaseOrphanedPoolAssignments_RetainsProtectedWakeWork(t *testing.T) {
	store, work := seedProtectedWakeWork(t)

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, testPoolReleaseConfig(), "", nil,
		[]beads.Bead{work}, []beads.Store{store}, nil, nil,
		map[string]struct{}{work.ID: {}},
	)
	if len(released) != 0 {
		t.Fatalf("released %v — protected wake work was reopened; the release arm ran ahead of the wake arm it must yield to", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-mc-gone" {
		t.Fatalf("work mutated despite protection: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// The empty protected set preserves existing behavior exactly: the same bead
// with no protection is released (the genuine dead-assignee recovery this
// sweep exists for must keep working).
func TestReleaseOrphanedPoolAssignments_EmptyProtectedSetStillReleases(t *testing.T) {
	store, work := seedProtectedWakeWork(t)

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, testPoolReleaseConfig(), "", nil,
		[]beads.Bead{work}, []beads.Store{store}, nil, nil,
		nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want exactly the dead-assignee bead %s", released, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("unprotected dead-assignee work not reopened: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// Protection is keyed on the work bead's ID alone, independent of identity
// spelling or store residency: an alias-assigned bead in a rig owner store is
// retained the same way (the guard must not depend on the alias/store-ref
// liveness fixes that already exist — it protects precisely the case they
// cannot see).
func TestReleaseOrphanedPoolAssignments_ProtectsAliasAssignedRigWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	ownerStore := beads.NewMemStore()
	work, err := ownerStore.Create(beads.Bead{
		Title:    "rig routed work",
		Assignee: "r-dog/gastown.furiosa",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := ownerStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	work, _ = ownerStore.Get(work.ID)

	released := releaseOrphanedPoolAssignments(
		cityStore, beads.SessionStore{Store: cityStore}, testPoolReleaseConfig(), "", nil,
		[]beads.Bead{work}, []beads.Store{ownerStore}, nil, nil,
		map[string]struct{}{work.ID: {}},
	)
	if len(released) != 0 {
		t.Fatalf("released %v — alias-assigned rig-store work was reopened despite wake protection", released)
	}
	got, err := ownerStore.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "r-dog/gastown.furiosa" {
		t.Fatalf("work mutated despite protection: status=%q assignee=%q", got.Status, got.Assignee)
	}
}
