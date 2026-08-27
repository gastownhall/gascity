package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// v59OpaqueRevisionTokens are row_lock values the ga-f7v2ft v59 journey read off
// live bd session rows. They are the shape the revision contract on
// beads.ConditionalWriter actually promises — "backends may generate either
// counters or random tokens" — and nothing about them is ordered, evenly spaced,
// or positive.
var v59OpaqueRevisionTokens = [...]int64{
	5434260017027113294,
	-1700993557661895454,
	8834124395982504135,
	-444891346261809656,
	-1655629893108404930,
	-763273861394134104,
}

// opaqueRevisionStore replaces the native Mem/File stores' 1, 2, 3… counter with
// bd's opaque token minting. Every store in the tree mints counters — MemStore
// and FileStore do `Revision++`, sqlite does `revision=beads.revision+1` — so a
// consumer that PREDICTS `prev + 1` passes every in-tree test and still dies on
// the only backend a real city runs. This double closes that hole without an
// integration bd: reads project the token naming the row's current version, and
// fenced writes translate a token back to the underlying revision, so the CAS
// still evaluates on the real row. Consecutive tokens are ~1e18 apart, so any
// consumer arithmetic fails loudly instead of passing by luck.
//
// It embeds *beads.MemStore rather than wrapping a beads.Store because the
// conditional-writes mode stamp is unforgeable outside internal/beads: the
// embedded store promotes the stamp carrier, so beads.ResolveConditionalWriter
// resolves this type — with these overrides — as the writer.
type opaqueRevisionStore struct {
	*beads.MemStore

	mu sync.Mutex
	// tokens maps a bead ID to the opaque token naming its current row version.
	tokens map[string]int64
	// backing maps a bead ID to the underlying counter that token names.
	backing map[string]int64
	minted  int
}

func newOpaqueRevisionStore() *opaqueRevisionStore {
	return &opaqueRevisionStore{
		MemStore: beads.NewMemStore(),
		tokens:   map[string]int64{},
		backing:  map[string]int64{},
	}
}

// openOpaqueRevisionStore stamps the double through the real beads factory so
// the conditional-writes seam sees the same shape production sees.
func openOpaqueRevisionStore(t *testing.T) *opaqueRevisionStore {
	t.Helper()
	store := newOpaqueRevisionStore()
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return store, nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open opaque-revision store: %v", err)
	}
	if opened.Store != beads.Store(store) {
		t.Fatalf("factory returned %T, want the opaque-revision double itself", opened.Store)
	}
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil || writer == nil {
		t.Fatalf("resolve conditional writer on opaque-revision store = (%v, %v), want the double", writer, err)
	}
	return store
}

// project swaps a loaded row's counter for the opaque token that names it,
// minting a fresh token whenever the counter has moved.
func (s *opaqueRevisionStore) project(row beads.Bead) beads.Bead {
	if row.ID == "" || row.Revision == 0 {
		return row
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if backing, ok := s.backing[row.ID]; !ok || backing != row.Revision {
		s.backing[row.ID] = row.Revision
		s.tokens[row.ID] = s.mintLocked()
	}
	row.Revision = s.tokens[row.ID]
	return row
}

// mintLocked returns the next opaque token. Cycling six wildly separated bases
// and stepping by a large prime keeps successive tokens distinct, unordered, and
// nowhere near one apart.
func (s *opaqueRevisionStore) mintLocked() int64 {
	for {
		token := v59OpaqueRevisionTokens[s.minted%len(v59OpaqueRevisionTokens)] + int64(s.minted)*7919
		s.minted++
		if token != 0 {
			return token
		}
	}
}

// backingRevision resolves an opaque token to the counter it names.
func (s *opaqueRevisionStore) backingRevision(id string, token int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backing, ok := s.backing[id]
	return backing, ok && s.tokens[id] == token
}

func (s *opaqueRevisionStore) staleFence(id string, expected int64) error {
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	return &beads.PreconditionFailedError{ID: id, Expected: expected, Current: current.Revision}
}

func (s *opaqueRevisionStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.MemStore.Create(b)
	if err != nil {
		return created, err
	}
	return s.project(created), nil
}

func (s *opaqueRevisionStore) Get(id string) (beads.Bead, error) {
	row, err := s.MemStore.Get(id)
	if err != nil {
		return row, err
	}
	return s.project(row), nil
}

func (s *opaqueRevisionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.List(query))
}

func (s *opaqueRevisionStore) ListOpen(status ...string) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.ListOpen(status...))
}

func (s *opaqueRevisionStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.Ready(query...))
}

func (s *opaqueRevisionStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.Children(parentID, opts...))
}

func (s *opaqueRevisionStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.ListByLabel(label, limit, opts...))
}

func (s *opaqueRevisionStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.ListByAssignee(assignee, status, limit))
}

func (s *opaqueRevisionStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.projectAll(s.MemStore.ListByMetadata(filters, limit, opts...))
}

func (s *opaqueRevisionStore) projectAll(rows []beads.Bead, err error) ([]beads.Bead, error) {
	if err != nil {
		return rows, err
	}
	for i := range rows {
		rows[i] = s.project(rows[i])
	}
	return rows, nil
}

func (s *opaqueRevisionStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	backing, ok := s.backingRevision(id, expectedRevision)
	if !ok {
		return s.staleFence(id, expectedRevision)
	}
	return s.MemStore.UpdateIfMatch(id, backing, opts)
}

func (s *opaqueRevisionStore) CloseIfMatch(id string, expectedRevision int64) error {
	backing, ok := s.backingRevision(id, expectedRevision)
	if !ok {
		return s.staleFence(id, expectedRevision)
	}
	return s.MemStore.CloseIfMatch(id, backing)
}

func (s *opaqueRevisionStore) DeleteIfMatch(id string, expectedRevision int64) error {
	backing, ok := s.backingRevision(id, expectedRevision)
	if !ok {
		return s.staleFence(id, expectedRevision)
	}
	return s.MemStore.DeleteIfMatch(id, backing)
}

// TestOpaqueRevisionStoreMintsNonCounterRevisions pins the double's premise: the
// whole RED below is worthless if this store hands out prev+1 like every native
// store does.
func TestOpaqueRevisionStoreMintsNonCounterRevisions(t *testing.T) {
	store := openOpaqueRevisionStore(t)
	created, err := store.Create(beads.Bead{Title: "row", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	seen := map[int64]bool{created.Revision: true}
	previous := created.Revision
	for i := range 4 {
		if err := store.UpdateIfMatch(created.ID, previous, beads.UpdateOpts{
			Metadata: map[string]string{"round": fmt.Sprint(i)},
		}); err != nil {
			t.Fatalf("fenced write %d: %v", i, err)
		}
		row, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("re-read %d: %v", i, err)
		}
		if row.Revision == previous+1 {
			t.Fatalf("minted revision %d = previous+1: the double still behaves like a counter", row.Revision)
		}
		if !beads.RevisionKnown(row.Revision) || seen[row.Revision] {
			t.Fatalf("minted revision %d is zero or reused; seen=%v", row.Revision, seen)
		}
		seen[row.Revision] = true
		previous = row.Revision
	}
	if err := store.UpdateIfMatch(created.ID, previous+1, beads.UpdateOpts{Metadata: map[string]string{"stale": "1"}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("write fenced on a token the store never minted = %v, want PreconditionFailed", err)
	}
}

// TestRoutedWorkPoolAllocationReusesIdleMemberOnOpaqueRevisions is the
// ga-f7v2ft.144 red. It is TestRoutedWorkPoolAllocationReusesSoleIdleGenericMember
// ForNewWork's exact shape run on a store that mints opaque revisions instead of
// a counter — the only difference between the two, and the reason the reuse path
// could ship a `preRebindPersisted.Revision + 1` prediction and stay green.
//
// The rebind CAS lands. The post-rebind re-read then returns the token the store
// actually minted, which on bd (and here) is nowhere near prev+1, so the
// consumer refuses its own committed write, no nudge is delivered, and the keyed
// reuse path falls back to legacy growth on every reusable member of every
// bd-backed city.
func TestRoutedWorkPoolAllocationReusesIdleMemberOnOpaqueRevisions(t *testing.T) {
	fixture, firstWork, info := prepareIdleGenericPoolMemberForReuseWithStore(t, openOpaqueRevisionStore(t), 2)
	if err := fixture.store.Close(firstWork.ID); err != nil {
		t.Fatalf("close prior routed work: %v", err)
	}
	baselineNudges := providerNudgeCalls(fixture.provider, info.SessionNameMetadata)
	secondWork, err := fixture.store.Create(beads.Bead{
		Title: "second generic work", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create second routed work: %v", err)
	}

	result, err := fixture.cr.reconcileRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID: secondWork.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	})
	if err != nil || !result.Handled || result.Created || result.Session.ID != info.ID {
		t.Fatalf("reuse idle member on opaque revisions = (%+v, %v), want existing %q without create", result, err, info.ID)
	}
	stored, err := fixture.store.Get(info.ID)
	if err != nil {
		t.Fatalf("read rebound member: %v", err)
	}
	if stored.Metadata[beadmeta.TriggerBeadIDMetadataKey] != secondWork.ID {
		t.Fatalf("rebound trigger = %q, want %q", stored.Metadata[beadmeta.TriggerBeadIDMetadataKey], secondWork.ID)
	}
	if got := providerNudgeCalls(fixture.provider, info.SessionNameMetadata); got != baselineNudges+1 {
		t.Fatalf("nudges after reuse on opaque revisions = %d, want %d: the rebound member was refused", got, baselineNudges+1)
	}
	if len(fixture.cr.pokeCh) != 0 {
		t.Fatalf("legacy fallback after opaque-revision reuse = %d pokes, want none", len(fixture.cr.pokeCh))
	}
}
