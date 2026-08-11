package beads

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// nativeBlockedColumnStorage is a native beadslib backing that carries the one
// thing a MemStore cannot: bd's denormalized, TRANSITIVE is_blocked column.
//
// The real Dolt storage maintains that column down parent-child edges
// (issueops.markBlockedTemplateForIssues joins `d.type = 'parent-child' AND
// p.is_blocked = 1`), GetReadyWork filters on it (sqlbuild.ReadyWhere emits
// `is_blocked = 0`), and BlockedQuerier.IsBlockedBatch reads it back. blocked
// stands in for the stored column, so both reads here answer from the same
// place the live ledger does and cannot drift apart inside the fixture.
type nativeBlockedColumnStorage struct {
	*nativeDoltMemStorage
	blocked map[string]bool

	mu         sync.Mutex
	batchCalls int
	batchedIDs int
}

// IsBlocked mirrors the stored column for one id.
func (s *nativeBlockedColumnStorage) IsBlocked(_ context.Context, id string) (bool, []string, error) {
	return s.blocked[id], nil, nil
}

// IsBlockedBatch mirrors issueops.IsBlockedBatchInTx: one read over the stored
// column, with ids absent from both tables absent from the map.
func (s *nativeBlockedColumnStorage) IsBlockedBatch(_ context.Context, ids []string) (map[string]bool, error) {
	s.mu.Lock()
	s.batchCalls++
	s.batchedIDs += len(ids)
	s.mu.Unlock()
	rows := s.rows()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, resident := rows[id]; !resident {
			continue
		}
		out[id] = s.blocked[id]
	}
	return out, nil
}

// rows is the fixture's stand-in for "the id exists in issues or wisps". The
// MemStore scan behind it cannot fail; a nil map would simply enrich nothing
// and fail the assertions loudly.
func (s *nativeBlockedColumnStorage) rows() map[string]Bead {
	beads, err := s.store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		return nil
	}
	rows := make(map[string]Bead, len(beads))
	for _, b := range beads {
		rows[b.ID] = b
	}
	return rows
}

// GetReadyWork answers the way bd's does: open rows whose stored is_blocked is
// 0. It deliberately does NOT re-derive blocked-ness from the edges, because
// the whole point of the column is that it already carries the transitive
// answer the edge walk cannot reach.
func (s *nativeBlockedColumnStorage) GetReadyWork(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	beads, err := s.store.List(ListQuery{AllowScan: true, Status: string(beadslib.StatusOpen), TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	ready := make([]Bead, 0, len(beads))
	for _, bead := range beads {
		if !filter.IncludeEphemeral && bead.Ephemeral {
			continue
		}
		if filter.Assignee != nil && bead.Assignee != *filter.Assignee {
			continue
		}
		if s.blocked[bead.ID] {
			continue
		}
		ready = append(ready, bead)
	}
	return nativeIssuesFromBeads(ready)
}

func (s *nativeBlockedColumnStorage) counts() (calls, ids int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchCalls, s.batchedIDs
}

// nativeReadyDisagreementFixture builds the two shapes where a cache's own
// dependency-derived readiness predicate and a native backing's is_blocked
// column disagree, primes a cache over the native store, and returns both.
//
//	blocker (open) <-- blocks -- parent <-- parent-child -- child
//	gcg-offscope (not in this scope) <-- blocks -- xstore
//
// child carries no blocking dep of its own, so cachedBeadReady calls it ready
// while bd marks it is_blocked=1 through the parent-child edge. xstore carries
// one, but its target is not resident in this scope's cache, and cachedBeadReady
// treats a dep as blocking only when statusByID holds the target — so that edge
// is invisible to it too. unrelated is the control both models call ready.
func nativeReadyDisagreementFixture(t *testing.T) (*CachingStore, *NativeDoltStore, *nativeBlockedColumnStorage, map[string]string) {
	t.Helper()
	storage := &nativeBlockedColumnStorage{nativeDoltMemStorage: newNativeDoltMemStorage(), blocked: map[string]bool{}}
	native := newNativeDoltStoreForTest(storage)

	ids := map[string]string{}
	for _, title := range []string{"blocker", "parent", "child", "xstore", "unrelated"} {
		b, err := native.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	if err := storage.store.DepAdd(ids["parent"], ids["blocker"], "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := storage.store.DepAdd(ids["child"], ids["parent"], "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	// The relocated graph class lives in another store, so its row is never
	// primed into this scope's cache even though the edge is.
	if err := storage.store.DepAdd(ids["xstore"], "gcg-offscope", "blocks"); err != nil {
		t.Fatalf("DepAdd cross-store blocks: %v", err)
	}
	storage.blocked[ids["parent"]] = true
	storage.blocked[ids["child"]] = true
	storage.blocked[ids["xstore"]] = true

	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, native, storage, ids
}

// TestCachedReadyOverNativeBackingNeverOffersWorkTheBackingHides is the
// invariant a cache over a native store must satisfy, and the one the rig
// scopes newly depend on now that preflight lets them reach NativeDoltStore.
//
// NativeDoltStore.listIncludesCompleteDependencies reports true, which latches
// depsComplete on the cache and hands readiness to the cache's own predicate.
// That predicate reads bd's is_blocked when it is present and falls back to the
// bead's OWN direct blocks/waits-for/conditional-blocks deps when it is not.
// The fallback is strictly weaker than the column in two ways this fixture
// pins: it does not propagate down parent-child, and it ignores an edge whose
// target is not resident in the same scope. Either gap offers the control
// dispatcher a step whose gate has not opened — regression #3218.
// The subtests re-assert the invariant in the states a long-lived cache
// actually spends its life in, not just the freshly primed one (ga-cfhgr).
// beadFromNativeIssue cannot set IsBlocked — beadslib's types.Issue carries no
// is_blocked field — so every routine refresh through backing.Get hands the
// cache a row with no verdict. Before ga-cfhgr each of these overwrote the
// column and left the weaker predicate answering, with no degrade latched,
// which is why this defect was already live on the Dolt-backed cities rather
// than introduced by the BdStore work.
func TestCachedReadyOverNativeBackingNeverOffersWorkTheBackingHides(t *testing.T) {
	hidden := func(ids map[string]string) map[string]string {
		return map[string]string{
			ids["child"]:  "blocked-ness propagates down parent-child and the cache's direct-dep predicate cannot see it",
			ids["xstore"]: "its blocker gcg-offscope is not resident in this scope's cache, and cachedBeadReady treats a dep as blocking only when statusByID holds the target",
		}
	}

	t.Run("after prime", func(t *testing.T) {
		cache, native, _, ids := nativeReadyDisagreementFixture(t)
		stage := newReadyInvariantStage(t, cache, nativeLiveReady(t, native, ids["blocker"], ids["unrelated"]))

		stage.assertNeverExceedsBacking("after prime", hidden(ids))
		stage.assertStillAnswers("after prime")
	})

	// A write-through refresh (CachingStore.Update -> backing.Get) reinstalls
	// the row from a payload that cannot carry is_blocked.
	t.Run("after update", func(t *testing.T) {
		cache, native, _, ids := nativeReadyDisagreementFixture(t)
		stage := newReadyInvariantStage(t, cache, nativeLiveReady(t, native, ids["blocker"], ids["unrelated"]))

		for _, name := range []string{"child", "xstore"} {
			assignee := "agent"
			if err := cache.Update(ids[name], UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatalf("Update(%s): %v", name, err)
			}
			stage.assertNeverExceedsBacking("after Update("+name+")", hidden(ids))
		}
		stage.assertStillAnswers("after Update")
	})

	// The dirty-row overlay refreshes through the same projection-less Get
	// before serving a cached read.
	t.Run("after dirty overlay ready", func(t *testing.T) {
		cache, native, _, ids := nativeReadyDisagreementFixture(t)
		stage := newReadyInvariantStage(t, cache, nativeLiveReady(t, native, ids["blocker"], ids["unrelated"]))

		markDirtyForTest(cache, ids["child"])
		if _, err := cache.Ready(); err != nil {
			t.Fatalf("Ready over dirty overlay: %v", err)
		}
		stage.assertNeverExceedsBacking("after dirty-overlay Ready", hidden(ids))
		stage.assertStillAnswers("after dirty-overlay Ready")
	})

	// A close invalidates its dependents' projection so the direct-dep
	// predicate recomputes — correct for a direct blocks edge, blind to the
	// parent-child propagation the column carries.
	t.Run("after close", func(t *testing.T) {
		cache, native, ids := nativeReadyCloseInvalidationFixture(t)

		newReadyInvariantStage(t, cache, nativeLiveReady(t, native, ids["gate"], ids["x"])).
			assertStillAnswers("before close")

		if err := cache.Close(ids["x"]); err != nil {
			t.Fatalf("Close(x): %v", err)
		}
		stage := newReadyInvariantStage(t, cache, nativeLiveReady(t, native, ids["gate"]))
		stage.assertNeverExceedsBacking("after Close(x)", map[string]string{
			ids["both"]: "its parent is still blocked by the open gate, and blocked-ness propagates down the parent-child edge",
		})
		// The cost, stated: the row carries a parent-child edge, so its verdict
		// is one only the column can give and the cache declines rather than
		// guesses. And it is a WINDOW, not a latch — the next reconcile refills
		// the column and the cache serves readiness again.
		stage.assertDeclines("after Close(x)", ids["both"])
		cache.runReconciliation()
		stage.assertStillAnswers("after Close(x) + reconcile")
	})
}

// nativeLiveReady reads the backing's own verdict and pins the fixture to it.
func nativeLiveReady(t *testing.T, native *NativeDoltStore, want ...string) []string {
	t.Helper()
	live, err := native.Ready()
	if err != nil {
		t.Fatalf("native Ready: %v", err)
	}
	got := sortedIDs(live)
	if !equalIDs(got, wantReadyIDs(want...)) {
		t.Fatalf("native Ready = %v, want %v: the fixture must model bd's is_blocked filter", got, wantReadyIDs(want...))
	}
	return got
}

// nativeReadyCloseInvalidationFixture is the close-path shape on the native
// store:
//
//	gate (open) <-- blocks -- parent <-- parent-child -- both
//	x (open)    <-- blocks -- both
//
// Closing x satisfies both's only DIRECT blocking edge, so the cache's
// dependency-derived predicate calls it ready the moment
// clearDependentReadyProjectionsLocked drops its is_blocked. The backing does
// not: both's parent is still blocked by the open gate, and blocked-ness
// propagates down parent-child. Every other row's verdict is unchanged by the
// close, so the fixture's stored column stays honest across the mutation.
func nativeReadyCloseInvalidationFixture(t *testing.T) (*CachingStore, *NativeDoltStore, map[string]string) {
	t.Helper()
	storage := &nativeBlockedColumnStorage{nativeDoltMemStorage: newNativeDoltMemStorage(), blocked: map[string]bool{}}
	native := newNativeDoltStoreForTest(storage)

	ids := map[string]string{}
	for _, title := range []string{"gate", "x", "parent", "both"} {
		b, err := native.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	if err := storage.store.DepAdd(ids["parent"], ids["gate"], "blocks"); err != nil {
		t.Fatalf("DepAdd parent blocks gate: %v", err)
	}
	if err := storage.store.DepAdd(ids["both"], ids["x"], "blocks"); err != nil {
		t.Fatalf("DepAdd both blocks x: %v", err)
	}
	if err := storage.store.DepAdd(ids["both"], ids["parent"], "parent-child"); err != nil {
		t.Fatalf("DepAdd both parent-child parent: %v", err)
	}
	storage.blocked[ids["parent"]] = true
	storage.blocked[ids["both"]] = true

	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, native, ids
}

// TestNativeReadyProjectionIsOneBatchedReadPerCycle pins the cost of filling the
// column. The enrichment runs on every cache prime and every reconcile of every
// native scope, so it must stay a single batched read over the active set — the
// shape IsBlockedBatch exists for — rather than a per-bead fan-out.
func TestNativeReadyProjectionIsOneBatchedReadPerCycle(t *testing.T) {
	_, _, storage, ids := nativeReadyDisagreementFixture(t)

	calls, batched := storage.counts()
	if calls != 1 {
		t.Fatalf("IsBlockedBatch calls during prime = %d, want 1: the projection must be one batched read, not a fan-out", calls)
	}
	if batched != len(ids) {
		t.Fatalf("IsBlockedBatch received %d ids, want %d: the batch must cover the whole active set", batched, len(ids))
	}
}

// nativeProjectionlessStorage is a native backing whose beadslib Storage does
// not expose BlockedQuerier, so the is_blocked column is unreachable. The core
// beads.Storage interface does not declare IsBlockedBatch (only DoltStorage
// composes DependencyQueryStore), so this is a real shape, not a contrivance.
type nativeProjectionlessStorage struct {
	*nativeDoltMemStorage
	blocked map[string]bool
}

func (s *nativeProjectionlessStorage) GetReadyWork(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	beads, err := s.store.List(ListQuery{AllowScan: true, Status: string(beadslib.StatusOpen), TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	ready := make([]Bead, 0, len(beads))
	for _, bead := range beads {
		if !filter.IncludeEphemeral && bead.Ephemeral {
			continue
		}
		if s.blocked[bead.ID] {
			continue
		}
		ready = append(ready, bead)
	}
	return nativeIssuesFromBeads(ready)
}

// TestNativeBackingWithoutTheBlockedColumnSendsReadyToTheLiveVerdict extends
// #5183's invariant to the native path.
//
// TestDegradedProjectionSendsReadyToTheLiveBdVerdict pins that a cache with no
// is_blocked column must decline every readiness handle and take the backing's
// own verdict. It guards only the BdStore shape. A native store whose storage
// cannot answer IsBlockedBatch is the same state — every IsBlocked nil, the
// cache's predicate weaker than the backing's — and owes the same fail-safe.
func TestNativeBackingWithoutTheBlockedColumnSendsReadyToTheLiveVerdict(t *testing.T) {
	storage := &nativeProjectionlessStorage{nativeDoltMemStorage: newNativeDoltMemStorage(), blocked: map[string]bool{}}
	if _, ok := beadslib.AsBlockedQuerier(storage); ok {
		t.Fatal("fixture storage exposes BlockedQuerier; it must model a backing that cannot answer the column")
	}
	native := newNativeDoltStoreForTest(storage)

	ids := map[string]string{}
	for _, title := range []string{"blocker", "parent", "child", "unrelated"} {
		b, err := native.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	if err := storage.store.DepAdd(ids["parent"], ids["blocker"], "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := storage.store.DepAdd(ids["child"], ids["parent"], "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	storage.blocked[ids["parent"]] = true
	storage.blocked[ids["child"]] = true

	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if !cache.readyReadsMustGoLive() {
		t.Fatal("a native backing that cannot answer the is_blocked column must latch the ready-projection degrade")
	}
	rows, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got, want := sortedIDs(rows), wantReadyIDs(ids["blocker"], ids["unrelated"]); !equalIDs(got, want) {
		t.Fatalf("Ready = %v, want %v: the transitively blocked child %s must not be offered", got, want, ids["child"])
	}
	if _, ok := cache.CachedReady(); ok {
		t.Error("CachedReady answered from a cache with no is_blocked column; the control dispatcher reads this handle")
	}
	if _, err := cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("ReadyContext error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Handles().Cached.Ready(); !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("cached reader Ready error = %v, want ErrCacheUnavailable", err)
	}

	// The rows are whole, so everything that does not need the column keeps
	// serving from cache — the separation #5183 established.
	cached, ok := cache.CachedList(ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("CachedList declined: the degrade must not make non-readiness reads unavailable")
	}
	if len(cached) != len(ids) {
		t.Fatalf("CachedList returned %d rows, want %d", len(cached), len(ids))
	}
}

// containsID reports whether ids holds want.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// idsBeyond returns the members of got that want does not contain.
func idsBeyond(got, want []string) []string {
	allowed := make(map[string]struct{}, len(want))
	for _, id := range want {
		allowed[id] = struct{}{}
	}
	var extra []string
	for _, id := range got {
		if _, ok := allowed[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return extra
}
