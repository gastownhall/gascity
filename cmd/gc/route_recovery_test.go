package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRouteRecoveryBackstopLegRestoresCarriedWorkRoutes covers ga-n2d.4: after a controller restart,
// open+unassigned work that carries a gc.run_target pool route but no
// gc.routed_to is invisible to the pool autoscaler (which keys on gc.routed_to)
// and never spawns a worker. The lane's backstop leg scan must re-stamp gc.routed_to
// from the route the bead already declares, for both carriers of a legacy route
// — a plain (kind-less) standalone work bead and a pre-ga-eld2x workflow root —
// while leaving every bead for which gc.run_target is not a recoverable pool
// route untouched: already-routed, assigned, closed, control-dispatcher, and
// workflow-topology beads.
func TestRouteRecoveryBackstopLegRestoresCarriedWorkRoutes(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Recoverable: open workflow root, run_target set, routed_to empty.
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Already routed — left alone (idempotent, no double-write).
		{ID: "WR-2", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool, "gc.routed_to": "gascity/gastown.refinery",
		}},
		// Assigned workflow root — already claimed, no route restored.
		{ID: "WR-3", Title: "root", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Closed workflow root — done, no route restored.
		{ID: "WR-4", Title: "root", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Recoverable broadening: a plain (kind-less) standalone work bead — this
		// fork's dominant work shape — carries its pool route in gc.run_target
		// too. The autoscaler is blind to it until gc.routed_to is restored.
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Assigned plain work bead — already claimed, no route restored.
		{ID: "T-2", Title: "work", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Already-routed plain work bead — idempotent, left untouched.
		{ID: "T-3", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool, "gc.routed_to": pool,
		}},
		// Control-dispatcher and workflow-topology beads carry a bare
		// gc.run_target, but there it is a dispatch/structure target an agent
		// never claims from a pool — they must never be pool-routed.
		{ID: "CTRL-1", Title: "retry", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "retry", "gc.run_target": pool,
		}},
		{ID: "TOPO-1", Title: "scope", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "scope", "gc.run_target": pool,
		}},
		{ID: "TOPO-2", Title: "spec", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "spec", "gc.run_target": pool,
		}},
	}, nil)

	restored, err := scanOneRouteRecoveryLeg(store)
	if err != nil {
		t.Fatalf("backstop leg scan: %v", err)
	}
	if restored != 2 {
		t.Fatalf("restored = %d, want 2 (WR-1 workflow root + T-1 plain work bead)", restored)
	}

	// Restored from the route each bead already carried.
	for _, id := range []string{"WR-1", "T-1"} {
		if got := mustRoutedTo(t, store, id); got != pool {
			t.Errorf("%s gc.routed_to = %q, want %q (restored from gc.run_target)", id, got, pool)
		}
	}
	// Already-routed beads keep their original route, not their run_target.
	if got := mustRoutedTo(t, store, "WR-2"); got != "gascity/gastown.refinery" {
		t.Errorf("WR-2 gc.routed_to = %q, want gascity/gastown.refinery (untouched)", got)
	}
	if got := mustRoutedTo(t, store, "T-3"); got != pool {
		t.Errorf("T-3 gc.routed_to = %q, want %q (untouched)", got, pool)
	}
	// Assigned, closed, control, and topology beads must stay unrouted.
	for _, id := range []string{"WR-3", "WR-4", "T-2", "CTRL-1", "TOPO-1", "TOPO-2"} {
		if got := mustRoutedTo(t, store, id); got != "" {
			t.Errorf("%s gc.routed_to = %q, want empty (must be left unrouted)", id, got)
		}
	}

	// Idempotent: a second pass restores nothing because WR-1 and T-1 now carry
	// gc.routed_to and yield no recoverable carried route.
	restored2, err := scanOneRouteRecoveryLeg(store)
	if err != nil {
		t.Fatalf("backstop leg scan (second pass): %v", err)
	}
	if restored2 != 0 {
		t.Errorf("second pass restored = %d, want 0 (idempotent)", restored2)
	}
}

// TestRouteRecoveryBackstopLegNilStore guards the nil-store path the controller
// hits when a scope's bead store is unavailable.
func TestRouteRecoveryBackstopLegNilStore(t *testing.T) {
	restored, err := scanOneRouteRecoveryLeg(nil)
	if err != nil {
		t.Fatalf("backstop leg scan(nil): %v", err)
	}
	if restored != 0 {
		t.Errorf("restored = %d, want 0 for nil store", restored)
	}
}

// staleOpenListStore returns a fixed open-bead snapshot from List while
// delegating every live read/write (Get, SetMetadata, …) to an embedded store.
// It reproduces the reconcile TOCTOU: the backstop leg scan captures the open
// snapshot, but a polecat claims the bead before the per-bead re-stamp runs, so
// the live store already holds the claimed (in_progress) bead.
type staleOpenListStore struct {
	beads.Store
	openSnapshot []beads.Bead
}

func (s staleOpenListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), s.openSnapshot...), nil
}

// TestRouteRecoveryBackstopLegSkipsRaceClaimedBead covers ga-bgu: restore must
// not re-stamp gc.routed_to onto a bead that a polecat claimed after the
// open-bead List snapshot. The claim atomically consumes the pool route
// (open->in_progress, assignee set, gc.routed_to cleared, gc.run_target recorded
// — ga-sa0). A blind SetMetadata keyed on the stale snapshot resurrects
// gc.routed_to on the now-in_progress bead, feeding the dispatcher a phantom
// pool-demand bead that flaps open<->in_progress. Restore must re-read the live
// bead and skip the write when it is no longer open+unassigned.
func TestRouteRecoveryBackstopLegSkipsRaceClaimedBead(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	// Live store: the bead has ALREADY been claimed — open->in_progress, assignee
	// set, gc.routed_to consumed, gc.run_target carrying the route (ga-sa0 claim).
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "T-1", Title: "work", Type: "task", Status: "in_progress",
			Assignee: pool + "/th-abc", Metadata: map[string]string{
				"gc.run_target": pool,
			},
		},
	}, nil)
	// Stale snapshot: List captured T-1 BEFORE the claim — open, unassigned,
	// unrouted, carrying gc.run_target, so carriedPoolRoute(snapshot) == pool.
	store := staleOpenListStore{
		Store: live,
		openSnapshot: []beads.Bead{
			{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
				"gc.run_target": pool,
			}},
		},
	}

	restored, err := scanOneRouteRecoveryLeg(store)
	if err != nil {
		t.Fatalf("backstop leg scan: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (must not re-stamp a bead claimed since the snapshot)", restored)
	}
	// The claim's route consumption must survive: gc.routed_to stays empty.
	if got := mustRoutedTo(t, live, "T-1"); got != "" {
		t.Fatalf("T-1 gc.routed_to = %q, want empty (claim consumed the route; restore must not re-stamp)", got)
	}
	// And the bead must remain claimed, not silently mutated back toward demand.
	b, err := live.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if b.Status != "in_progress" || strings.TrimSpace(b.Assignee) == "" {
		t.Fatalf("T-1 status=%q assignee=%q, want in_progress + assigned (untouched)", b.Status, b.Assignee)
	}
}

// staleCacheStore models a CachingStore-wrapped production store whose plain Get
// returns a STALE cached bead — a cross-process claim not yet absorbed into this
// process's cache — while its authoritative Live handle bypasses the cache to the
// backing store and sees the claim. List likewise serves the stale open snapshot.
// It reproduces the production hazard the backstop leg scan must survive: both
// the List snapshot and a plain store.Get show the pre-claim bead, so only a
// cache-bypassing live read (HandlesFor(store).Live.Get) catches the race.
type staleCacheStore struct {
	beads.Store            // backing/live store: authoritative, already holds the claim
	cached      beads.Bead // stale cached view returned by plain Get and List
}

// Get returns the stale cached bead (a cache hit that predates the claim).
func (s staleCacheStore) Get(string) (beads.Bead, error) {
	return s.cached, nil
}

// List returns the stale open snapshot.
func (s staleCacheStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return []beads.Bead{s.cached}, nil
}

// Handles exposes a Live reader that bypasses the stale cache to the backing
// store, mirroring CachingStore.Handles().Live.
func (s staleCacheStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	return beads.StoreHandles{Cached: h.Cached, Live: h.Live, Writer: s.Store}
}

// TestRouteRecoveryBackstopLegSkipsCacheStaleClaimedBead covers the CachingStore
// leg of ga-bgu: on production stores a plain Get can return a cached bead that
// predates a cross-process claim, so restore must re-read through the
// authoritative cache-bypassing live handle. With a stale-cache Get the bead
// still looks open+unassigned+unrouted; only the live backing read shows the
// claim (in_progress, assigned, route consumed). Restore must skip the re-stamp.
// It fails against a plain store.Get re-read and passes with handles.Live.Get.
func TestRouteRecoveryBackstopLegSkipsCacheStaleClaimedBead(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	// Backing/live store: T-1 has ALREADY been claimed (ga-sa0).
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "T-1", Title: "work", Type: "task", Status: "in_progress",
			Assignee: pool + "/th-abc", Metadata: map[string]string{
				"gc.run_target": pool,
			},
		},
	}, nil)
	// Stale cache: both List and plain Get still return the pre-claim T-1 — open,
	// unassigned, unrouted, carrying gc.run_target — so a plain re-read would
	// clobber the claim. Only HandlesFor(store).Live.Get sees the live claim.
	store := staleCacheStore{
		Store: live,
		cached: beads.Bead{
			ID: "T-1", Title: "work", Type: "task", Status: "open",
			Metadata: map[string]string{"gc.run_target": pool},
		},
	}

	restored, err := scanOneRouteRecoveryLeg(store)
	if err != nil {
		t.Fatalf("backstop leg scan: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (stale-cache Get must not defeat the claim guard)", restored)
	}
	// The claim's route consumption must survive in the live store.
	if got := mustRoutedTo(t, live, "T-1"); got != "" {
		t.Fatalf("T-1 gc.routed_to = %q, want empty (claim consumed the route; restore must not re-stamp)", got)
	}
	b, err := live.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if b.Status != "in_progress" || strings.TrimSpace(b.Assignee) == "" {
		t.Fatalf("T-1 status=%q assignee=%q, want in_progress + assigned (untouched)", b.Status, b.Assignee)
	}
}

// TestCityRuntimeRecoverUnroutedWorkRoutes confirms the controller method
// sweeps both the city store and every rig store, and recovers both carried-route
// shapes (workflow root and plain work bead).
func TestCityRuntimeRecoverUnroutedWorkRoutes(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CW-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "city/gastown.polecat",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Plain work bead — the fork's standalone-issue shape.
		{ID: "RW-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": "gascity/gastown.polecat",
		}},
	}, nil)
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"gascity": rigStore},
		stderr:              io.Discard,
	}

	cr.recoverUnroutedWorkRoutes()

	if got := mustRoutedTo(t, cityStore, "CW-1"); got != "city/gastown.polecat" {
		t.Errorf("CW-1 gc.routed_to = %q, want city/gastown.polecat", got)
	}
	if got := mustRoutedTo(t, rigStore, "RW-1"); got != "gascity/gastown.polecat" {
		t.Errorf("RW-1 gc.routed_to = %q, want gascity/gastown.polecat", got)
	}
}

// scanOneRouteRecoveryLeg runs the lane's authoritative per-leg scan on a fresh
// lane, which is the unit the pre-lane restoreCarriedWorkRoutes was: one store,
// one full live open read, one batched re-verify, no cross-pass accounting.
func scanOneRouteRecoveryLeg(store beads.Store) (int, error) {
	report := newRouteRecoveryLane().backstopLeg(planeLeg{store: store})
	return report.restored, report.err
}

func mustRoutedTo(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Metadata["gc.routed_to"]
}

// collapsedBlockedStatusStore models the production read path for a bead that is
// blocked in the backing store. Two behaviors combine there, and neither is
// visible from the bead alone:
//
//  1. mapBdStatus folds bd's blocked/deferred/review/testing into Gas City's
//     three statuses, so a blocked bead decodes with Status "open". Every read
//     that returns a beads.Bead — the cached List and the live Get alike — sees
//     "open", so no status comparison downstream can recognize the block.
//  2. CachingStore.List serves a non-Live query from its in-memory active set,
//     filtering with ListQuery.Matches against that already-collapsed status.
//     bd's server-side --status=open filter does see the raw status and does
//     exclude blocked, but a cached read never reaches it.
//
// A Live query bypasses the cache and reaches bd, which filters on the raw
// status, so the blocked bead is correctly absent from liveSnapshot.
type collapsedBlockedStatusStore struct {
	beads.Store
	cachedSnapshot []beads.Bead // non-Live: blocked rows present, collapsed to "open"
	liveSnapshot   []beads.Bead // Live: bd filtered the raw status server-side
}

func (s collapsedBlockedStatusStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Live {
		return append([]beads.Bead(nil), s.liveSnapshot...), nil
	}
	return append([]beads.Bead(nil), s.cachedSnapshot...), nil
}

// TestRouteRecoveryBackstopLegSkipsBlockedBead covers gc-4zb: restore must not
// re-stamp gc.routed_to onto a bead that is blocked in the backing store.
//
// Live reproduction (EnterpriseBench-42o8, root EnterpriseBench-c7ga, step
// mol-focus-review.finalize): dolt_history_issues shows status=blocked at every
// revision while gc.routed_to oscillated empty -> set on a patrol cadence
// (03:10:05 set, 03:14:04 cleared by blocked-routed-reaper, 03:18:20 set again),
// each restored value equal to gc.run_target — carriedPoolRoute's copy. The
// bead never reopened, so this is a write onto a continuously blocked bead, not
// a legitimate re-route of work that briefly became ready.
//
// The existing open+unassigned guards cannot catch it: the snapshot bead, the
// belt-and-braces b.Status check, and the live re-read all observe the collapsed
// "open". Gating requires a read that filters on the raw status, which is what
// the Live query delegates to bd.
func TestRouteRecoveryBackstopLegSkipsBlockedBead(t *testing.T) {
	const pool = "/home/ds/projects/EnterpriseBench/enterprisebench-worker"
	// Backing bead: blocked in bd, but decoded as "open" by mapBdStatus, so a
	// live Get cannot reveal the block either. The reaper has already cleared
	// gc.routed_to, leaving exactly carriedPoolRoute's recoverable shape.
	live := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "EB-42o8", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
	}, nil)
	store := collapsedBlockedStatusStore{
		Store: live,
		cachedSnapshot: []beads.Bead{
			{ID: "EB-42o8", Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
				"gc.run_target": pool,
			}},
		},
		// bd's --status=open filter sees the raw status=blocked and excludes it.
		liveSnapshot: nil,
	}

	restored, err := scanOneRouteRecoveryLeg(store)
	if err != nil {
		t.Fatalf("backstop leg scan: %v", err)
	}
	if restored != 0 {
		t.Fatalf("restored = %d, want 0 (must not re-stamp gc.routed_to onto a blocked bead)", restored)
	}
	if route := strings.TrimSpace(mustRoutedTo(t, live, "EB-42o8")); route != "" {
		t.Errorf("gc.routed_to = %q, want empty (a blocked bead must stay unrouted)", route)
	}
}

// TestBackstopCountsRoutedWorkTheRuntimePlaneCannotSee pins the visibility half
// of the tick's routed-demand narrowing (ga-l7jdg).
//
// The controller's demand read is binding-only, so a routed bead left on a work
// leg is demanded by nothing and no seat is ever spawned for it. That is a
// migration defect rather than a demand bug — but only if somebody counts it.
// This lane already reads every leg's open corpus on its own cadence, so the
// count is free and the assumption "there is no routed work out there" becomes
// checkable instead of load-bearing.
func TestBackstopCountsRoutedWorkTheRuntimePlaneCannotSee(t *testing.T) {
	routed := func(id, assignee string) beads.Bead {
		return beads.Bead{
			ID:       id,
			Title:    id,
			Type:     "task",
			Assignee: assignee,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "pool/worker"},
		}
	}
	for _, tc := range []struct {
		name    string
		leg     planeLeg
		seed    beads.Bead
		want    int
		because string
	}{
		{
			name: "unassigned routed work on a work leg",
			leg:  planeLeg{label: "city"},
			seed: routed("ga-off-plane", ""),
			want: 1, because: "the tick's demand read refuses this leg, so nothing spawns for the bead",
		},
		{
			name: "the same bead on the binding",
			leg:  planeLeg{label: "class:gmnos", binding: true},
			seed: routed("gcg-on-plane", ""),
			want: 0, because: "the runtime plane reads the binding, so this bead IS demanded",
		},
		{
			name: "a routed bead on a work leg that already has a holder",
			leg:  planeLeg{label: "city"},
			seed: routed("ga-held", "worker-1"),
			want: 0, because: "an assigned bead needs no seat spawned for it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			if _, err := store.Create(tc.seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			leg := tc.leg
			leg.store = store
			report := newRouteRecoveryLane().backstopLeg(leg)
			if report.err != nil {
				t.Fatalf("backstop leg: %v", report.err)
			}
			if report.offPlaneRouted != tc.want {
				t.Fatalf("off_plane_routed = %d, want %d: %s", report.offPlaneRouted, tc.want, tc.because)
			}
		})
	}
}
