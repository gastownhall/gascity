package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Every fake below embeds *beads.MemStore and overrides one method to shape a
// specific race. These assertions exist because that override is silent when it
// drifts: if the interface signature changes, the override stops satisfying it,
// MemStore's own implementation gets promoted in its place, and every subtest
// quietly reroutes onto the other tier while still reporting green.
var (
	_ beads.ConditionalAssignmentReleaser = (*errReleaseStore)(nil)
	_ beads.ConditionalAssignmentReleaser = (*errGetStore)(nil)
	_ beads.ConditionalAssignmentReleaser = (*staleGetReleaseStore)(nil)
	_ beads.ConditionalAssignmentReleaser = (*failSecondWriteStore)(nil)
	_ beads.ConditionalAssignmentReleaser = (*clobberingReleaseStore)(nil)
)

// errReleaseStore fails the conditional release with a genuine backend error
// (not the unsupported sentinel), standing in for a bd sql failure or a JSON
// parse failure from BdStore.ReleaseIfCurrent.
type errReleaseStore struct {
	*beads.MemStore
	err error
}

func (s *errReleaseStore) ReleaseIfCurrent(string, string) (bool, error) {
	return false, s.err
}

// errGetStore fails the tier-2 pre-release verification read. It reports the
// conditional verb as unsupported so the release is forced onto tier 2. The
// verification is a LIVE List (not Get), because a cached Get would re-confirm
// the caller's stale snapshot against an equally stale cache.
type errGetStore struct {
	*beads.MemStore
	err  error
	fail bool
}

func (s *errGetStore) ReleaseIfCurrent(string, string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

func (s *errGetStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fail {
		return nil, s.err
	}
	return s.MemStore.List(q)
}

// TestReleaseWorkBead_PropagatesBackendFailures pins the failure DIRECTION for
// both tiers. A release that did not happen must not report success: callers
// count a nil error as a completed unassign, and repairStrandedPoolWorkerBead
// gates on that count before closing a session bead. Swallowing the error there
// closes the session bead while its work is still assigned to it, and nothing
// watches the work afterwards. That is worse than the unconditional write this
// guard replaced, which propagated its Update error.
func TestReleaseWorkBead_PropagatesBackendFailures(t *testing.T) {
	backendErr := errors.New("transient backend failure")

	t.Run("tier 1 conditional release error", func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &errReleaseStore{MemStore: mem, err: backendErr}
		claimed := seedClaimedBead(t, mem, "retired-session")

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		err := wa.ReleaseWorkBead(claimed, "")
		if err == nil {
			t.Fatal("ReleaseWorkBead returned nil after a backend failure; the caller will count this as a completed unassign")
		}
		if !errors.Is(err, backendErr) {
			t.Errorf("error = %v, want it to wrap %v", err, backendErr)
		}
		got, getErr := mem.Get(claimed.ID)
		if getErr != nil {
			t.Fatalf("Get: %v", getErr)
		}
		if got.Assignee != "retired-session" || got.Status != "in_progress" {
			t.Errorf("bead moved despite the failure: status=%q assignee=%q", got.Status, got.Assignee)
		}
	})

	t.Run("tier 2 verification read error", func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &errGetStore{MemStore: mem, err: backendErr}
		claimed := seedClaimedBead(t, mem, "retired-session")
		store.fail = true

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		err := wa.ReleaseWorkBead(claimed, "")
		if err == nil {
			t.Fatal("ReleaseWorkBead returned nil after the pre-release read failed; the caller will count this as a completed unassign")
		}
		if !errors.Is(err, backendErr) {
			t.Errorf("error = %v, want it to wrap %v", err, backendErr)
		}
	})
}

// TestReleaseWorkBead_FallbackRouteNeverRidesASecondWrite pins the tier-1
// bypass. ReleaseIfCurrent swaps only status/assignee, so a run_target stamped
// afterwards rides a separate write. In that gap the bead is open, unassigned,
// and carries neither run_target nor routed_to, which makes it invisible to the
// controller demand query and to the orphan sweep (which scans only ASSIGNED
// work); if the second write then fails, nothing revisits it. The release must
// therefore take the single-write path whenever a route has to be stamped, the
// same bypass releaseOrphanedPoolAssignment applies to continuation groups.
func TestReleaseWorkBead_FallbackRouteNeverRidesASecondWrite(t *testing.T) {
	t.Run("current snapshot uses one combined write", func(t *testing.T) {
		store := &failSecondWriteStore{MemStore: beads.NewMemStore()}
		claimed := seedClaimedBead(t, store, "retired-session")
		store.updateCalls = 0

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReleaseWorkBead(claimed, "worker"); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}

		if store.updateCalls != 1 {
			t.Fatalf("Update calls = %d, want 1 combined release write", store.updateCalls)
		}
		got, err := store.Get(claimed.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Assignee != "" || got.Status != "open" {
			t.Fatalf("release did not land: status=%q assignee=%q", got.Status, got.Assignee)
		}
		// The invariant: a released bead is never claimable without its route. On the
		// two-write path this bead would be open, unassigned, and unrouted.
		if got.Metadata[beadmeta.RunTargetMetadataKey] != "worker" {
			t.Errorf("run_target = %q, want %q — a released bead with no route is invisible to the demand query and to the orphan sweep",
				got.Metadata[beadmeta.RunTargetMetadataKey], "worker")
		}
	})

	t.Run("stale snapshot performs no write", func(t *testing.T) {
		store := &failSecondWriteStore{MemStore: beads.NewMemStore()}
		stale := seedReclaimedBead(t, store)
		store.updateCalls = 0

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReleaseWorkBead(stale, "worker"); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}

		if store.updateCalls != 0 {
			t.Fatalf("Update calls = %d, want 0 for a stale release snapshot", store.updateCalls)
		}
		got, err := store.Get(stale.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Assignee != "fresh-worker" || got.Status != "in_progress" {
			t.Errorf("status=%q assignee=%q, want in_progress/fresh-worker — the stale fallback-route release clobbered a live claim",
				got.Status, got.Assignee)
		}
	})
}

// staleGetReleaseStore answers Get from a stale snapshot while List(Live:true) tells
// the truth, which is the shape CachingStore has: Get serves a clone from the
// in-memory cache for a tracked, non-dirty bead. It reports the conditional verb
// as unsupported so the release is forced onto tier 2.
type staleGetReleaseStore struct {
	*beads.MemStore
	stale beads.Bead
}

func (s *staleGetReleaseStore) ReleaseIfCurrent(string, string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

func (s *staleGetReleaseStore) Get(id string) (beads.Bead, error) {
	if id == s.stale.ID {
		return s.stale, nil
	}
	return s.MemStore.Get(id)
}

// TestReleaseWorkBead_Tier2DoesNotTrustACachedRead is the cache half of the
// dr-huhn race. Verifying the caller's stale snapshot with a CACHED read
// re-confirms it against an equally stale cache and lets the unconditional write
// clobber a live claim, which is the original bug wearing a guard. The
// verification must read live.
func TestReleaseWorkBead_Tier2DoesNotTrustACachedRead(t *testing.T) {
	mem := beads.NewMemStore()
	store := &staleGetReleaseStore{MemStore: mem}
	// Live truth: a fresh worker holds it.
	claimed := seedClaimedBead(t, mem, "fresh-worker")
	// The caller's snapshot, and what a cached Get would still answer.
	stale := claimed
	stale.Assignee = "retired-session"
	store.stale = stale

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	if err := wa.ReleaseWorkBead(stale, ""); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}

	got, err := mem.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "fresh-worker" || got.Status != "in_progress" {
		t.Errorf("status=%q assignee=%q, want in_progress/fresh-worker — a cached verification read let the release clobber a live claim",
			got.Status, got.Assignee)
	}
}

// failSecondWriteStore implements the conditional verb but fails a
// METADATA-ONLY Update, which is exactly the second write of the tier-1 path.
// A combined status+assignee+metadata Update (the single-write tier-2 release)
// still succeeds. So a release that correctly bypasses tier 1 lands whole, and
// one that splits the write leaves the bead released but unrouted.
type failSecondWriteStore struct {
	*beads.MemStore
	updateCalls int
}

func (s *failSecondWriteStore) Update(id string, opts beads.UpdateOpts) error {
	s.updateCalls++
	if opts.Assignee == nil && opts.Status == nil && opts.Metadata != nil {
		return errors.New("metadata-only write failed")
	}
	return s.MemStore.Update(id, opts)
}

// clobberingReleaseStore simulates the dr-huhn race: the caller computed its
// release set from a snapshot, and between that snapshot and the release write
// a fresh worker re-claimed the bead. The store answers reads with the CURRENT
// (re-claimed) state, so a release that still writes unconditionally destroys
// the new owner's claim.
type clobberingReleaseStore struct {
	*beads.MemStore
	supportsConditional bool
}

func newClobberingReleaseStore(conditional bool) *clobberingReleaseStore {
	return &clobberingReleaseStore{MemStore: beads.NewMemStore(), supportsConditional: conditional}
}

// ReleaseIfCurrent hides MemStore's implementation when this store is standing
// in for a backend without the conditional verb, so the fallback path is
// exercised rather than the CAS fast path.
func (s *clobberingReleaseStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if !s.supportsConditional {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return s.MemStore.ReleaseIfCurrent(id, expectedAssignee)
}

// seedClaimedBead creates a bead and drives it to in_progress under owner.
// MemStore.Create hard-sets status to "open", so the claim must be a follow-up
// Update rather than a field on the create.
func seedClaimedBead(t *testing.T, store beads.Store, owner string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{Title: "work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inProgress, assignee := "in_progress", owner
	if err := store.Update(created.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("Update to claimed: %v", err)
	}
	claimed, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after claim: %v", err)
	}
	if claimed.Status != "in_progress" || claimed.Assignee != owner {
		t.Fatalf("seed failed: status=%q assignee=%q, want in_progress/%s", claimed.Status, claimed.Assignee, owner)
	}
	return claimed
}

// seedReclaimedBead creates a bead already owned by the fixture "fresh-worker"
// owner and returns the STALE snapshot (owned by the fixture "retired-session"
// owner) that a caller would be holding.
func seedReclaimedBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	stale := seedClaimedBead(t, store, "fresh-worker")
	stale.Assignee = "retired-session"
	return stale
}

// TestReleaseWorkBead_DoesNotClobberReclaimedAssignee is the dr-huhn regression:
// releasing against a stale snapshot must not clear an assignee that now belongs
// to a different, live worker. Asserted for both store shapes — with the atomic
// conditional verb (CAS fast path) and without it (recheck fallback).
func TestReleaseWorkBead_DoesNotClobberReclaimedAssignee(t *testing.T) {
	for _, tc := range []struct {
		name        string
		conditional bool
	}{
		{"conditional release available", true},
		{"conditional release unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newClobberingReleaseStore(tc.conditional)
			stale := seedReclaimedBead(t, store)

			wa := workAssignmentForStore(beads.WorkStore{Store: store})
			if err := wa.ReleaseWorkBead(stale, ""); err != nil {
				t.Fatalf("ReleaseWorkBead: %v", err)
			}

			got, err := store.Get(stale.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Assignee != "fresh-worker" {
				t.Errorf("assignee = %q, want %q — the release clobbered a re-claim it did not own", got.Assignee, "fresh-worker")
			}
			if got.Status != "in_progress" {
				t.Errorf("status = %q, want in_progress — the release reset a bead a live worker still holds", got.Status)
			}
		})
	}
}

// TestReleaseWorkBead_ReleasesWhenSnapshotStillCurrent guards the other
// direction: the no-clobber guard must not block the release it exists to
// perform. When the snapshot is still accurate the bead is released normally.
func TestReleaseWorkBead_ReleasesWhenSnapshotStillCurrent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		conditional bool
	}{
		{"conditional release available", true},
		{"conditional release unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newClobberingReleaseStore(tc.conditional)
			created := seedClaimedBead(t, store, "retired-session")

			wa := workAssignmentForStore(beads.WorkStore{Store: store})
			if err := wa.ReleaseWorkBead(created, ""); err != nil {
				t.Fatalf("ReleaseWorkBead: %v", err)
			}

			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Assignee != "" {
				t.Errorf("assignee = %q, want empty — a current snapshot must still release", got.Assignee)
			}
			if got.Status != "open" {
				t.Errorf("status = %q, want open — an in_progress release must reset to open", got.Status)
			}
		})
	}
}

// TestReassignWorkBead_DoesNotClobberReclaimedAssignee is the dr-huhn regression
// on the sibling write. reassignWorkAssignedToRetiredSession* walks the same
// OpenAssignedTo snapshot the release sweep walks (session_beads.go), so a
// reassign computed from it carries the identical lost-update hazard: a fresh
// worker claims the bead after the list, and an unconditional
// Update{Assignee:&successor} stamps the retired session's replacement over that
// live claim. There is no conditional-reassign verb, so the only guard is the
// live re-read — which is why this test exercises a plain MemStore rather than
// the two store shapes the release test needs.
func TestReassignWorkBead_DoesNotClobberReclaimedAssignee(t *testing.T) {
	store := beads.NewMemStore()
	stale := seedReclaimedBead(t, store)

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	if err := wa.ReassignWorkBead(stale, "successor-session"); err != nil {
		t.Fatalf("ReassignWorkBead: %v", err)
	}

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "fresh-worker" {
		t.Errorf("assignee = %q, want fresh-worker — a stale snapshot reassigned live work to the retired session's successor", got.Assignee)
	}
}

// TestReassignWorkBead_ReassignsWhenSnapshotStillCurrent is the other half: the
// guard must not turn every reassign into a no-op. Without this, deleting the
// write entirely would pass the clobber test above.
func TestReassignWorkBead_ReassignsWhenSnapshotStillCurrent(t *testing.T) {
	store := beads.NewMemStore()
	claimed := seedClaimedBead(t, store, "retired-session")

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	if err := wa.ReassignWorkBead(claimed, "successor-session"); err != nil {
		t.Fatalf("ReassignWorkBead: %v", err)
	}

	got, err := store.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "successor-session" {
		t.Errorf("assignee = %q, want successor-session — a current snapshot must still reassign", got.Assignee)
	}
}

// TestReassignWorkBead_PropagatesVerificationFailure pins the failure direction:
// a reassign whose verification read failed must not report success. The callers
// log per-bead failures, and a swallowed error there reads as a completed
// re-home of work that is in fact still on the retired session.
func TestReassignWorkBead_PropagatesVerificationFailure(t *testing.T) {
	backendErr := errors.New("transient backend failure")
	mem := beads.NewMemStore()
	claimed := seedClaimedBead(t, mem, "retired-session")
	store := &errGetStore{MemStore: mem, err: backendErr, fail: true}

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	err := wa.ReassignWorkBead(claimed, "successor-session")
	if !errors.Is(err, backendErr) {
		t.Fatalf("ReassignWorkBead err = %v, want wrapped %v", err, backendErr)
	}
}

// TestReleaseWorkBead_ContinuationGroupNeverRidesASecondWrite covers the other
// disjunct of singleWriteRequired. A bead carrying gc.continuation_group must be
// released in ONE write, because between a tier-1 conditional release and its
// follow-up metadata write the bead is open, unassigned, and still carrying the
// group — and `gc hook --claim` reads that group to vacuum the bead and its
// {root, group} siblings onto the claiming session. The fallback-route disjunct
// is covered separately; this one has no route to stamp, so only the group
// forces the single write.
func TestReleaseWorkBead_ContinuationGroupNeverRidesASecondWrite(t *testing.T) {
	t.Run("current snapshot uses one combined write", func(t *testing.T) {
		store := &failSecondWriteStore{MemStore: beads.NewMemStore()}
		claimed := seedClaimedBead(t, store, "retired-session")
		if err := store.SetMetadata(claimed.ID, beadmeta.ContinuationGroupMetadataKey, "grp-1"); err != nil {
			t.Fatalf("seed continuation group: %v", err)
		}
		claimed.Metadata = map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"}
		store.updateCalls = 0

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		// runTargetFallback is empty, so stampFallbackRoute is false and the group is
		// the only thing that can force the single-write path.
		if err := wa.ReleaseWorkBead(claimed, ""); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}

		if store.updateCalls != 1 {
			t.Fatalf("Update calls = %d, want 1 combined release write", store.updateCalls)
		}
		got, err := store.Get(claimed.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Assignee != "" || got.Status != "open" {
			t.Fatalf("release did not land: status=%q assignee=%q", got.Status, got.Assignee)
		}
		// The invariant: the bead is never claimable while still carrying the group.
		// On the two-write path the group would survive the failing second write.
		if g := strings.TrimSpace(got.Metadata[beadmeta.ContinuationGroupMetadataKey]); g != "" {
			t.Errorf("continuation group = %q, want cleared — an open, unassigned bead still carrying its group lets a concurrent claim vacuum its siblings through it", g)
		}
	})

	t.Run("stale snapshot performs no write", func(t *testing.T) {
		store := &failSecondWriteStore{MemStore: beads.NewMemStore()}
		stale := seedReclaimedBead(t, store)
		if err := store.SetMetadata(stale.ID, beadmeta.ContinuationGroupMetadataKey, "grp-1"); err != nil {
			t.Fatalf("seed continuation group: %v", err)
		}
		stale.Metadata = map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"}
		store.updateCalls = 0

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReleaseWorkBead(stale, ""); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}

		if store.updateCalls != 0 {
			t.Fatalf("Update calls = %d, want 0 for a stale release snapshot", store.updateCalls)
		}
		got, err := store.Get(stale.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Assignee != "fresh-worker" || got.Status != "in_progress" {
			t.Errorf("status=%q assignee=%q, want in_progress/fresh-worker — the stale continuation-group release clobbered a live claim",
				got.Status, got.Assignee)
		}
	})
}
