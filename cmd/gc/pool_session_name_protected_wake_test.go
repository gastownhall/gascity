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
		map[storeScopedBeadKey]struct{}{{ID: work.ID}: {}},
		nil,
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
		map[storeScopedBeadKey]struct{}{{ID: work.ID}: {}},
		nil,
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

// Protection is store-scoped: a wake candidate in one store must not shield a
// genuinely orphaned bead that happens to share its ID in another store.
// AssignedWorkBeads carries colliding IDs across independent city and rig
// stores by design (storeScopedBeadKey), so keying the protected set on the
// bead ID alone strands the same-ID row in every other store forever — the
// retain check sits ahead of the liveness probes, so nothing downstream can
// undo it.
func TestReleaseOrphanedPoolAssignments_ProtectionDoesNotCrossStoreIDCollision(t *testing.T) {
	inProgress := "in_progress"
	seed := func(store beads.Store, title, assignee string) beads.Bead {
		t.Helper()
		wb, err := store.Create(beads.Bead{
			Title:    title,
			Assignee: assignee,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if err := store.Update(wb.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("set %s in_progress: %v", title, err)
		}
		wb, err = store.Get(wb.ID)
		if err != nil {
			t.Fatalf("re-read %s: %v", title, err)
		}
		return wb
	}

	// Two independent stores each mint their own "gc-1": storeA holds the wake
	// candidate under ref "", storeB holds a genuinely orphaned bead of the
	// same ID under ref "rig:b".
	storeA := beads.NewMemStore()
	storeB := beads.NewMemStore()
	aWork := seed(storeA, "storeA wake candidate", "worker-a-waking")
	bWork := seed(storeB, "storeB orphan sharing storeA's bead ID", "worker-mc-gone")
	if aWork.ID != bWork.ID {
		t.Fatalf("fixture needs colliding IDs across stores, got %q and %q", aWork.ID, bWork.ID)
	}

	released := releaseOrphanedPoolAssignments(
		storeA, beads.SessionStore{Store: storeA}, testPoolReleaseConfig(), "", nil,
		[]beads.Bead{aWork, bWork}, []beads.Store{storeA, storeB}, []string{"", "rig:b"}, nil,
		protectedWakeWorkKeys([]beads.Bead{aWork}, []string{""}),
		nil,
	)
	if len(released) != 1 || released[0].ID != bWork.ID {
		t.Fatalf("released = %v, want exactly storeB's %s — storeA's wake candidate shielded a same-ID bead in another store", released, bWork.ID)
	}

	gotB, err := storeB.Get(bWork.ID)
	if err != nil {
		t.Fatalf("re-read storeB work: %v", err)
	}
	if gotB.Status != "open" || gotB.Assignee != "" {
		t.Fatalf("storeB orphan not reopened: status=%q assignee=%q", gotB.Status, gotB.Assignee)
	}

	gotA, err := storeA.Get(aWork.ID)
	if err != nil {
		t.Fatalf("re-read storeA work: %v", err)
	}
	if gotA.Status != inProgress || gotA.Assignee != "worker-a-waking" {
		t.Fatalf("storeA wake candidate lost its protection: status=%q assignee=%q", gotA.Status, gotA.Assignee)
	}
}
