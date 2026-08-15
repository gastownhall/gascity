package main

import (
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A worker that cleared both gc.routed_to and assignee in a failed done
// sequence (e.g. $REFINERY_TARGET was empty) leaves the work bead
// open+unassigned+unrouted with a branch on origin. The pool demand probe
// can't see it (keys on gc.routed_to) and releaseOrphanedPoolAssignments
// can't recover it (only processes assigned work). sweepDetachedHandoffOrphans
// finds it via gc.session_name → session bead → template.
func TestSweepDetachedHandoffOrphans_RestoresRoute(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-abc",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphaned work",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-abc",
			beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
			// gc.routed_to and assignee left empty by failed done sequence
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if got.Assignee != "" {
		t.Fatalf("assignee=%q, want empty", got.Assignee)
	}
	if got.Status != "open" {
		t.Fatalf("status=%q, want open", got.Status)
	}
}

// A bead that still has gc.routed_to set must not be touched.
func TestSweepDetachedHandoffOrphans_SkipsAlreadyRouted(t *testing.T) {
	store := beads.NewMemStore()

	work, err := store.Create(beads.Bead{
		Title:  "routed work",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-routed",
			beadmeta.SessionNameMetadataKey: "some-session",
			beadmeta.RoutedToMetadataKey:    "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to changed unexpectedly to %q", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A bead that still has an assignee must not be touched (covered by
// releaseOrphanedPoolAssignments when the session closes).
func TestSweepDetachedHandoffOrphans_SkipsAssigned(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Status:   "open",
		Assignee: "some-session-id",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-assigned",
			beadmeta.SessionNameMetadataKey: "some-session-id",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (assigned beads are not detached orphans)", n)
	}
}

// Workflow-kind beads route via gc.run_target and are handled by
// restoreCarriedWorkRoutes — the detached-orphan sweep must leave them alone.
func TestSweepDetachedHandoffOrphans_SkipsWorkflowKind(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "workflow root",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-wf",
			beadmeta.SessionNameMetadataKey: "some-session",
			beadmeta.KindMetadataKey:        beadmeta.KindWorkflow,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (workflow-kind beads are not detached handoff orphans)", n)
	}
}

// A bead with no branch set is not a completed-work bead and must be skipped.
func TestSweepDetachedHandoffOrphans_SkipsNoBranch(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "no-branch bead",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.SessionNameMetadataKey: "some-session",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (no branch → not a completed-work bead)", n)
	}
}

// A bead with no gc.session_name cannot be routed back to the pool because
// we have no session reference to recover the route from.
func TestSweepDetachedHandoffOrphans_SkipsNoSessionName(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "no-session work",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey: "polecat/ga-nosession",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (no gc.session_name → can't recover route)", n)
	}
}

// When the session bead is not found (e.g. already GC'd), the bead is left
// untouched — no route to recover.
func TestSweepDetachedHandoffOrphans_SkipsWhenSessionNotFound(t *testing.T) {
	store := beads.NewMemStore()

	work, err := store.Create(beads.Bead{
		Title:  "orphan with missing session",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-gone",
			beadmeta.SessionNameMetadataKey: "gastown__polecat-gone",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (session bead not found)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty (no session bead to recover from)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// When the session bead exists but has no template/agent_name, no route can be
// recovered — the bead is left untouched but not an error.
func TestSweepDetachedHandoffOrphans_SkipsWhenSessionHasNoTemplate(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "templateless session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "unknown-session",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphan with templateless session",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-notempl",
			beadmeta.SessionNameMetadataKey: "unknown-session",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (session has no template)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A closed session bead still carries the template we need — route recovery
// works even after the worker session has been closed and GC-swept.
func TestSweepDetachedHandoffOrphans_RecoverFromClosedSessionBead(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "closed polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-closed",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	// Close the session bead to simulate the dead-worker scenario.
	closed := "closed"
	if err := store.Update(sessionBead.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphaned work (closed session)",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-closed",
			beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (closed session bead still carries template)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// Multiple detached orphans from the same session are all recovered in one sweep.
func TestSweepDetachedHandoffOrphans_MultipleCandidates(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-multi",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{
			Status: "open",
			Metadata: map[string]string{
				beadmeta.WorkBranchMetadataKey:  "polecat/ga-multi",
				beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
			},
		}); err != nil {
			t.Fatalf("create work bead %d: %v", i, err)
		}
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 3 {
		t.Fatalf("restored=%d, want 3", n)
	}
}

// Nil store is a no-op.
func TestSweepDetachedHandoffOrphans_NilStore(t *testing.T) {
	n, err := sweepDetachedHandoffOrphans(nil)
	if err != nil {
		t.Fatalf("sweep nil store: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 for nil store", n)
	}
}

// A detached handoff orphan can live in a RIG store while its closing session
// bead lives in the CITY store — the common case sweepDetachedHandoffOrphansAcross
// Stores exists for. The route index must be built from the city store too, or
// the rig-stored orphan's session bead is never found and it is never recovered.
// Cross-store round-trip through the public entry point.
func TestSweepDetachedHandoffOrphansAcrossStores_RigOrphanCityStoredSession(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Session bead lives in the CITY store (where session beads live).
	if _, err := cityStore.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-xyz",
			"template":     "gascity/gastown.polecat",
		},
	}); err != nil {
		t.Fatalf("create city session bead: %v", err)
	}

	// Detached orphan lives in the RIG store, referencing the city-stored session.
	work, err := rigStore.Create(beads.Bead{
		Title:  "orphaned rig work",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-xyz",
			beadmeta.SessionNameMetadataKey: "gastown__polecat-th-xyz",
			// gc.routed_to and assignee left empty by the failed done sequence
		},
	})
	if err != nil {
		t.Fatalf("create rig work bead: %v", err)
	}

	n := sweepDetachedHandoffOrphansAcrossStores(cityStore, map[string]beads.Store{"ga": rigStore}, "test", io.Discard)
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (rig orphan recovered via city-stored session bead)", n)
	}

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get rig work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A candidate that is blocked in the backing store (bd blocked/review/testing,
// which mapBdStatus collapses to "open" on every decoded read) must NOT be
// re-stamped. The cached candidate List returns it as ready; only a Live query
// reaches bd's raw-status filter and correctly omits it. This is the gc-4zb
// class the sibling restoreCarriedWorkRoutes already guards — the MemStore-only
// suite could not reproduce it because MemStore stores the literal status. The
// closing session bead lives in a distinct plain routeStore so the route IS
// recoverable and the only thing standing between the sweep and a re-stamp is
// the Live candidate List. Fails against a non-Live candidate List (restored=1);
// passes with Live (collapsedBlockedStatusStore serves nil to a Live query).
func TestSweepDetachedHandoffOrphans_SkipsBlockedCollapsedCandidate(t *testing.T) {
	const sessionName = "gastown__polecat-th-blk"
	// Backing store: the candidate is blocked in bd but decodes as "open"
	// (mapBdStatus), carrying the full detached-orphan signature with an
	// already-consumed (empty) gc.routed_to.
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "EB-blk", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-blk",
			beadmeta.SessionNameMetadataKey: sessionName,
		}},
	}, nil)
	store := collapsedBlockedStatusStore{
		Store: live,
		cachedSnapshot: []beads.Bead{
			{ID: "EB-blk", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
				beadmeta.WorkBranchMetadataKey:  "polecat/ga-blk",
				beadmeta.SessionNameMetadataKey: sessionName,
			}},
		},
		// bd's --status=open filter sees raw status=blocked and excludes it.
		liveSnapshot: nil,
	}
	routeStore := beads.NewMemStore()
	if _, err := routeStore.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "gascity/gastown.polecat",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	restored, err := sweepDetachedHandoffOrphansWithRouteStore(store, routeStore)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored=%d, want 0 (a blocked/collapsed candidate must not be re-stamped)", restored)
	}
	if got := mustRoutedTo(t, live, "EB-blk"); got != "" {
		t.Errorf("gc.routed_to=%q, want empty (a blocked bead must stay unrouted)", got)
	}
}

// A candidate that a worker claims in the window between the open-bead List
// snapshot and the per-candidate write must NOT be re-stamped. On production
// CachingStores a plain Get can still return the pre-claim cached bead, so the
// sweep must re-read through the authoritative cache-bypassing live handle. Here
// the stale cache serves the open candidate to both List and plain Get, while
// the live backing store already holds the claim (in_progress, assigned, route
// consumed — ga-sa0/ga-bgu). Fails against a blind SetMetadata (restored=1);
// passes with the handles.Live.Get re-read.
func TestSweepDetachedHandoffOrphans_SkipsRaceClaimedCandidate(t *testing.T) {
	const sessionName = "gastown__polecat-th-race"
	// Backing/live store: the bead has ALREADY been claimed — open->in_progress,
	// assignee set, gc.routed_to consumed.
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "EB-race", Title: "work", Type: "task", Status: "in_progress",
			Assignee: "gascity/gastown.polecat/th-race", Metadata: map[string]string{
				beadmeta.WorkBranchMetadataKey:  "polecat/ga-race",
				beadmeta.SessionNameMetadataKey: sessionName,
			},
		},
	}, nil)
	// Stale cache: List and plain Get still return the pre-claim candidate — open,
	// unassigned, unrouted — so a blind re-stamp would clobber the claim. Only
	// HandlesFor(store).Live.Get reaches the live claim.
	store := staleCacheStore{
		Store: live,
		cached: beads.Bead{
			ID: "EB-race", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
				beadmeta.WorkBranchMetadataKey:  "polecat/ga-race",
				beadmeta.SessionNameMetadataKey: sessionName,
			},
		},
	}
	routeStore := beads.NewMemStore()
	if _, err := routeStore.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "gascity/gastown.polecat",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	restored, err := sweepDetachedHandoffOrphansWithRouteStore(store, routeStore)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored=%d, want 0 (a candidate claimed since the snapshot must not be re-stamped)", restored)
	}
	// The claim's route consumption must survive in the live store.
	if got := mustRoutedTo(t, live, "EB-race"); got != "" {
		t.Errorf("gc.routed_to=%q, want empty (claim consumed the route; sweep must not re-stamp)", got)
	}
	b, err := live.Get("EB-race")
	if err != nil {
		t.Fatalf("get EB-race: %v", err)
	}
	if b.Status != "in_progress" || b.Assignee == "" {
		t.Errorf("EB-race status=%q assignee=%q, want in_progress + assigned (untouched)", b.Status, b.Assignee)
	}
}

// A production claim stamps gc.work_branch, gc.session_id, AND gc.session_name
// together in one hookClaimIdentityPatch — never a raw "branch" key. This is the
// exact shape a detached orphan carries in production; before the branch gate read
// gc.work_branch it was skipped and no route was ever restored. Route recovery
// resolves through the exact gc.session_id → session-bead ID match. Pins F1.
func TestSweepDetachedHandoffOrphans_ClaimShapedRoute(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-claim",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "claim-shaped orphan",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-claim",
			beadmeta.SessionIDMetadataKey:   sessionBead.ID,
			beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	// The regression contract: production never stamps a raw "branch" key.
	if _, ok := work.Metadata["branch"]; ok {
		t.Fatalf("fixture must not carry a raw \"branch\" key — production stamps gc.work_branch")
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (claim-shaped orphan must be recovered via gc.work_branch)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// Two historical session beads can share a session_name while resolving to
// different pool routes. Claimed work also carries gc.session_id — the unique
// session-bead ID — so route recovery must prefer that exact match and restore the
// orphan to ITS session's route, not the first duplicate's. Mirrors
// internal/session/resolve.go preferring an exact id over an ambiguous name.
func TestSweepDetachedHandoffOrphans_DuplicateSessionNamePrefersSessionID(t *testing.T) {
	store := beads.NewMemStore()

	const sharedName = "gastown__polecat-dup"
	// First session with this name routes to polecat-a.
	if _, err := store.Create(beads.Bead{
		Title:  "polecat session A",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sharedName,
			"template":     "gascity/gastown.polecat-a",
		},
	}); err != nil {
		t.Fatalf("create session bead A: %v", err)
	}
	// Second session reuses the name but routes to polecat-b.
	sessionB, err := store.Create(beads.Bead{
		Title:  "polecat session B",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sharedName,
			"template":     "gascity/gastown.polecat-b",
		},
	})
	if err != nil {
		t.Fatalf("create session bead B: %v", err)
	}

	// The orphan belongs to session B by exact gc.session_id, though its name is shared.
	work, err := store.Create(beads.Bead{
		Title:  "orphan owned by session B",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-dup",
			beadmeta.SessionIDMetadataKey:   sessionB.ID,
			beadmeta.SessionNameMetadataKey: sharedName,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (exact gc.session_id must resolve even with a duplicated name)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat-b" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat-b (session B's route, chosen by gc.session_id)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// When a session_name is shared by sessions resolving to conflicting routes and
// the orphan carries NO gc.session_id to disambiguate, the sweep must refuse to
// guess — restoring to an arbitrary one could hand the work to the wrong pool.
// Mirrors internal/session/resolve.go treating an ambiguous match as an error.
func TestSweepDetachedHandoffOrphans_AmbiguousSessionNameWithoutSessionID(t *testing.T) {
	store := beads.NewMemStore()

	const sharedName = "gastown__polecat-ambig"
	for _, tmpl := range []string{"gascity/gastown.polecat-a", "gascity/gastown.polecat-b"} {
		if _, err := store.Create(beads.Bead{
			Title:  "polecat session " + tmpl,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name": sharedName,
				"template":     tmpl,
			},
		}); err != nil {
			t.Fatalf("create session bead %s: %v", tmpl, err)
		}
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphan with only an ambiguous name",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-ambig",
			beadmeta.SessionNameMetadataKey: sharedName,
			// no gc.session_id — nothing to disambiguate the shared name
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (an ambiguous session_name with no gc.session_id must not be guessed)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty (ambiguous name must not resolve to an arbitrary route)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A session_name shared by sessions that all resolve to the SAME route is not
// ambiguous — every match agrees — so an orphan carrying only that name (no
// gc.session_id) is still recovered. Only conflicting routes force the refuse-to-
// guess path; agreement resolves.
func TestSweepDetachedHandoffOrphans_DuplicateSessionNameAgreeingRouteResolves(t *testing.T) {
	store := beads.NewMemStore()

	const sharedName = "gastown__polecat-agree"
	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "polecat session",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name": sharedName,
				"template":     "gascity/gastown.polecat",
			},
		}); err != nil {
			t.Fatalf("create session bead %d: %v", i, err)
		}
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphan with an agreeing duplicated name",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-agree",
			beadmeta.SessionNameMetadataKey: sharedName,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (a duplicated name whose matches agree must still resolve)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A production claim may stamp gc.session_id WITHOUT gc.session_name: the hook
// path stamps gc.session_id whenever GC_SESSION_ID is set but adds
// gc.session_name only when GC_SESSION_NAME is also present
// (hookClaimIdentityPatch, cmd_hook_claim.go). Such a session-ID-only orphan is
// a valid shape, and the exact gc.session_id → session-bead ID resolver can
// recover it — so the candidate gate must accept it even though gc.session_name
// is absent. Before the fix isDetachedHandoffOrphanCandidate required
// gc.session_name and stranded this orphan invisibly, unreachable by the exact-ID
// resolver. Pins the session-ID-only candidate fix.
func TestSweepDetachedHandoffOrphans_SessionIDOnlyNoSessionName(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-idonly",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "session-id-only orphan",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey: "polecat/ga-idonly",
			beadmeta.SessionIDMetadataKey:  sessionBead.ID,
			// no gc.session_name — GC_SESSION_NAME was unset at claim time
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	// The regression contract: this orphan carries no gc.session_name at all.
	if _, ok := work.Metadata[beadmeta.SessionNameMetadataKey]; ok {
		t.Fatalf("fixture must not carry gc.session_name — it pins the session-ID-only shape")
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (a gc.session_id-only orphan must recover via the exact ID match)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// partialUnionSessionStore models a bd session-bead List that returns a PARTIAL
// result: some session rows failed to parse (or one tier was unavailable), so
// ListAllSessionBeads folds the incomplete rows in and returns a
// PartialResultError alongside them. Only the two session-bead union legs (by
// Type=session and by Label=gc:session, matching ListAllSessionBeads) return the
// partial subset; the candidate open-bead scan (Type/Label empty) delegates to
// the backing store. A conflicting same-name session bead lives in the unlisted
// partition, so the visible subset cannot detect the ambiguity.
type partialUnionSessionStore struct {
	beads.Store                  // backing store: holds the detached orphan
	partialSessions []beads.Bead // the incomplete session-bead subset surfaced by List
}

func (s partialUnionSessionStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Type == sessionBeadType || q.Label == sessionBeadLabel {
		return append([]beads.Bead(nil), s.partialSessions...),
			&beads.PartialResultError{Op: "bd list", Err: errors.New("a session row failed to parse")}
	}
	return s.Store.List(q)
}

// When ListAllSessionBeads returns a PartialResultError, the missing partition
// can hide a conflicting same-name session bead, so an ambiguous gc.session_name
// would silently resolve to whichever duplicate happened to be listed. The
// route-index build must treat a partial list as unsafe for the byName fallback
// and recover only by exact gc.session_id. Here two sessions share a name and
// resolve to CONFLICTING routes, but only one is visible in the partial list and
// the orphan carries no gc.session_id — so it must remain unrouted rather than be
// guessed onto the visible route. Before the fix the visible duplicate populated
// byName and the orphan was restored to an arbitrary pool. Pins the partial-list
// name-fallback fix.
func TestSweepDetachedHandoffOrphans_PartialSessionListSkipsNameFallback(t *testing.T) {
	const sharedName = "gastown__polecat-partial"

	// Backing store: just the detached orphan, carrying only the shared name.
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:  "orphan with an ambiguous name under a partial session list",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-partial",
			beadmeta.SessionNameMetadataKey: sharedName,
			// no gc.session_id — nothing to disambiguate the shared name
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	// The partial session list surfaces only ONE of two conflicting same-name
	// sessions (route polecat-a). The orphan's true owner (route polecat-b) is in
	// the unlisted partition, invisible to the ambiguity check.
	store := partialUnionSessionStore{
		Store: backing,
		partialSessions: []beads.Bead{{
			ID:     "SB-partial-a",
			Title:  "polecat session A",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Status: "open",
			Metadata: map[string]string{
				"session_name": sharedName,
				"template":     "gascity/gastown.polecat-a",
			},
		}},
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (a partial session list must not resolve an ambiguous name)", n)
	}

	got, err := backing.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty (partial list must not guess a name-only route)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A partial session list still recovers an orphan that carries an exact
// gc.session_id: session-bead IDs are unique, so a partial list can only omit an
// ID entry entirely (recovered on a later complete tick), never make one
// ambiguous. The byID map therefore stays trustworthy even when byName is
// suppressed. This guards the fix from over-correcting into "partial list → never
// recover anything," which would strand an exactly-identified orphan.
func TestSweepDetachedHandoffOrphans_PartialSessionListStillResolvesByID(t *testing.T) {
	const sharedName = "gastown__polecat-partial-id"

	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:  "orphan recoverable by exact id under a partial list",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.WorkBranchMetadataKey:  "polecat/ga-partial-id",
			beadmeta.SessionIDMetadataKey:   "SB-partial-id",
			beadmeta.SessionNameMetadataKey: sharedName,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	store := partialUnionSessionStore{
		Store: backing,
		partialSessions: []beads.Bead{{
			ID:     "SB-partial-id",
			Title:  "polecat session",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Status: "open",
			Metadata: map[string]string{
				"session_name": sharedName,
				"template":     "gascity/gastown.polecat",
			},
		}},
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (exact gc.session_id must still resolve under a partial list)", n)
	}

	got, err := backing.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}
