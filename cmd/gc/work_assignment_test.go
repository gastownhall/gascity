package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// recordingWorkStore embeds a MemStore (so it satisfies the full beads.Store
// interface) and records every List/Ready query the work-assignment façade
// emits, while delegating to the MemStore for real results. It deliberately
// does NOT implement CachedList or Backing(): the façade's optional-capability
// fast-paths must degrade to the plain List/Ready calls, exactly as a
// non-caching store does today.
type recordingWorkStore struct {
	*beads.MemStore
	listQueries  []beads.ListQuery
	readyQueries []beads.ReadyQuery
}

func newRecordingWorkStore() *recordingWorkStore {
	return &recordingWorkStore{MemStore: beads.NewMemStore()}
}

func (s *recordingWorkStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listQueries = append(s.listQueries, q)
	return s.MemStore.List(q)
}

func (s *recordingWorkStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	if len(q) > 0 {
		s.readyQueries = append(s.readyQueries, q[0])
	} else {
		s.readyQueries = append(s.readyQueries, beads.ReadyQuery{})
	}
	return s.MemStore.Ready(q...)
}

// TestWorkAssignmentOpenAssignedTo_ByteIdenticalQuery asserts the façade's
// OpenAssignedTo emits the exact same ListQuery the raw probe ran:
// {Assignee,Status,Live,TierMode}, against the WORK store.
func TestWorkAssignmentOpenAssignedTo_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.OpenAssignedTo("agent-1", "in_progress", beads.TierBoth, true); err != nil {
		t.Fatalf("OpenAssignedTo: %v", err)
	}

	want := beads.ListQuery{Assignee: "agent-1", Status: "in_progress", Live: true, TierMode: beads.TierBoth}
	if len(rec.listQueries) != 1 {
		t.Fatalf("expected exactly 1 List call, got %d: %#v", len(rec.listQueries), rec.listQueries)
	}
	if !reflect.DeepEqual(rec.listQueries[0], want) {
		t.Fatalf("List query mismatch:\n got  %#v\n want %#v", rec.listQueries[0], want)
	}
}

// TestWorkAssignmentReadyAssignedTo_ByteIdenticalQuery asserts ReadyAssignedTo
// emits the same ReadyQuery{Assignee,TierMode} the raw beads.ReadyLive probe ran.
func TestWorkAssignmentReadyAssignedTo_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.ReadyAssignedTo("agent-2", beads.TierWisps); err != nil {
		t.Fatalf("ReadyAssignedTo: %v", err)
	}

	want := beads.ReadyQuery{Assignee: "agent-2", TierMode: beads.TierWisps}
	if len(rec.readyQueries) != 1 {
		t.Fatalf("expected exactly 1 Ready call, got %d: %#v", len(rec.readyQueries), rec.readyQueries)
	}
	if !reflect.DeepEqual(rec.readyQueries[0], want) {
		t.Fatalf("Ready query mismatch:\n got  %#v\n want %#v", rec.readyQueries[0], want)
	}
}

// TestWorkAssignmentCachedOpenAssignedWisps_NoFastPathWithoutCache asserts that
// on a store without the CachedList capability the cache probe reports "not
// answered" (so the caller falls through to OpenAssignedTo), and crucially does
// NOT assert CachedList on the WorkStore wrapper (which would always fail and
// silently drop the cache on a real caching store).
func TestWorkAssignmentCachedOpenAssignedWisps_NoFastPathWithoutCache(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if items, ok := wa.CachedOpenAssignedWisps("agent-3", "open"); ok {
		t.Fatalf("expected cache miss on non-caching store, got ok=true items=%#v", items)
	}
	if len(rec.listQueries) != 0 {
		t.Fatalf("cache probe must not issue a List on a non-caching store, got %#v", rec.listQueries)
	}
}

// fakeCachingWorkStore implements the CachedList capability on the underlying
// store (not the wrapper) so the façade's fast-path can be exercised.
type fakeCachingWorkStore struct {
	*beads.MemStore
	cachedCalls []beads.ListQuery
	cachedHit   []beads.Bead
	cachedOK    bool
}

func (s *fakeCachingWorkStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	s.cachedCalls = append(s.cachedCalls, q)
	return s.cachedHit, s.cachedOK
}

// TestWorkAssignmentCachedOpenAssignedWisps_UsesUnwrappedStore proves the
// CachedList assertion is made on the embedded .Store, not the WorkStore
// wrapper: the wrapper does not promote CachedList, so asserting on it would
// miss this capability and silently lose the fast-path (the typed-nil trap).
func TestWorkAssignmentCachedOpenAssignedWisps_UsesUnwrappedStore(t *testing.T) {
	want := beads.ListQuery{Assignee: "agent-4", Status: "in_progress", TierMode: beads.TierWisps}
	cache := &fakeCachingWorkStore{
		MemStore:  beads.NewMemStore(),
		cachedHit: []beads.Bead{{ID: "w-1"}},
		cachedOK:  true,
	}
	wa := workAssignmentForStore(beads.WorkStore{Store: cache})

	items, ok := wa.CachedOpenAssignedWisps("agent-4", "in_progress")
	if !ok {
		t.Fatalf("expected cache hit via unwrapped .Store, got ok=false")
	}
	if len(items) != 1 || items[0].ID != "w-1" {
		t.Fatalf("unexpected cached items: %#v", items)
	}
	if len(cache.cachedCalls) != 1 || !reflect.DeepEqual(cache.cachedCalls[0], want) {
		t.Fatalf("CachedList query mismatch:\n got %#v\n want %#v", cache.cachedCalls, want)
	}
}

// TestWorkAssignmentForStore_NilUnderlyingStoreSafe asserts the façade tolerates
// a nil underlying store the same way the raw probes did (return empty, no
// panic).
func TestWorkAssignmentForStore_NilUnderlyingStoreSafe(t *testing.T) {
	wa := workAssignmentForStore(beads.WorkStore{Store: nil})
	if items, err := wa.OpenAssignedTo("a", "open", beads.TierBoth, true); err != nil || items != nil {
		t.Fatalf("nil store OpenAssignedTo: items=%#v err=%v", items, err)
	}
	if items, err := wa.ReadyAssignedTo("a", beads.TierIssues); err != nil || items != nil {
		t.Fatalf("nil store ReadyAssignedTo: items=%#v err=%v", items, err)
	}
	if items, ok := wa.CachedOpenAssignedWisps("a", "open"); ok || items != nil {
		t.Fatalf("nil store CachedOpenAssignedWisps: items=%#v ok=%v", items, ok)
	}
}

// TestWorkAssignmentHasNonSessionWork_SkipsMail pins the gate half of the mail exemption:
// excludeMailMessageBeads filters mail only at the OpenAssignedTo
// sources, but ReadyAssignedTo and the CachedOpenAssignedWisps fast path hand
// HasNonSessionWork raw results. A session whose only assigned bead is its own
// unread mail must not read as holding work.
func TestWorkAssignmentHasNonSessionWork_SkipsMail(t *testing.T) {
	wa := workAssignmentForStore(beads.WorkStore{Store: beads.NewMemStore()})
	mail := beads.Bead{ID: "m-1", Type: "message", Assignee: "worker"}
	task := beads.Bead{ID: "t-1", Type: "task", Assignee: "worker"}

	if wa.HasNonSessionWork([]beads.Bead{mail}) {
		t.Fatal("HasNonSessionWork(mail only) = true, want false: mail is not work")
	}
	if !wa.HasNonSessionWork([]beads.Bead{mail, task}) {
		t.Fatal("HasNonSessionWork(mail + task) = false, want true")
	}
}

// parkedBdStore stands in for a BdStore over bd's raw statuses. mem holds the
// raw status; every read folds all but in_progress and closed to "open" and
// keeps the raw one in SourceStatus (mapBdStatus); a list of a status matches
// the raw one, as `bd list --status` does, so a Live "open" list omits a
// blocked row; and the guarded update compares the raw status, as
// `bd update --if-status --if-assignee` does. Its rows carry no revision (bd's
// lists publish none) and it has no revision fence: embedding only
// beads.Store hides MemStore's UpdateIfMatch. guarded records every guarded
// update; writes counts every write that landed.
type parkedBdStore struct {
	beads.Store
	mem     *beads.MemStore
	guarded []parkedGuardedCall
	writes  int
}

// parkedGuardedCall is one UpdateIfAssignment a parkedBdStore received.
type parkedGuardedCall struct {
	status, assignee string
	opts             beads.UpdateOpts
}

var _ beads.AssignmentGuardedUpdater = (*parkedBdStore)(nil)

func newParkedBdStore() *parkedBdStore {
	mem := beads.NewMemStore()
	return &parkedBdStore{Store: mem, mem: mem}
}

// foldBdStatus is mapBdStatus' read of b: its raw status kept in
// SourceStatus, its Status folded to Gas City's three.
func foldBdStatus(b beads.Bead) beads.Bead {
	b.SourceStatus = b.Status
	if b.Status != "in_progress" && b.Status != "closed" {
		b.Status = "open"
	}
	b.Revision = 0
	return b
}

func (s *parkedBdStore) Get(id string) (beads.Bead, error) {
	b, err := s.mem.Get(id)
	if err != nil {
		return b, err
	}
	return foldBdStatus(b), nil
}

func (s *parkedBdStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	items, err := s.mem.List(q)
	for i := range items {
		items[i] = foldBdStatus(items[i])
	}
	return items, err
}

func (s *parkedBdStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.mem.Update(id, opts); err != nil {
		return err
	}
	s.writes++
	return nil
}

func (s *parkedBdStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	s.guarded = append(s.guarded, parkedGuardedCall{status: expectedStatus, assignee: expectedAssignee, opts: opts})
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
	return true, s.Update(id, opts)
}

// parkedCASStore is a parkedBdStore's folded reads over a store with a
// revision fence and no guarded update: releaseAssignmentFenced's tier 2,
// whose live re-read returns the folded row.
type parkedCASStore struct {
	beads.Store
	bd *parkedBdStore
}

var _ beads.ConditionalWriter = (*parkedCASStore)(nil)

func (s *parkedCASStore) Get(id string) (beads.Bead, error) {
	b, err := s.bd.Get(id)
	if err != nil {
		return b, err
	}
	raw, _ := s.bd.mem.Get(id)
	b.Revision = raw.Revision
	return b, nil
}

func (s *parkedCASStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.bd.mem.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	s.bd.writes++
	return nil
}

func (s *parkedCASStore) CloseIfMatch(id string, revision int64) error {
	return s.bd.mem.CloseIfMatch(id, revision)
}

func (s *parkedCASStore) DeleteIfMatch(id string, revision int64) error {
	return s.bd.mem.DeleteIfMatch(id, revision)
}

func (s *parkedCASStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return s.bd.mem.CompareAndSetMetadataKey(id, key, expected, next)
}

// seedParkedClaim creates a work bead in mem held by assignee, then parks it
// in raw status, as a worker that ran `gc bd update --status=blocked` after
// its claim does.
func seedParkedClaim(t *testing.T, mem *beads.MemStore, assignee, status string, metadata map[string]string) beads.Bead {
	t.Helper()
	b, err := mem.Create(beads.Bead{Title: "parked claim", Assignee: assignee, Metadata: metadata})
	if err != nil {
		t.Fatalf("create claim: %v", err)
	}
	if err := mem.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("park claim as %s: %v", status, err)
	}
	b, err = mem.Get(b.ID)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	return b
}

// assertParkedRelease checks that id is unassigned, still in raw status
// want, and carries no session affinity.
func assertParkedRelease(t *testing.T, mem *beads.MemStore, id, want string) {
	t.Helper()
	got, err := mem.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if got.Assignee != "" {
		t.Errorf("%s assignee = %q, want cleared", id, got.Assignee)
	}
	if got.Status != want {
		t.Errorf("%s status = %q, want %q kept", id, got.Status, want)
	}
	for key := range clearedSessionAffinityMetadata() {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s metadata %s = %q, want cleared", id, key, v)
		}
	}
}

// TestReleaseAssignmentFencedParkedClaimFencesOnRecordedStatus is #7685's
// fence: a claim its worker parked as blocked reads "open" through bd's fold,
// and a release fenced on that folded status never matched its own row
// (`bd update --if-status open` on a blocked bead), so it was skipped as
// "assignment changed" on every pass. The fence is the status the store
// recorded, and the release writes no status: the hold stays.
func TestReleaseAssignmentFencedParkedClaimFencesOnRecordedStatus(t *testing.T) {
	t.Run("guarded update fences on the raw status", func(t *testing.T) {
		store := newParkedBdStore()
		raw := seedParkedClaim(t, store.mem, "dead-session", "blocked", nil)
		snapshot := foldBdStatus(raw)

		outcome, err := releaseAssignmentFenced(store, snapshot, beads.UpdateOpts{Assignee: stringPtr("")})
		if err != nil || outcome != fencedReleaseApplied {
			t.Fatalf("releaseAssignmentFenced = %v, %v; want applied", outcome, err)
		}
		if len(store.guarded) != 1 || store.guarded[0].status != "blocked" || store.guarded[0].assignee != "dead-session" {
			t.Fatalf("guarded updates = %+v, want one expecting (blocked, dead-session)", store.guarded)
		}
	})

	t.Run("ReleaseWorkBead writes no status", func(t *testing.T) {
		store := newParkedBdStore()
		raw := seedParkedClaim(t, store.mem, "dead-session", "blocked", map[string]string{beadmeta.SessionIDMetadataKey: "dead-session"})

		if err := workAssignmentForStore(beads.WorkStore{Store: store}).ReleaseWorkBead(foldBdStatus(raw), ""); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}
		if len(store.guarded) != 1 || store.guarded[0].status != "blocked" || store.guarded[0].opts.Status != nil {
			t.Fatalf("guarded updates = %+v, want one expecting blocked that writes no status", store.guarded)
		}
		assertParkedRelease(t, store.mem, raw.ID, "blocked")
	})

	t.Run("an event's deferred shape fences on deferred", func(t *testing.T) {
		store := newParkedBdStore()
		raw := seedParkedClaim(t, store.mem, "dead-holder", "deferred", nil)
		snapshot := raw
		snapshot.Status, snapshot.IndefinitelyDeferred = "open", true

		outcome, err := releaseAssignmentFenced(store, snapshot, beads.UpdateOpts{Assignee: stringPtr("")})
		if err != nil || outcome != fencedReleaseApplied {
			t.Fatalf("releaseAssignmentFenced = %v, %v; want applied", outcome, err)
		}
		if len(store.guarded) != 1 || store.guarded[0].status != "deferred" || store.guarded[0].assignee != "dead-holder" {
			t.Fatalf("guarded updates = %+v, want one expecting deferred", store.guarded)
		}
		assertParkedRelease(t, store.mem, raw.ID, "deferred")
	})

	t.Run("conditional writer re-reads the folded row", func(t *testing.T) {
		bd := newParkedBdStore()
		store := &parkedCASStore{Store: bd.mem, bd: bd}
		raw := seedParkedClaim(t, bd.mem, "dead-session", "blocked", nil)

		outcome, err := releaseAssignmentFenced(store, foldBdStatus(raw), beads.UpdateOpts{Assignee: stringPtr("")})
		if err != nil || outcome != fencedReleaseApplied {
			t.Fatalf("releaseAssignmentFenced = %v, %v; want applied", outcome, err)
		}
		assertParkedRelease(t, bd.mem, raw.ID, "blocked")
	})
}
