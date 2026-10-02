package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func TestWorkReleaseConditionalWritesPreservesNewIncarnation(t *testing.T) {
	for name, release := range map[string]func(beads.Store, beads.Bead) error{
		"retired session": func(store beads.Store, snapshot beads.Bead) error {
			return workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(snapshot, "")
		},
		"orphaned pool": func(store beads.Store, snapshot beads.Bead) error {
			if releaseOrphanedPoolAssignment(store, snapshot, true) {
				return errors.New("released a newer claim")
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
				ScopeRoot:         t.TempDir(),
				Provider:          "file",
				ConditionalWrites: gate.Require,
				OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			store := result.Store
			stale := seedClaimedBead(t, store, "reused-worker")
			if err := store.Update(stale.ID, beads.UpdateOpts{
				Status: stringPtr("open"), Assignee: stringPtr(""),
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Update(stale.ID, beads.UpdateOpts{
				Status:   stringPtr("in_progress"),
				Assignee: stringPtr("reused-worker"),
				Metadata: map[string]string{
					beadmeta.SessionIDMetadataKey:         "successor-session",
					beadmeta.ContinuationGroupMetadataKey: "successor-group",
				},
			}); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(stale.ID)
			if err != nil {
				t.Fatal(err)
			}
			releaseErr := release(store, stale)
			if name == "retired session" {
				if !beads.IsPreconditionFailed(releaseErr) {
					t.Errorf("release error=%v, want a revision conflict", releaseErr)
				}
			} else if releaseErr != nil {
				t.Errorf("release: %v", releaseErr)
			}
			after, err := store.Get(stale.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("stale release changed a successor using the same assignee:\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

type revisionReleaseStore struct {
	*beads.MemStore
	conditionalCalls int
	legacyCalls      int
	writeErr         error
}

var _ beads.ConditionalWriter = (*revisionReleaseStore)(nil)

func (s *revisionReleaseStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	s.conditionalCalls++
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func (s *revisionReleaseStore) Update(id string, opts beads.UpdateOpts) error {
	s.legacyCalls++
	return s.MemStore.Update(id, opts)
}

func (s *revisionReleaseStore) ReleaseIfCurrent(id, assignee string) (bool, error) {
	s.legacyCalls++
	return s.MemStore.ReleaseIfCurrent(id, assignee)
}

func openRevisionReleaseStore(t *testing.T, mode gate.Mode) *revisionReleaseStore {
	t.Helper()
	store := &revisionReleaseStore{MemStore: beads.NewMemStore()}
	if _, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: mode,
		OpenFileStore:     func() (beads.Store, error) { return store, nil },
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestWorkReleaseConditionalWritesAtomicCleanup(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
		for _, orphan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/orphan=%t", mode, orphan), func(t *testing.T) {
				store := openRevisionReleaseStore(t, mode)
				claimed := seedClaimedBead(t, store, "retired-session")
				if err := store.Update(claimed.ID, beads.UpdateOpts{Metadata: map[string]string{
					beadmeta.SessionAffinityMetadataKey:   "retired-session",
					beadmeta.ContinuationGroupMetadataKey: "old-group",
					detachedProbeMetadataKey:              "old-probe",
					"unrelated":                           "preserve",
				}}); err != nil {
					t.Fatal(err)
				}
				snapshot, err := store.Get(claimed.ID)
				if err != nil {
					t.Fatal(err)
				}
				store.legacyCalls = 0
				if orphan {
					if !releaseOrphanedPoolAssignment(store, snapshot, true) {
						t.Fatal("orphan release failed")
					}
				} else if err := workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(snapshot, "worker"); err != nil {
					t.Fatal(err)
				}
				if store.conditionalCalls != 1 || store.legacyCalls != 0 {
					t.Fatalf("conditional=%d legacy=%d, want one combined conditional write", store.conditionalCalls, store.legacyCalls)
				}
				got, err := store.Get(claimed.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Assignee != "" || got.Status != "open" || got.Metadata["unrelated"] != "preserve" {
					t.Fatalf("incorrect release: %+v", got)
				}
				for key := range clearedSessionAffinityMetadata() {
					if got.Metadata[key] != "" {
						t.Errorf("%s not cleared: %q", key, got.Metadata[key])
					}
				}
				if orphan && got.Metadata[detachedProbeMetadataKey] != "" {
					t.Error("detached probe not cleared")
				}
				if !orphan && (got.Metadata[beadmeta.RunTargetMetadataKey] != "worker" || got.Metadata[detachedProbeMetadataKey] != "old-probe") {
					t.Error("fallback route or detached probe changed incorrectly")
				}
			})
		}
	}
}

func TestWorkReleaseConditionalWritesNeverFallsBackAfterFailure(t *testing.T) {
	for _, writeErr := range []error{
		errors.New("backend unavailable"),
		beads.ErrConditionalWriteUnsupported,
	} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			store := openRevisionReleaseStore(t, gate.Require)
			snapshot := seedClaimedBead(t, store, "retired-session")
			store.legacyCalls = 0
			store.writeErr = writeErr
			err := workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(snapshot, "")
			if !errors.Is(err, writeErr) {
				t.Fatalf("error=%v, want %v", err, writeErr)
			}
			if releaseOrphanedPoolAssignment(store, snapshot, true) {
				t.Fatal("reported a failed orphan release as successful")
			}
			if store.legacyCalls != 0 || store.conditionalCalls != 2 {
				t.Fatalf("conditional=%d legacy=%d, want no fallback", store.conditionalCalls, store.legacyCalls)
			}
			got, err := store.Get(snapshot.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, snapshot) {
				t.Fatalf("failed release changed the bead: %+v", got)
			}
		})
	}
}

func TestWorkReleaseConditionalWritesConflictDoesNotAcknowledgeRelease(t *testing.T) {
	store := openRevisionReleaseStore(t, gate.Require)
	snapshot := seedClaimedBead(t, store, "retired-session")
	if err := store.Update(snapshot.ID, beads.UpdateOpts{Metadata: map[string]string{
		"progress": "updated since the release snapshot",
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.legacyCalls = 0
	err = workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(snapshot, "")
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("error=%v, want a conflict so the caller cannot count this as a released assignment", err)
	}
	if store.legacyCalls != 0 {
		t.Fatalf("legacy writes=%d, want no fallback", store.legacyCalls)
	}
	after, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("conflict changed the still-owned bead: %+v", after)
	}
}

func TestWorkReleaseConditionalWritesRequireRefusesUnsafeSnapshots(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("capability_unavailable=%t", unavailable), func(t *testing.T) {
			store := openRevisionReleaseStore(t, gate.Require)
			before := seedClaimedBead(t, store, "retired-session")
			snapshot := before
			if unavailable {
				store.DisableConditionalWrites = true
			} else {
				snapshot.Revision = 0
			}
			store.legacyCalls = 0
			err := workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(snapshot, "")
			if err == nil {
				t.Fatal("unsafe release did not return an error")
			}
			if unavailable && !beads.IsConditionalWritesRequired(err) {
				t.Fatalf("want required-capability refusal, got %v", err)
			}
			if !unavailable && !strings.Contains(err.Error(), "snapshot has no revision") {
				t.Fatalf("want missing-revision refusal, got %v", err)
			}
			if releaseOrphanedPoolAssignment(store, snapshot, true) {
				t.Fatal("unsafe orphan release reported success")
			}
			if store.conditionalCalls != 0 || store.legacyCalls != 0 {
				t.Fatalf("conditional=%d legacy=%d, want no writes", store.conditionalCalls, store.legacyCalls)
			}
			got, err := store.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, before) {
				t.Fatalf("unsafe release changed the bead: %+v", got)
			}
		})
	}
}

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

// errGetStore fails the live re-read before a fenced write: the Get that picks
// the revision a tier-2 release or a reassign is fenced on. It reports the
// conditional verb as unsupported so the release is forced onto tier 2.
type errGetStore struct {
	*beads.MemStore
	err  error
	fail bool
}

func (s *errGetStore) ReleaseIfCurrent(string, string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

func (s *errGetStore) Get(id string) (beads.Bead, error) {
	if s.fail {
		return beads.Bead{}, s.err
	}
	return s.MemStore.Get(id)
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
			t.Fatalf("write calls = %d, want 1 combined release write", store.updateCalls)
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
			t.Fatalf("write calls = %d, want 0 for a stale release snapshot", store.updateCalls)
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

// staleGetReleaseStore answers Get from a stale snapshot while the store itself
// has moved on, which is the shape a cache that missed an out-of-process write
// has. It reports the conditional verb as unsupported so the release is forced
// onto tier 2.
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
// dr-huhn race. A re-read that is as stale as the caller's snapshot confirms
// it, which let the old unconditional write clobber a live claim: the original
// bug wearing a guard. Any read may be stale, so the fence is at the write:
// the release is fenced on the stale read's revision, the fresh claim moved
// it, and nothing is written. The release reports an error because a lost
// fence cannot tell a claim from an unrelated write, and the bead may still be
// assigned to the retired session.
func TestReleaseWorkBead_Tier2DoesNotTrustACachedRead(t *testing.T) {
	mem := beads.NewMemStore()
	store := &staleGetReleaseStore{MemStore: mem}
	// The caller's snapshot, and what a stale Get still answers.
	stale := seedClaimedBead(t, mem, "retired-session")
	store.stale = stale
	// Live truth: a fresh worker claimed it since.
	fresh := "fresh-worker"
	if err := mem.Update(stale.ID, beads.UpdateOpts{Assignee: &fresh}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	if err := wa.ReleaseWorkBead(stale, ""); err == nil {
		t.Fatal("ReleaseWorkBead = nil after losing its fence; a caller gating a close on it would close over the bead")
	}

	got, err := mem.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != "fresh-worker" || got.Status != "in_progress" {
		t.Errorf("status=%q assignee=%q, want in_progress/fresh-worker — a cached verification read let the release clobber a live claim",
			got.Status, got.Assignee)
	}
}

// failSecondWriteStore implements the conditional verb but fails a
// METADATA-ONLY write, which is exactly the second write of the tier-1 path.
// A combined status+assignee+metadata write (the single-write tier-2 release,
// fenced through UpdateIfMatch) still succeeds. So a release that correctly
// bypasses tier 1 lands whole, and one that splits the write leaves the bead
// released but unrouted. updateCalls counts plain and fenced writes alike.
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

func (s *failSecondWriteStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	s.updateCalls++
	if opts.Assignee == nil && opts.Status == nil && opts.Metadata != nil {
		return errors.New("metadata-only write failed")
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
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
// live claim. The reassign is fenced on the snapshot like a tier-2 release;
// here the re-read already sees the claim, so nothing is written.
// TestReassignWorkBead_FencesTheWriteOnTheSnapshot covers a claim that lands
// after the re-read.
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

// reclaimOnce moves a bead to "fresh-worker" the first time it is applied. A
// store fake applies it immediately before its write, which is the window
// between a reassign's snapshot (and any re-read it makes) and the write.
type reclaimOnce struct {
	done bool
}

func (r *reclaimOnce) apply(mem *beads.MemStore, id string) error {
	if r.done {
		return nil
	}
	r.done = true
	fresh := "fresh-worker"
	return mem.Update(id, beads.UpdateOpts{Assignee: &fresh})
}

// reclaimBeforeWriteStore is a revision-fenced store (MemStore's UpdateIfMatch)
// on which a fresh worker re-claims the bead just before the reassign's write.
type reclaimBeforeWriteStore struct {
	*beads.MemStore
	reclaim reclaimOnce
}

func (s *reclaimBeforeWriteStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.reclaim.apply(s.MemStore, id); err != nil {
		return err
	}
	return s.MemStore.Update(id, opts)
}

func (s *reclaimBeforeWriteStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if err := s.reclaim.apply(s.MemStore, id); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// guardedReassignStore stands in for BdStore with `bd update --if-status
// --if-assignee`: it has the guarded update and no revision fence (embedding
// only beads.Store hides MemStore's UpdateIfMatch). When reclaim is armed, a
// fresh worker re-claims the bead just before each write. guarded records the
// guarded updates' options.
type guardedReassignStore struct {
	beads.Store
	mem     *beads.MemStore
	armed   bool
	reclaim reclaimOnce
	guarded []beads.UpdateOpts
}

func (s *guardedReassignStore) Update(id string, opts beads.UpdateOpts) error {
	if s.armed {
		if err := s.reclaim.apply(s.mem, id); err != nil {
			return err
		}
	}
	return s.mem.Update(id, opts)
}

func (s *guardedReassignStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	s.guarded = append(s.guarded, opts)
	if s.armed {
		if err := s.reclaim.apply(s.mem, id); err != nil {
			return false, err
		}
	}
	current, err := s.mem.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.Status != expectedStatus || strings.TrimSpace(current.Assignee) != expectedAssignee {
		return false, nil
	}
	return true, s.mem.Update(id, opts)
}

// incapableWorkStore has neither a guarded update nor a revision fence:
// embedding only beads.Store hides every optional capability of the store
// it wraps.
type incapableWorkStore struct {
	beads.Store
}

var (
	_ beads.ConditionalWriter        = (*reclaimBeforeWriteStore)(nil)
	_ beads.AssignmentGuardedUpdater = (*guardedReassignStore)(nil)
)

// TestReassignWorkBead_FencesTheWriteOnTheSnapshot is the regression for a
// reassign that re-checked the snapshot and then wrote blind (mc-zndi7.66). A
// fresh worker that re-claims the bead after the re-check had its claim
// overwritten with the retired session's successor. The write is now fenced
// the way ReleaseWorkBead's tier 2 fences a release: the guarded update checks
// status and assignee in the write, a revision-fenced store re-reads and
// writes on that read's revision, and a store with neither is refused. In
// every case the re-claim survives.
func TestReassignWorkBead_FencesTheWriteOnTheSnapshot(t *testing.T) {
	t.Run("guarded update, re-claimed before the write", func(t *testing.T) {
		mem := beads.NewMemStore()
		claimed := seedClaimedBead(t, mem, "retired-session")
		store := &guardedReassignStore{Store: mem, mem: mem, armed: true}

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReassignWorkBead(claimed, "successor-session"); err != nil {
			t.Fatalf("ReassignWorkBead: %v (a guard miss means the bead moved on, not a failure)", err)
		}
		if len(store.guarded) != 1 {
			t.Fatalf("guarded updates = %d, want 1", len(store.guarded))
		}
		assertAssignee(t, mem, claimed.ID, "fresh-worker")
	})

	t.Run("guarded update, current snapshot writes only the assignee", func(t *testing.T) {
		mem := beads.NewMemStore()
		claimed := seedClaimedBead(t, mem, "retired-session")
		store := &guardedReassignStore{Store: mem, mem: mem}

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReassignWorkBead(claimed, "successor-session"); err != nil {
			t.Fatalf("ReassignWorkBead: %v", err)
		}
		if len(store.guarded) != 1 {
			t.Fatalf("guarded updates = %d, want 1", len(store.guarded))
		}
		if opts := store.guarded[0]; derefStr(opts.Assignee) != "successor-session" || opts.Status != nil || opts.Metadata != nil {
			t.Fatalf("guarded update = %#v, want only Assignee=successor-session", opts)
		}
		assertAssignee(t, mem, claimed.ID, "successor-session")
	})

	t.Run("revision fence, re-claimed after the re-read", func(t *testing.T) {
		store := &reclaimBeforeWriteStore{MemStore: beads.NewMemStore()}
		claimed := seedClaimedBead(t, store.MemStore, "retired-session")

		wa := workAssignmentForStore(beads.WorkStore{Store: store})
		if err := wa.ReassignWorkBead(claimed, "successor-session"); err == nil {
			t.Fatal("ReassignWorkBead returned nil after losing the revision fence; the bead may still be on the retired session")
		}
		assertAssignee(t, store.MemStore, claimed.ID, "fresh-worker")
	})

	t.Run("store with neither is refused", func(t *testing.T) {
		mem := beads.NewMemStore()
		claimed := seedClaimedBead(t, mem, "retired-session")

		wa := workAssignmentForStore(beads.WorkStore{Store: incapableWorkStore{Store: mem}})
		err := wa.ReassignWorkBead(claimed, "successor-session")
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("ReassignWorkBead err = %v, want wrapped %v", err, beads.ErrConditionalWriteUnsupported)
		}
		assertAssignee(t, mem, claimed.ID, "retired-session")
	})
}

// TestReassignWorkAssignedToRetiredSessionBeadLogsAndContinuesPastARefusal pins
// the callers' side of a refused reassign: each failure is logged and the
// sweep moves on to the next bead, which stays with the retired identity until
// a later pass.
func TestReassignWorkAssignedToRetiredSessionBeadLogsAndContinuesPastARefusal(t *testing.T) {
	mem := beads.NewMemStore()
	first := seedClaimedBead(t, mem, "retired-session")
	second := seedClaimedBead(t, mem, "retired-session")

	var stderr strings.Builder
	reassignWorkAssignedToRetiredSessionBead("", nil, incapableWorkStore{Store: mem}, nil, beads.Bead{ID: "retired-session"}, "successor-session", &stderr)

	for _, id := range []string{first.ID, second.ID} {
		if !strings.Contains(stderr.String(), "reassigning work "+id+" from retired session retired-session") {
			t.Errorf("stderr does not log the refused reassign of %s:\n%s", id, stderr.String())
		}
		assertAssignee(t, mem, id, "retired-session")
	}
}

func assertAssignee(t *testing.T, store beads.Store, id, want string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Assignee != want {
		t.Fatalf("assignee of %s = %q, want %q", id, got.Assignee, want)
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
			t.Fatalf("write calls = %d, want 1 combined release write", store.updateCalls)
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
			t.Fatalf("write calls = %d, want 0 for a stale release snapshot", store.updateCalls)
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
