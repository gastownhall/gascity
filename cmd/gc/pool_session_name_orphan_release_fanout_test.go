package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// countingLiveSessionListStore counts the live gc:session listings issued
// against it. That query — beads.ListQuery{Label: sessionBeadLabel, Live: true}
// in liveOpenSessionAssignmentExists (cmd/gc/pool_session_name.go) — carries no
// assignee filter: it lists every session bead in the store and the caller then
// compares assignees in Go. Its result is therefore invariant across the
// per-work-bead loop in releaseOrphanedPoolAssignments, so counting it measures
// redundant round-trips directly.
type countingLiveSessionListStore struct {
	beads.Store
	liveSessionLists int
}

func (s *countingLiveSessionListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Live && query.Label == sessionBeadLabel {
		s.liveSessionLists++
	}
	return s.Store.List(query)
}

// countOrphanReleaseLiveSessionLists runs one orphan-release sweep over
// workBeadCount beads that all share a single assignee owning no live session
// bead, and returns how many live gc:session listings the sweep issued.
//
// The fixture mirrors the production call shape: assignedWorkStoreRefs is
// index-aligned with the work beads (storeRefAware), which is what
// collectAssignedWorkBeadsWithStores always produces.
func countOrphanReleaseLiveSessionLists(t *testing.T, workBeadCount int) int {
	t.Helper()

	mem := beads.NewMemStore()
	store := &countingLiveSessionListStore{Store: mem}

	var work []beads.Bead
	var stores []beads.Store
	var storeRefs []string
	for i := 0; i < workBeadCount; i++ {
		b, err := mem.Create(beads.Bead{
			Title:    fmt.Sprintf("orphaned pool work %d", i),
			Assignee: "worker-dead",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("Create work bead %d: %v", i, err)
		}
		if err := mem.Update(b.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("Set work status %d: %v", i, err)
		}
		b, err = mem.Get(b.ID)
		if err != nil {
			t.Fatalf("Reload work bead %d: %v", i, err)
		}
		work = append(work, b)
		stores = append(stores, store)
		storeRefs = append(storeRefs, "")
	}

	releaseOrphanedPoolAssignments(
		store,
		beads.SessionStore{Store: store},
		testPoolReleaseConfig(),
		"",
		nil,
		work,
		stores,
		storeRefs,
		nil,
	)
	return store.liveSessionLists
}

// TestReleaseOrphanedPoolAssignments_LiveSessionListsDoNotScaleWithWorkBeads
// pins the cost of the orphan-release sweep to the store topology, not to the
// size of the assigned-work snapshot.
//
// MEASURED on the live gc-management controller 2026-09-05 (ga-451jnv): the
// bead_reconcile.release_orphaned_pool_assignments phase took 645-725s on four
// consecutive reconcile ticks and released 0 beads every time — 79-86% of a
// tick whose configured patrol interval is 30s. The snapshot held 68-69
// assigned work beads across only 18 distinct assignees, and the sweep probes
// two stores per bead, so the assignee-independent listing above was issued
// ~138 times per tick against a loaded Dolt store.
//
// The observable cost is wake latency. buildDesiredState — which computes the
// named-session demand that wakes a sleeping on_demand singleton — runs once per
// tick, so a ~14-minute tick bounds wake latency at ~14 minutes no matter how
// promptly the sling poke arrives.
//
// Asserting independence rather than an exact count is deliberate: the honest
// invariant is "this listing does not repeat per work bead", which a fixed
// magic number would not distinguish from a smaller-but-still-linear sweep.
func TestReleaseOrphanedPoolAssignments_LiveSessionListsDoNotScaleWithWorkBeads(t *testing.T) {
	const (
		smallSnapshot = 4
		largeSnapshot = 40
	)

	small := countOrphanReleaseLiveSessionLists(t, smallSnapshot)
	large := countOrphanReleaseLiveSessionLists(t, largeSnapshot)

	if large != small {
		t.Fatalf("live gc:session listings scale with the work snapshot: %d beads -> %d listings, %d beads -> %d listings; "+
			"want the same count for both. liveOpenSessionAssignmentExists issues an assignee-independent "+
			"List{Label: sessionBeadLabel, Live: true}, and releaseOrphanedPoolAssignments calls it inside its "+
			"per-bead loop (once for the sessions store, once for the work bead's owner store), making the sweep "+
			"O(work beads) live store round-trips instead of O(distinct assignees) — 645-725s per reconcile tick "+
			"on gc-management, releasing 0 beads (ga-451jnv)",
			smallSnapshot, small, largeSnapshot, large)
	}
}

// seedOrphanedWorkInStore creates an in_progress work bead assigned to "nux"
// with no session bead anywhere in store — a genuinely orphaned claim.
func seedOrphanedWorkInStore(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()

	work, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Assignee: "nux",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	if work, err = store.Get(work.ID); err != nil {
		t.Fatalf("reload work bead: %v", err)
	}
	return work
}

// TestReleaseOrphanedPoolAssignments_OwnerStoreMemoDoesNotCollapseDistinctStores
// pins the safety property the owner-store memo rests on: its cached answer is
// keyed by the work bead's store ref as well as the assignee, so two beads that
// share an assignee but live in DIFFERENT stores are still judged independently.
//
// The memo is sound only because liveOpenSessionAssignmentExists is
// assignee-independent *per store* (see the fanout test above), which makes
// (store, assignee) — not assignee alone — the correct key.
// collectAssignedWorkBeadsWithStores appends assignedWorkStores[i] and
// assignedWorkStoreRefs[i] from the same census leg ("the city work store under
// the empty store-ref, the serving rigs under their names, then every relocated
// class binding under its own class:* ref", build_desired_state.go), so equal
// refs do mean the same store. This test keeps that invariant load-bearing
// rather than incidental.
//
// Both collapse directions are claim-level bugs, which is why this asserts the
// exact released set rather than a count: cache the live answer for the dead
// store and a genuinely orphaned claim is stranded forever; cache the dead
// answer for the live store and a live holder's claim is released underneath it
// — the claim loss ga-g3pf0 and #5242 exist to prevent.
func TestReleaseOrphanedPoolAssignments_OwnerStoreMemoDoesNotCollapseDistinctStores(t *testing.T) {
	// Serves no session beads, so the sessions-store gate answers "dead" for
	// both beads and the verdict falls to the owner-store probe under test.
	sessionsStore := beads.NewMemStore()

	liveStore := beads.NewMemStore() // a live session bead for "nux", plus its work
	deadStore := beads.NewMemStore() // work only — "nux" holds no session here

	liveWork := seedOwnerStoreReleaseFixture(t, liveStore, liveStore, "open")
	deadWork := seedOrphanedWorkInStore(t, deadStore, "orphaned claim in a second store")

	// Run both orderings: a memo populated by the live bead first must not leak
	// into the dead bead's verdict, and vice versa.
	for _, tc := range []struct {
		name  string
		work  []beads.Bead
		store []beads.Store
		refs  []string
	}{
		{"live first", []beads.Bead{liveWork, deadWork}, []beads.Store{liveStore, deadStore}, []string{"rig-live", "rig-dead"}},
		{"dead first", []beads.Bead{deadWork, liveWork}, []beads.Store{deadStore, liveStore}, []string{"rig-dead", "rig-live"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Re-seed so each ordering starts from the same claim state.
			if err := liveStore.Update(liveWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: stringPtr("nux")}); err != nil {
				t.Fatalf("restore live claim: %v", err)
			}
			if err := deadStore.Update(deadWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: stringPtr("nux")}); err != nil {
				t.Fatalf("restore dead claim: %v", err)
			}

			released := releaseOrphanedPoolAssignments(
				sessionsStore,
				beads.SessionStore{Store: sessionsStore},
				testPoolReleaseConfig(),
				"",
				nil,
				tc.work,
				tc.store,
				tc.refs,
				nil,
			)

			var releasedIDs []string
			for _, r := range released {
				releasedIDs = append(releasedIDs, r.ID)
			}
			if len(releasedIDs) != 1 || releasedIDs[0] != deadWork.ID {
				t.Fatalf("released %v, want exactly [%s] — the owner-store liveness memo collapsed two distinct "+
					"stores onto one answer. liveOpenSessionAssignmentExists is assignee-independent per store, so "+
					"its memo key must name the store (assignedWorkStoreRefs[i]) and not the assignee alone: %q holds "+
					"a live session bead in one store and none in the other, and the two claims must be judged apart",
					releasedIDs, deadWork.ID, "nux")
			}
			got, err := liveStore.Get(liveWork.ID)
			if err != nil {
				t.Fatalf("re-read the live holder's claim: %v", err)
			}
			if got.Status != "in_progress" || got.Assignee != "nux" {
				t.Fatalf("the live holder's claim is status=%q assignee=%q, want in_progress/nux — a live claim was "+
					"released from a cached verdict belonging to a different store", got.Status, got.Assignee)
			}
		})
	}
}
