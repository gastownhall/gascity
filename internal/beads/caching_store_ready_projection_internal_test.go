package beads

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// projectionStore is a backing store whose ready-projection enrichment fails a
// chosen way, so the cache's reaction to each verdict can be tested without a bd
// subprocess.
type projectionStore struct {
	Store
	err   error
	calls int
}

func (p *projectionStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	p.calls++
	return items, p.err
}

func unsupportedProjectionCause() error {
	return fmt.Errorf("bd sql ready projection: %w: exit status 1: Error: 'bd sql' is not yet supported in embedded mode", ErrReadyProjectionUnsupported)
}

func primedProjectionCache(t *testing.T, err error) (*CachingStore, string) {
	t.Helper()
	backing := NewMemStore()
	ready, createErr := backing.Create(Bead{Type: "task", Status: "open", Title: "ready work"})
	if createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}
	cache := NewCachingStoreForTest(&projectionStore{Store: backing, err: err}, nil)
	if primeErr := cache.Prime(context.Background()); primeErr != nil {
		t.Fatalf("Prime: %v", primeErr)
	}
	return cache, ready.ID
}

// TestPrimeDegradesRatherThanGoingPartialOnAnUnsupportedProjection is the
// operator-visible half of the maintainer-city defect. The enrichment failure
// folded into primePartialErr, which is never cleared except by a clean prime,
// so every cache-only read declined with "bead cache unavailable" for the life
// of the process and fell back to a live 5-6s bd subprocess.
//
// A projection the backing store CANNOT serve costs the snapshot one column,
// not rows, so the rows keep being served and the cache degrades to a named
// state rather than a partial one. What the missing column costs READINESS is a
// separate verdict, pinned by TestDegradedProjectionSendsReadyToTheLiveBdVerdict.
func TestPrimeDegradesRatherThanGoingPartialOnAnUnsupportedProjection(t *testing.T) {
	cache, readyID := primedProjectionCache(t, unsupportedProjectionCause())

	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial != nil {
		t.Fatalf("primePartialErr = %v, want nil: an unsupported projection must not make the cache permanently unavailable", partial)
	}

	rows, ok := cache.CachedList(ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("CachedList declined after the degrade: the rows are whole, so non-readiness reads must keep being served from cache")
	}
	if len(rows) != 1 || rows[0].ID != readyID {
		t.Fatalf("CachedList rows = %+v, want the cached bead %s", rows, readyID)
	}
	if _, err := cache.Get(readyID); err != nil {
		t.Fatalf("Get after the degrade: %v", err)
	}

	stats := cache.Stats()
	if !strings.Contains(stats.LastProblem, "not yet supported in embedded mode") {
		t.Errorf("LastProblem = %q, want the degrade to name its cause", stats.LastProblem)
	}
	if !strings.Contains(stats.LastProblem, "ready projection") {
		t.Errorf("LastProblem = %q, want the degrade to name the projection", stats.LastProblem)
	}
}

// TestPrimeStaysPartialOnATransientProjectionFailure is the control: only the
// structural verdict degrades. A projection that merely failed this cycle still
// marks the snapshot partial, because the rows really are missing an answer the
// store can give.
func TestPrimeStaysPartialOnATransientProjectionFailure(t *testing.T) {
	cache, _ := primedProjectionCache(t, errors.New("bd sql ready projection: exit status 1: dial tcp: connection refused"))

	cache.mu.RLock()
	partial := cache.primePartialErr
	cache.mu.RUnlock()
	if partial == nil {
		t.Fatal("primePartialErr = nil, want a transient projection failure to keep marking the snapshot partial")
	}
}

// bdVerdictStore is a backing store whose ready projection is structurally
// unavailable and whose Ready answers the way `bd ready` does.
//
// bd's is_blocked is not the cache's dependency-derived predicate: migration
// 0046_add_is_blocked.up.sql propagates blocked-ness transitively DOWN
// parent-child edges (its `reachable` CTE joins d.type='parent-child', pinned
// upstream by TestIsBlocked_ParentChildTransitivePropagation) and `bd ready`
// filters is_blocked = 0. blocked names the ids bd's column marks, so Ready
// here is bd's verdict rather than the MemStore's direct-deps-only one.
type bdVerdictStore struct {
	Store
	projectionErr error
	blocked       map[string]bool

	mu         sync.Mutex
	readyCalls int
	listCalls  int
}

func (b *bdVerdictStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	return items, b.projectionErr
}

func (b *bdVerdictStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	b.mu.Lock()
	b.readyCalls++
	b.mu.Unlock()
	rows, err := b.Store.Ready(query...)
	if err != nil {
		return nil, err
	}
	kept := make([]Bead, 0, len(rows))
	for _, row := range rows {
		if b.blocked[row.ID] {
			continue
		}
		kept = append(kept, row)
	}
	return kept, nil
}

func (b *bdVerdictStore) List(query ListQuery) ([]Bead, error) {
	b.mu.Lock()
	b.listCalls++
	b.mu.Unlock()
	return b.Store.List(query)
}

func (b *bdVerdictStore) counts() (ready, list int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readyCalls, b.listCalls
}

// transitivelyBlockedFixture builds the shape the two readiness models disagree
// about: a parent held by an open `blocks` dep, and a child attached to it by
// parent-child only.
//
//	blocker (open) <-- blocks -- parent <-- parent-child -- child
//
// The child carries no blocking dep of its own, so the cache's predicate calls
// it ready; bd marks it is_blocked=1 through the parent-child edge and hides it.
// unrelated is the control both models call ready.
func transitivelyBlockedFixture(t *testing.T, projectionErr error) (*CachingStore, *bdVerdictStore, map[string]string) {
	t.Helper()
	mem := NewMemStore()
	ids := map[string]string{}
	for _, title := range []string{"blocker", "parent", "child", "unrelated"} {
		b, err := mem.Create(Bead{Type: "task", Status: "open", Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids[title] = b.ID
	}
	if err := mem.DepAdd(ids["parent"], ids["blocker"], "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := mem.DepAdd(ids["child"], ids["parent"], "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}

	backing := &bdVerdictStore{
		Store:         mem,
		projectionErr: projectionErr,
		blocked:       map[string]bool{ids["parent"]: true, ids["child"]: true},
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, backing, ids
}

// TestDegradedProjectionSendsReadyToTheLiveBdVerdict is the correctness half of
// the degrade, and the reason the flag is not primePartialErr.
//
// Without the projection every bead's IsBlocked is nil, and cachedBeadReady then
// consults only the bead's OWN blocks/waits-for/conditional-blocks deps. That is
// strictly weaker than bd's is_blocked, which is transitive down parent-child:
// serving from cache would OFFER the control dispatcher a step whose molecule
// gate has not opened, the regression #3218 closed by mirroring bd's projection
// in the first place. So readiness — and only readiness — declines the cache and
// takes bd's verdict live.
func TestDegradedProjectionSendsReadyToTheLiveBdVerdict(t *testing.T) {
	cache, backing, ids := transitivelyBlockedFixture(t, unsupportedProjectionCause())

	if !cache.readyReadsMustGoLive() {
		t.Fatal("cache did not latch the ready-projection degrade")
	}

	readyBefore, _ := backing.counts()
	rows, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got := sortedIDs(rows)
	if want := wantReadyIDs(ids["blocker"], ids["unrelated"]); !equalIDs(got, want) {
		t.Fatalf("Ready = %v, want %v: the transitively blocked child %s must not be offered as ready",
			got, want, ids["child"])
	}
	readyAfter, listAfter := backing.counts()
	if readyAfter != readyBefore+1 {
		t.Fatalf("backing Ready calls = %d, want %d: the readiness read must reach bd's verdict", readyAfter, readyBefore+1)
	}

	if _, ok := cache.CachedReady(); ok {
		t.Fatal("CachedReady answered from a cache with no ready projection; the control dispatcher reads this handle")
	}
	if _, err := cache.ReadyContext(context.Background()); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("ReadyContext error = %v, want ErrCacheUnavailable", err)
	}
	if _, err := cache.Handles().Cached.Ready(); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("cached reader Ready error = %v, want ErrCacheUnavailable", err)
	}

	// The outage this PR fixes must stay fixed: everything that does not depend
	// on is_blocked keeps being served from the cache, with no backing traffic.
	cached, ok := cache.CachedList(ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("CachedList declined: the degrade must not make non-readiness reads unavailable")
	}
	if len(cached) != len(ids) {
		t.Fatalf("CachedList returned %d rows, want %d", len(cached), len(ids))
	}
	if _, listNow := backing.counts(); listNow != listAfter {
		t.Fatalf("backing List calls = %d, want %d: cached reads must not reach the backing store", listNow, listAfter)
	}
}

// TestHealthyProjectionStillAnswersReadyFromCache is the control for the test
// above: with a projection the backing store CAN serve, readiness is answered
// from the cache off bd's is_blocked column, so the blocked child is hidden
// without a live query.
func TestHealthyProjectionStillAnswersReadyFromCache(t *testing.T) {
	cache, backing, ids := transitivelyBlockedFixture(t, nil)
	// A backing store that serves the projection stamps is_blocked itself; the
	// fixture's enrichment is a no-op, so stamp the verdict the column carries.
	cache.mu.Lock()
	for id, blocked := range backing.blocked {
		b := cache.beads[id]
		b.IsBlocked = cloneBoolPtr(&blocked)
		cache.beads[id] = b
	}
	cache.mu.Unlock()

	if cache.readyReadsMustGoLive() {
		t.Fatal("a servable projection must not latch the degrade")
	}
	readyBefore, _ := backing.counts()
	rows, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got, want := sortedIDs(rows), wantReadyIDs(ids["blocker"], ids["unrelated"]); !equalIDs(got, want) {
		t.Fatalf("Ready = %v, want %v", got, want)
	}
	if readyAfter, _ := backing.counts(); readyAfter != readyBefore {
		t.Fatalf("backing Ready calls = %d, want %d: a healthy cache answers readiness itself", readyAfter, readyBefore)
	}
}

// wantReadyIDs is the expected readiness ANSWER, compared against sortedIDs so
// the assertion does not depend on the order two different readers impose on
// it: the cache path sorts with sortBeadsReadyOrder and the live path returns
// the backing store's order.
func wantReadyIDs(ids ...string) []string {
	want := append([]string(nil), ids...)
	sort.Strings(want)
	return want
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestReadyJudgmentContextJudgesRowByRow (ga-ylpbsy.67): the judgment serves
// what ReadyContext serves while the cache can vouch for every row, and
// reports the snapshot it judged from: the edges of every judged candidate,
// served or not, and every row the cache held, closed ones included. Where
// ReadyContext declines the whole read over one dirty or unanswerable row, the
// judgment leaves just that row unjudged. Kills: a candidate's edges reported
// only when it was served, which lets a reader trust a stale "ready" over the
// cache's "blocked"; a row the cache does not hold, or holds dirty, reported
// as held, which lets a reader trust a verdict that skipped or misread that
// target; an unanswerable row judged anyway; and a row whose released verdict
// its blocking edges reproduce left unjudged over an edge no verdict reads.
func TestReadyJudgmentContextJudgesRowByRow(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatal(err)
	}
	engine := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = engine.CloseStore() })
	create := func(title string) Bead {
		t.Helper()
		b, err := engine.Create(Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	blocker, step, loose, done := create("blocker"), create("step"), create("loose"), create("done")
	parent, child := create("parent"), create("child")
	root, gate, tracked := create("root"), create("gate"), create("tracked")
	for _, edge := range [][3]string{
		{step.ID, blocker.ID, "blocks"},
		{loose.ID, "gcg-absent", "blocks"},
		{child.ID, parent.ID, "blocks"},
		{child.ID, "gc-elsewhere", "parent-child"},
		{tracked.ID, gate.ID, "blocks"},
		{tracked.ID, root.ID, "tracks"},
	} {
		if err := engine.DepAdd(edge[0], edge[1], edge[2]); err != nil {
			t.Fatal(err)
		}
	}
	cache := NewCachingStoreForTest(engine, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	q := ReadyQuery{TierMode: TierBoth}
	ids := func(rows []Bead) string {
		out := make([]string, 0, len(rows))
		for _, b := range rows {
			out = append(out, b.ID)
		}
		return fmt.Sprint(out)
	}

	judged, err := cache.ReadyJudgmentContext(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyJudgmentContext: %v", err)
	}
	rows, err := cache.ReadyContext(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if got, want := ids(judged.Rows), ids(rows); got != want {
		t.Errorf("judged rows %v, ReadyContext %v", got, want)
	}
	if deps, ok := judged.Deps[step.ID]; !ok || len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
		t.Errorf("Deps[%s] = %v (judged %t), want its edge to %s though the step is not served", step.ID, deps, ok, blocker.ID)
	}
	if deps, ok := judged.Deps[blocker.ID]; !ok || len(deps) != 0 {
		t.Errorf("Deps[%s] = %v (judged %t), want an empty entry", blocker.ID, deps, ok)
	}
	if _, ok := judged.Deps[done.ID]; ok {
		t.Errorf("Deps holds closed %s, which is no ready candidate", done.ID)
	}
	for id, want := range map[string]bool{blocker.ID: true, step.ID: true, done.ID: true, "gcg-absent": false} {
		if got := judged.Holds(id); got != want {
			t.Errorf("Holds(%s) = %t, want %t", id, got, want)
		}
	}

	// child keeps a parent-child edge, which bd's verdict reads and the
	// dependency-derived predicate does not, so once its blocker closes its
	// lost verdict cannot be re-derived. tracked keeps only a tracks edge
	// besides its blocker, which no backend's verdict reads, so its verdict
	// can be. loose goes dirty.
	for _, id := range []string{parent.ID, gate.ID} {
		if err := cache.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	cache.markDirtyLocked(loose.ID)
	cache.mu.Unlock()
	if _, err := cache.ReadyContext(context.Background(), q); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("fixture: ReadyContext = %v, want it to decline over a dirty row", err)
	}
	judged, err = cache.ReadyJudgmentContext(context.Background(), q)
	if err != nil {
		t.Fatalf("ReadyJudgmentContext over a dirty and an unanswerable row: %v", err)
	}
	for _, id := range []string{loose.ID, child.ID} {
		if _, ok := judged.Deps[id]; ok {
			t.Errorf("%s was judged; a dirty or unanswerable row must be left unjudged", id)
		}
		if strings.Contains(ids(judged.Rows), id) {
			t.Errorf("judged rows %v serve %s, which the cache cannot vouch for", ids(judged.Rows), id)
		}
	}
	if judged.Holds(loose.ID) {
		t.Errorf("Holds(%s) = true for a dirty row", loose.ID)
	}
	if _, ok := judged.Deps[step.ID]; !ok {
		t.Errorf("%s was left unjudged; only the rows the cache cannot vouch for may be", step.ID)
	}
	if _, ok := judged.Deps[tracked.ID]; !ok || !strings.Contains(ids(judged.Rows), tracked.ID) {
		t.Errorf("%s, released by %s's close and tracking %s, is not judged ready (judged %t, rows %v)", tracked.ID, gate.ID, root.ID, ok, ids(judged.Rows))
	}
}
