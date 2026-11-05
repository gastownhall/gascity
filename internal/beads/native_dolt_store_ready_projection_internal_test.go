package beads

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	lookups    [][]string
	// lookupErr, when set, fails every exact-id listing: the work-outcome
	// veto's blocker read breaking while the projection reads keep answering.
	lookupErr error
}

// SearchIssues records every exact-id listing, the door filterReadyByWorkOutcome
// reads blocker outcomes through, and fails it with lookupErr when that is set.
// The listings that prime and reconcile a cache name no ids and are neither
// recorded nor failed.
func (s *nativeBlockedColumnStorage) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	if len(filter.IDs) > 0 {
		s.mu.Lock()
		s.lookups = append(s.lookups, append([]string(nil), filter.IDs...))
		err := s.lookupErr
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	return s.nativeDoltMemStorage.SearchIssues(ctx, query, filter)
}

// failLookups makes every later exact-id listing fail with err, or answer
// again when err is nil.
func (s *nativeBlockedColumnStorage) failLookups(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupErr = err
}

// blockerLookups returns the ids each exact-id listing named, in call order.
func (s *nativeBlockedColumnStorage) blockerLookups() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.lookups...)
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
	// xstore's blocker is outside the snapshot, but the column already holds
	// xstore back, so there is no false verdict for a work outcome to veto.
	if lookups := storage.blockerLookups(); len(lookups) != 0 {
		t.Fatalf("prime looked up blockers %v, want none: only a row the column calls unblocked can be vetoed", lookups)
	}
}

// TestCachedReadyOverNativeBackingHonorsAnEvictedBlockersWorkOutcome pins the
// one gate bd's is_blocked column does not carry. The column counts every
// closed blocker as satisfied, so a close recorded with gc.work_outcome=blocked
// leaves its dependent unblocked there, and the backing's own Ready holds the
// dependent back through filterReadyByWorkOutcome. The cache's own work-outcome
// check can consult only a blocker it still holds, and a reconcile lists the
// active set, so the first pass after the close evicts the blocker. Unless the
// projection carries the veto itself, every cached handle, ReadyPreferCache
// among them, then offers the dependent for the life of the cache. The evicting
// reconcile runs first with that blocker lookup failing, so the dependent must
// stay held on the verdict the cache already had until a healthy cycle vetoes
// it again.
//
//	blocker   (closed, work_outcome=blocked) <-- blocks    -- dependent  held back
//	passed    (closed step, outcome=pass)    <-- blocks    -- advanced   ready
//	plain     (closed)                       <-- blocks    -- released   ready
//	unrelated (open)                         <-- waits-for -- gated      ready
//
// passed carries the exemption ReadinessWorkOutcome grants a step that passed
// and plain is an ordinary close, so the veto must leave both dependents ready.
// gated is bd's opened gate: an unblocked verdict over an edge whose target the
// snapshot holds, which the cache's own check already covers, so it must not
// cost a blocker lookup.
func TestCachedReadyOverNativeBackingHonorsAnEvictedBlockersWorkOutcome(t *testing.T) {
	storage := &nativeBlockedColumnStorage{nativeDoltMemStorage: newNativeDoltMemStorage(), blocked: map[string]bool{}}
	native := newNativeDoltStoreForTest(storage)

	ids := map[string]string{}
	for _, title := range []string{"blocker", "dependent", "passed", "advanced", "plain", "released", "unrelated", "gated"} {
		b, err := native.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	for _, edge := range []struct{ from, kind, to string }{
		{"dependent", "blocks", "blocker"},
		{"advanced", "blocks", "passed"},
		{"released", "blocks", "plain"},
		{"gated", "waits-for", "unrelated"},
	} {
		if err := storage.store.DepAdd(ids[edge.from], ids[edge.to], edge.kind); err != nil {
			t.Fatalf("DepAdd %s %s %s: %v", edge.from, edge.kind, edge.to, err)
		}
	}
	held := []string{ids["dependent"], ids["advanced"], ids["released"]}
	for _, id := range held {
		storage.blocked[id] = true
	}
	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if lookups := storage.blockerLookups(); len(lookups) != 0 {
		t.Fatalf("prime looked up blockers %v, want none: every edge of an unblocked row ends inside the snapshot", lookups)
	}

	// The closes land behind the cache, as another writer's do, and the column
	// follows bd: every close satisfies its dependents' blocks edge.
	if _, err := storage.store.CloseAll([]string{ids["blocker"]}, map[string]string{
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	if _, err := storage.store.CloseAll([]string{ids["passed"]}, map[string]string{
		beadmeta.StepRefMetadataKey:     "review",
		beadmeta.OutcomeMetadataKey:     beadmeta.OutcomePass,
		beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked,
	}); err != nil {
		t.Fatalf("Close(passed): %v", err)
	}
	if err := storage.store.Close(ids["plain"]); err != nil {
		t.Fatalf("Close(plain): %v", err)
	}
	for _, id := range held {
		storage.blocked[id] = false
	}
	want := nativeLiveReady(t, native, ids["advanced"], ids["released"], ids["unrelated"], ids["gated"])
	stage := newReadyInvariantStage(t, cache, want)

	// The evicting reconcile's blocker lookup fails, so the cycle loses the
	// column and the dependent must stay held on the verdict the cache already
	// had.
	storage.failLookups(errors.New("blocker lookup refused"))
	cache.ReconcileForTest()
	storage.failLookups(nil)
	cache.mu.RLock()
	_, resident := cache.beads[ids["blocker"]]
	cache.mu.RUnlock()
	if resident {
		t.Fatal("the reconcile kept the closed blocker, so this fixture is not measuring an evicted one")
	}
	stage.assertHeldThroughFailedVeto("after the reconcile whose blocker lookup failed", ids["dependent"])

	before := len(storage.blockerLookups())
	cache.ReconcileForTest()
	reconciled := storage.blockerLookups()[before:]

	got, err := cache.ReadyPreferCache(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("ReadyPreferCache: %v", err)
	}
	if gotIDs := sortedIDs(got); !equalIDs(gotIDs, want) {
		t.Errorf("ReadyPreferCache = %v, want the backing's %v", gotIDs, want)
	}
	stage.assertNeverExceedsBacking("after a healthy reconcile", map[string]string{
		ids["dependent"]: "its blocker closed with gc.work_outcome=blocked, which the column counts as satisfied and filterReadyByWorkOutcome does not",
	})
	if blocked := cachedIsBlockedForTest(cache, ids["dependent"]); blocked == nil || !*blocked {
		t.Errorf("cached is_blocked for dependent = %s, want true: the projection must carry the veto its evicted blocker can no longer give", ptrStr(blocked))
	}
	stage.assertStillAnswers("after a healthy reconcile")

	// The cost: one batched lookup per cycle, naming only the blockers outside
	// the snapshot. A live read would have run the backing's own veto, so the
	// warm ReadyPreferCache above must have added none.
	if len(reconciled) != 1 {
		t.Fatalf("the reconcile looked up blockers %d times (%v), want once: the veto is one batched read per cycle", len(reconciled), reconciled)
	}
	looked := append([]string(nil), reconciled[0]...)
	sort.Strings(looked)
	outside := []string{ids["blocker"], ids["passed"], ids["plain"]}
	sort.Strings(outside)
	if !equalIDs(looked, outside) {
		t.Fatalf("the reconcile looked up %v, want exactly the blockers outside the snapshot %v", looked, outside)
	}
	if since := storage.blockerLookups()[before:]; len(since) != len(reconciled) {
		t.Fatalf("ReadyPreferCache looked up blockers %v: it read live, and a warm cache must answer from the snapshot", since[len(reconciled):])
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

	cache, ids := nativeUnreachableColumnFixture(t, storage.nativeDoltMemStorage, storage, storage.blocked)
	assertReadinessOnlyDegrade(t, cache, ids)
}

// nativeUnsupportedRefusal is the SHAPE a real backend's refusal arrives in: a
// backend-owned struct carrying its own context that UNWRAPS to the portable
// sentinel. bdhttp.UnsupportedError is exactly this, and its Unwrap is what the
// classification under test has to reach through — a fixture that returned a
// bare *beadslib.ErrUnsupported would pass an errors.As arm that a direct type
// assertion would also pass, and production has no such error.
type nativeUnsupportedRefusal struct{ unsupported *beadslib.ErrUnsupported }

func (e *nativeUnsupportedRefusal) Error() string { return e.unsupported.Error() }
func (e *nativeUnsupportedRefusal) Unwrap() error { return e.unsupported }

// nativeCallTimeRefusalStorage is a native backing whose Storage SATISFIES
// BlockedQuerier and then refuses the call, which is the shape the http backend
// has and the one the interface probe cannot see: its generated stub is a
// method with the right signature whose whole body is a backstop refusal, so
// AsBlockedQuerier succeeds, no request is issued, and the refusal arrives at
// call time.
//
// GetReadyWork and the stored column come from nativeProjectionlessStorage,
// because the two fixtures model the same BACKING FACT — the column is out of
// reach — reported through two different doors.
type nativeCallTimeRefusalStorage struct {
	*nativeProjectionlessStorage
	batchCalls int
}

func (s *nativeCallTimeRefusalStorage) IsBlocked(_ context.Context, _ string) (bool, []string, error) {
	return false, nil, s.refuse()
}

func (s *nativeCallTimeRefusalStorage) IsBlockedBatch(_ context.Context, _ []string) (map[string]bool, error) {
	s.batchCalls++
	return nil, s.refuse()
}

func (s *nativeCallTimeRefusalStorage) refuse() error {
	return &nativeUnsupportedRefusal{unsupported: &beadslib.ErrUnsupported{Op: "IsBlockedBatch", Backend: "http"}}
}

// TestNativeBackingThatRefusesTheBlockedColumnAtCallTimeDegradesReadinessOnly
// is the same invariant over the door the interface probe cannot see, and it is
// the whole of ga-2ltro.20: measured against a real bd serve, gc took the BROAD
// degrade here where the code was written to take the narrow one.
//
// The refusal does not arrive at AsBlockedQuerier, so before the fix nothing
// wrapped ErrReadyProjectionUnsupported, applyReadyProjection's sentinel check
// missed it, and the enrichment failure folded into primePartialErr — which
// only a clean prime or a reconcile pass clears, and the control-ready cache
// runs with neither. Every cache-only handle then declined for the life of the
// cache and went to the live backing: the shape applyReadyProjection's own
// comment names as the one maintainer-city was stuck in.
func TestNativeBackingThatRefusesTheBlockedColumnAtCallTimeDegradesReadinessOnly(t *testing.T) {
	storage := &nativeCallTimeRefusalStorage{
		nativeProjectionlessStorage: &nativeProjectionlessStorage{
			nativeDoltMemStorage: newNativeDoltMemStorage(),
			blocked:              map[string]bool{},
		},
	}
	// The precondition that makes this a different case from its neighbor: the
	// probe SUCCEEDS. Without it the fixture would be testing the !ok arm again.
	if _, ok := beadslib.AsBlockedQuerier(storage); !ok {
		t.Fatal("fixture storage does not expose BlockedQuerier; it must model a backing whose probe succeeds and whose call refuses")
	}

	cache, ids := nativeUnreachableColumnFixture(t, storage.nativeDoltMemStorage, storage, storage.blocked)
	if storage.batchCalls == 0 {
		t.Fatal("the prime never reached IsBlockedBatch; this fixture is not measuring the call-time refusal")
	}
	assertReadinessOnlyDegrade(t, cache, ids)
}

// nativeUnreachableColumnFixture plants the shape whose readiness verdict ONLY
// the is_blocked column can give and primes a cache over a backing that cannot
// give it:
//
//	blocker (open) <-- blocks -- parent <-- parent-child -- child
//
// child carries no blocking edge of its own, so the cache's direct-dep
// predicate calls it ready while the backing marks it blocked through the
// parent-child edge. unrelated is the control both models call ready. blocked
// is the fixture's stored column, filled in place so the storage the caller
// built and the cache primed here read the same verdicts.
func nativeUnreachableColumnFixture(t *testing.T, mem *nativeDoltMemStorage, storage beadslib.Storage, blocked map[string]bool) (*CachingStore, map[string]string) {
	t.Helper()
	native := newNativeDoltStoreForTest(storage)

	ids := map[string]string{}
	for _, title := range []string{"blocker", "parent", "child", "unrelated"} {
		b, err := native.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	if err := mem.store.DepAdd(ids["parent"], ids["blocker"], "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := mem.store.DepAdd(ids["child"], ids["parent"], "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	blocked[ids["parent"]] = true
	blocked[ids["child"]] = true

	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, ids
}

// assertReadinessOnlyDegrade is the verdict a backing that cannot answer the
// column owes, whichever door it reports through: readiness declines to the
// live backing, and the snapshot keeps serving everything that does not need
// the column.
func assertReadinessOnlyDegrade(t *testing.T, cache *CachingStore, ids map[string]string) {
	t.Helper()

	if !cache.readyReadsMustGoLive() {
		t.Fatal("a native backing that cannot answer the is_blocked column must latch the ready-projection degrade")
	}
	// The half that separates the narrow degrade from the broad one. A
	// primePartialErr is what every cache-only handle consults, so leaving one
	// set here costs the snapshot every read rather than one column.
	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial != nil {
		t.Fatalf("primePartialErr = %v, want nil: a column the backing cannot answer costs the snapshot one column, not every read", partial)
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
	if _, err := cache.Get(ids["child"]); err != nil {
		t.Errorf("Get after the degrade: %v", err)
	}
	if _, err := cache.Handles().Cached.DepList(ids["child"], "down"); err != nil {
		t.Errorf("cached reader DepList after the degrade: %v", err)
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
