package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReassignReleaseClearsAParkCommittedAfterTheRead is round 4 A3 (codex r3
// finding 3): the release writes all eight park and counter keys empty on
// every --reassign, not only the ones its read saw, so a park that commits
// between the read and the release is cleared too.
func TestReassignReleaseClearsAParkCommittedAfterTheRead(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "task", Type: "task", Status: "in_progress", Assignee: "helper-1", Metadata: map[string]string{
		beadmeta.StartFailuresMetadataKey: "4",
		"gc.keep":                         "yes",
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := bead // the release's read: count 4, no park
	// The fifth failure parks W between that read and the release.
	if err := store.Update(bead.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.ParkedAtMetadataKey:          "2026-09-14T12:00:00Z",
		beadmeta.ParkReasonMetadataKey:        "fatal: two branches match",
		beadmeta.ParkFailuresMetadataKey:      "5",
		beadmeta.StartFailuresMetadataKey:     "",
		beadmeta.StartBackoffUntilMetadataKey: "",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reopenForReassignInStore(store, bead.ID, snapshot); err != nil {
		t.Fatalf("reopenForReassignInStore: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range ParkReleaseMetadataKeys {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q after a --reassign whose read predates the park, want cleared", key, v)
		}
	}
	if got.Assignee != "" || got.Status != "open" || got.Metadata["gc.keep"] != "yes" {
		t.Errorf("release changed the wrong fields: assignee=%q status=%q metadata=%v", got.Assignee, got.Status, got.Metadata)
	}
}

// TestBatchReassignRefusedCrossStoreReleasesNothing is round 4 A4 (the
// mayor's codex gate r1 on #19): a convoy --reassign to a target that cannot
// reach the children's store is refused with CrossStoreRouteError and releases
// nothing: every child keeps its assignee, its status, its park and its count.
func TestBatchReassignRefusedCrossStoreReleasesNothing(t *testing.T) {
	deps, router, rigTarget := crossStoreSlingDeps(t) // deps.StoreRef = "city:test-city"; rigTarget is rig-scoped
	store := deps.Store
	store.(*beads.MemStore).HonorExplicitIDs = true
	// Ids carry the rig's prefix so the batch-level cross-rig prefix guard
	// passes and each child reaches the per-child route-store reachability
	// check (the store ref is the city's, the target is rig-scoped).
	convoy, err := store.Create(beads.Bead{ID: "RW-convoy", Title: "convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	parked := map[string]string{
		beadmeta.ParkedAtMetadataKey:          "2026-09-14T12:00:00Z",
		beadmeta.ParkReasonMetadataKey:        "fatal: two branches match",
		beadmeta.ParkFailuresMetadataKey:      "5",
		beadmeta.StartFailuresMetadataKey:     "2",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-14T12:05:00Z",
	}
	var children []beads.Bead
	for _, title := range []string{"child-a", "child-b"} {
		meta := map[string]string{"gc.routed_to": "old-pool"}
		for k, v := range parked {
			meta[k] = v
		}
		// Created open (the batch routes only open children) and still held by
		// the old pool's session: the reassign would release that hold.
		child, err := store.Create(beads.Bead{ID: "RW-" + title, Title: title, Type: "task", Status: "open", Assignee: "old-pool-1", ParentID: convoy.ID, Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ExpandConvoy(context.Background(), convoy.ID, rigTarget, RouteOpts{Reassign: true}, store)
	if err == nil {
		t.Fatal("a convoy --reassign to an unreachable store target must be refused")
	}
	requireCrossStoreRouteError(t, err)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
	}
	for _, child := range children {
		got, err := store.Get(child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee != "old-pool-1" || got.Status != "open" {
			t.Errorf("%s: assignee=%q status=%q after a refused batch, want old-pool-1 / open (the child was released before the route check)", child.Title, got.Assignee, got.Status)
		}
		for key, want := range parked {
			if got.Metadata[key] != want {
				t.Errorf("%s: %s = %q after a refused batch, want %q intact", child.Title, key, got.Metadata[key], want)
			}
		}
	}
}
