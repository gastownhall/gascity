package main

import (
	"fmt"
	"testing"
	"time"

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
		nil,
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
				nil,
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

// newDeadAssigneeWorkBeads creates n in_progress work beads in store, all
// assigned to assignee and routed to the "worker" template, with no session
// bead anywhere — the fixture shape that reaches the owner-store liveness
// probe at the bottom of releaseOrphanedPoolAssignments's per-bead loop.
func newDeadAssigneeWorkBeads(t *testing.T, store beads.Store, n int, assignee string) []beads.Bead {
	t.Helper()

	var work []beads.Bead
	for i := 0; i < n; i++ {
		b, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("orphaned pool work %d", i),
			Assignee: assignee,
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work bead %d: %v", i, err)
		}
		if err := store.Update(b.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("set work in_progress %d: %v", i, err)
		}
		if b, err = store.Get(b.ID); err != nil {
			t.Fatalf("reload work bead %d: %v", i, err)
		}
		work = append(work, b)
	}
	return work
}

// findCapturedTracePhase returns the single captured phase with the given
// name, failing the test if it is missing or duplicated.
func findCapturedTracePhase(t *testing.T, captured []capturedTracePhase, name string) capturedTracePhase {
	t.Helper()

	var found []capturedTracePhase
	for _, p := range captured {
		if p.name == name {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("phase %q recorded %d times, want 1; captured=%+v", name, len(found), captured)
	}
	return found[0]
}

// TestReleaseOrphanedPoolAssignments_RecordsMemoizedVsFallbackBranchCounts
// covers ga-ihbl3e.1 work package 2: the owner-store liveness probe at the
// bottom of the per-bead loop takes either the memoized branch (O(1) once the
// store is warm) or the unmemoized liveOpenSessionAssignmentExists fallback
// (a live store round-trip per bead) depending on whether the call is both
// store-aware and store-ref-aware. That fallback is the exact residual cost
// measured on gc-management 2026-09-05 (ga-451jnv): 319.5s of a 609,851ms
// tick. The sweep must report how many assignees took each branch so a
// regression that silently drops a store-aware caller back onto the fallback
// shows up as a nonzero fallback_count instead of only as a duration blip.
func TestReleaseOrphanedPoolAssignments_RecordsMemoizedVsFallbackBranchCounts(t *testing.T) {
	t.Run("store-aware and store-ref-aware takes the memoized branch", func(t *testing.T) {
		mem := beads.NewMemStore()
		work := newDeadAssigneeWorkBeads(t, mem, 3, "worker-dead")
		stores := []beads.Store{mem, mem, mem}
		storeRefs := []string{"", "", ""}

		var captured []capturedTracePhase
		recordPhase := func(site TraceSiteCode, name string, start time.Time, fields map[string]any) {
			if start.IsZero() {
				t.Fatalf("recordPhase %s: start time is zero", name)
			}
			captured = append(captured, capturedTracePhase{site: site, name: name, fields: fields})
		}

		releaseOrphanedPoolAssignments(
			mem, beads.SessionStore{Store: mem}, testPoolReleaseConfig(), "", nil,
			work, stores, storeRefs, nil, nil, recordPhase,
		)

		phase := findCapturedTracePhase(t, captured, "bead_reconcile.release_orphaned_pool_assignments.release_sweep")
		if phase.site != TraceSiteControllerTickPhase {
			t.Fatalf("release_sweep site = %q, want %q", phase.site, TraceSiteControllerTickPhase)
		}
		if got := phase.fields["memoized_count"]; got != 3 {
			t.Fatalf("memoized_count = %v, want 3", got)
		}
		if got := phase.fields["fallback_count"]; got != 0 {
			t.Fatalf("fallback_count = %v, want 0 — a store-aware, store-ref-aware call shape must stay on the memoized branch", got)
		}
		// probe_ms isolates the liveness-probe cost from the rest of the sweep
		// body (ownership-index build, gates, release writes), which is the
		// number ga-ihbl3e needs. Against a MemStore the probes finish in
		// microseconds and floor to 0, so assert presence and a sane type
		// rather than a nonzero duration.
		probeMS, ok := phase.fields["probe_ms"]
		if !ok {
			t.Fatalf("release_sweep fields have no probe_ms; fields=%+v", phase.fields)
		}
		probeMSInt, ok := probeMS.(int64)
		if !ok {
			t.Fatalf("probe_ms = %v (%T), want int64", probeMS, probeMS)
		}
		if probeMSInt < 0 {
			t.Fatalf("probe_ms = %d, want >= 0", probeMSInt)
		}
	})

	t.Run("neither store-aware nor store-ref-aware falls back to the unmemoized branch", func(t *testing.T) {
		mem := beads.NewMemStore()
		work := newDeadAssigneeWorkBeads(t, mem, 3, "worker-dead")

		var captured []capturedTracePhase
		recordPhase := func(site TraceSiteCode, name string, start time.Time, fields map[string]any) {
			if start.IsZero() {
				t.Fatalf("recordPhase %s: start time is zero", name)
			}
			captured = append(captured, capturedTracePhase{site: site, name: name, fields: fields})
		}

		releaseOrphanedPoolAssignments(
			mem, beads.SessionStore{Store: mem}, testPoolReleaseConfig(), "", nil,
			work, nil, nil, nil, nil, recordPhase,
		)

		phase := findCapturedTracePhase(t, captured, "bead_reconcile.release_orphaned_pool_assignments.release_sweep")
		if got := phase.fields["fallback_count"]; got != 3 {
			t.Fatalf("fallback_count = %v, want 3", got)
		}
		if got := phase.fields["memoized_count"]; got != 0 {
			t.Fatalf("memoized_count = %v, want 0 — a call shape with no store awareness must not report memoized work", got)
		}
	})
}
