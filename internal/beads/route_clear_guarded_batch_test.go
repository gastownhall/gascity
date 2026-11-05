package beads_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// These tests pin the guarded-batch gate (ga-l69b9m, architect Amendment 1 on
// ga-cn94949q): a guarded batch whose Update item writes gc.routed_to reaches
// the engine through the route-clearing decorator's GuardedBatcherHandle, and
// that handle answers a gate that clears the executor-identity stamps exactly
// as the decorator's own Update does -- only after the batch landed, and only
// for a genuine re-route.
//
// Every test runs over a SQLite engine, the one engine whose guarded batcher
// the production stack reaches through the decorator.

// spyBatchEngine is a SQLite engine that counts the reads and writes the
// decorator and its gate issue against it. It answers its own batcher handle so
// the counters see ApplyGuarded; every other method is the engine's.
type spyBatchEngine struct {
	*beads.SQLiteStore
	gets       atomic.Int64
	applies    atomic.Int64
	clears     atomic.Int64
	failClears bool
	// onGet runs after each Get, so a test can act between the gate's pre-read
	// and its delegation.
	onGet func()
}

func (e *spyBatchEngine) Get(id string) (beads.Bead, error) {
	e.gets.Add(1)
	b, err := e.SQLiteStore.Get(id)
	if e.onGet != nil {
		e.onGet()
	}
	return b, err
}

func (e *spyBatchEngine) SetMetadataBatch(id string, kv map[string]string) error {
	e.clears.Add(1)
	if e.failClears {
		return errors.New("injected clear failure")
	}
	return e.SQLiteStore.SetMetadataBatch(id, kv)
}

func (e *spyBatchEngine) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	inner, ok := e.SQLiteStore.GuardedBatcherHandle()
	if !ok {
		return nil, false
	}
	return spyBatcher{engine: e, inner: inner}, true
}

type spyBatcher struct {
	engine *spyBatchEngine
	inner  beads.GuardedBatcher
}

func (b spyBatcher) ApplyGuarded(ctx context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	b.engine.applies.Add(1)
	return b.inner.ApplyGuarded(ctx, batch)
}

// guardedBatchFixture is a stamped bead and a stamped molecule step over a
// decorated SQLite engine.
type guardedBatchFixture struct {
	engine  *spyBatchEngine
	store   beads.Store
	batcher beads.GuardedBatcher
	bead    beads.Bead
	root    beads.Bead
	step    beads.Bead
	prefix  string
}

func newGuardedBatchFixture(t *testing.T) *guardedBatchFixture {
	t.Helper()
	engine := &spyBatchEngine{SQLiteStore: newSQLiteForConformance(t)}
	f := &guardedBatchFixture{engine: engine}
	f.bead = seedRoutedBead(t, engine, testOldTarget)
	f.root = seedRoutedBead(t, engine, testOldTarget)
	step, err := engine.Create(beads.Bead{
		Title: "molecule step",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:      testOldTarget,
			beadmeta.SessionNameMetadataKey:   "gascity--old-executor",
			beadmeta.WorkDirMetadataKey:       "worktrees/old-executor",
			beadmeta.LegacyWorkDirMetadataKey: "worktrees/old-executor-legacy",
			beadmeta.RootBeadIDMetadataKey:    f.root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	f.step = step
	f.prefix = strings.SplitN(f.bead.ID, "-", 2)[0]
	f.store = beads.WithRouteChangeClearing(engine, identityNormalizer)
	batcher, ok := beads.GuardedBatcherFor(f.store)
	if !ok {
		t.Fatalf("GuardedBatcherFor(decorator over SQLite) found no batcher: the decorator hides the engine's guarded batcher")
	}
	f.batcher = batcher
	return f
}

func (f *guardedBatchFixture) reroute(id, to string, guard *int64) beads.GuardedItem {
	return beads.GuardedItem{Update: &beads.GuardedUpdate{
		ID:               id,
		ExpectedRevision: guard,
		Opts:             beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: to}},
	}}
}

func (f *guardedBatchFixture) current(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := f.engine.SQLiteStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

func assertGuardedLanded(t *testing.T, f *guardedBatchFixture, id, wantTarget string) {
	t.Helper()
	if got := f.current(t, id).Metadata[beadmeta.RoutedToMetadataKey]; got != wantTarget {
		t.Fatalf("gc.routed_to on %s = %q, want %q: the batch did not land", id, got, wantTarget)
	}
}

// A genuine re-route in a batch clears the bead's stamps and, for a molecule
// step, its root's mirrors.
func TestRouteClearGuardedBatch_GenuineRerouteClearsStampsAndRootMirrors(t *testing.T) {
	f := newGuardedBatchFixture(t)
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testNewTarget, nil), f.reroute(f.step.ID, testNewTarget, nil)},
	}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	assertGuardedLanded(t, f, f.bead.ID, testNewTarget)
	assertGuardedLanded(t, f, f.step.ID, testNewTarget)
	assertStampsCleared(t, f.engine, f.bead.ID)
	assertStampsCleared(t, f.engine, f.step.ID)
	assertStampsCleared(t, f.engine, f.root.ID)
}

// Writing the route a bead already carries is not a re-route.
func TestRouteClearGuardedBatch_SameExecutorRestoreKeepsStamps(t *testing.T) {
	f := newGuardedBatchFixture(t)
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testOldTarget, nil)},
	}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	assertGuardedLanded(t, f, f.bead.ID, testOldTarget)
	assertStampsIntact(t, f.engine, f.bead.ID)
}

// A batch the engine refuses wrote nothing, so it clears nothing.
func TestRouteClearGuardedBatch_StaleRevisionRefusalClearsNothing(t *testing.T) {
	f := newGuardedBatchFixture(t)
	stale := f.bead.Revision + 1000
	_, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testNewTarget, &stale)},
	})
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded with a stale guard = %v, want a precondition failure", err)
	}
	assertGuardedLanded(t, f, f.bead.ID, testOldTarget)
	assertStampsIntact(t, f.engine, f.bead.ID)
	if n := f.engine.clears.Load(); n != 0 {
		t.Errorf("a refused batch ran %d stamp clear(s), want none", n)
	}
	if n := f.engine.gets.Load(); n != 1 {
		t.Errorf("a refused batch issued %d Get(s), want only the one pre-read", n)
	}
}

// A batch with no routed Update item costs no extra read and no clear.
func TestRouteClearGuardedBatch_NoRoutedItemAddsNoReads(t *testing.T) {
	f := newGuardedBatchFixture(t)
	title := "renamed"
	before := f.engine.gets.Load()
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{
			{Update: &beads.GuardedUpdate{ID: f.bead.ID, Opts: beads.UpdateOpts{Title: &title}}},
			{Update: &beads.GuardedUpdate{ID: f.step.ID, Opts: beads.UpdateOpts{Metadata: map[string]string{"other": "x"}}}},
		},
	}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if got := f.current(t, f.bead.ID).Title; got != title {
		t.Fatalf("title = %q, want %q: the batch did not land", got, title)
	}
	// f.current reads through the embedded store, not the spy, so the counter
	// holds only what the gate issued.
	if n := f.engine.gets.Load() - before; n != 0 {
		t.Errorf("a batch with no gc.routed_to item issued %d Get(s), want 0", n)
	}
	if n := f.engine.clears.Load(); n != 0 {
		t.Errorf("a batch with no gc.routed_to item ran %d clear(s), want 0", n)
	}
	assertStampsIntact(t, f.engine, f.bead.ID)
}

// A pinned Create that carries gc.routed_to supersedes no earlier route: no
// pre-read, no clear.
func TestRouteClearGuardedBatch_PinnedCreateWithRouteIsNeitherReadNorCleared(t *testing.T) {
	f := newGuardedBatchFixture(t)
	pinned := f.prefix + "-gbpin"
	before := f.engine.gets.Load()
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{{Create: &beads.Bead{
			ID:       pinned,
			Title:    "pinned",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: testNewTarget},
		}}},
	}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if got := f.current(t, pinned).Metadata[beadmeta.RoutedToMetadataKey]; got != testNewTarget {
		t.Fatalf("pinned create gc.routed_to = %q, want %q: the batch did not land", got, testNewTarget)
	}
	if n := f.engine.gets.Load() - before; n != 0 {
		t.Errorf("a pinned create issued %d Get(s), want 0", n)
	}
	if n := f.engine.clears.Load(); n != 0 {
		t.Errorf("a pinned create ran %d clear(s), want 0", n)
	}
}

// After a clear moved the row's revision, the batch's result carries the
// post-clear revision, so a chained guarded batch built from it lands.
func TestRouteClearGuardedBatch_ResultRevisionsSurviveTheClear(t *testing.T) {
	f := newGuardedBatchFixture(t)
	res, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testNewTarget, &f.bead.Revision)},
	})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	assertGuardedLanded(t, f, f.bead.ID, testNewTarget)
	assertStampsCleared(t, f.engine, f.bead.ID)
	if got, want := res.Revisions[f.bead.ID], f.current(t, f.bead.ID).Revision; got != want {
		t.Fatalf("Revisions[%s] = %d, want the row's current revision %d", f.bead.ID, got, want)
	}
	next := res.Revisions[f.bead.ID]
	title := "chained"
	if _, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: f.bead.ID, ExpectedRevision: &next, Opts: beads.UpdateOpts{Title: &title}}}},
	}); err != nil {
		t.Fatalf("chained batch guarded by the returned revision: %v", err)
	}
	if got := f.current(t, f.bead.ID).Title; got != title {
		t.Fatalf("chained title = %q, want %q", got, title)
	}
}

// A clear that fails is logged; the batch's own result and nil error stand.
func TestRouteClearGuardedBatch_FailedClearDoesNotFailLandedBatch(t *testing.T) {
	logs := captureLog(t)
	f := newGuardedBatchFixture(t)
	f.engine.failClears = true
	res, err := f.batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testNewTarget, nil)},
	})
	if err != nil {
		t.Fatalf("ApplyGuarded = %v, want the landed batch's nil error", err)
	}
	assertGuardedLanded(t, f, f.bead.ID, testNewTarget)
	if _, ok := res.Revisions[f.bead.ID]; !ok {
		t.Errorf("result carries no revision for %s", f.bead.ID)
	}
	if !strings.Contains(logs.String(), "injected clear failure") {
		t.Errorf("want the failed clear logged; log:\n%s", logs.String())
	}
}

// A context canceled after the pre-reads stops the batch before any write.
func TestRouteClearGuardedBatch_ContextCanceledAfterPreReadWritesNothing(t *testing.T) {
	f := newGuardedBatchFixture(t)
	cause := errors.New("lane shut down")
	ctx, cancel := context.WithCancelCause(context.Background())
	f.engine.onGet = func() { cancel(cause) }
	_, err := f.batcher.ApplyGuarded(ctx, beads.GuardedBatch{
		Items: []beads.GuardedItem{f.reroute(f.bead.ID, testNewTarget, nil)},
	})
	if !errors.Is(err, cause) {
		t.Fatalf("ApplyGuarded = %v, want the cancellation cause %v", err, cause)
	}
	if n := f.engine.applies.Load(); n != 0 {
		t.Errorf("the engine saw %d batch(es), want none", n)
	}
	assertGuardedLanded(t, f, f.bead.ID, testOldTarget)
	assertStampsIntact(t, f.engine, f.bead.ID)
}

// The decorator answers a batcher only when its inner store has one.
func TestRouteClearGuardedBatch_HandleIsHonest(t *testing.T) {
	sqlite := beads.WithRouteChangeClearing(newSQLiteForConformance(t), identityNormalizer)
	if _, ok := beads.GuardedBatcherFor(sqlite); !ok {
		t.Error("GuardedBatcherFor(decorator over SQLite) = false, want the engine's batcher exposed")
	}
	bare := beads.WithRouteChangeClearing(targetingStore{Store: beads.NewMemStore()}, identityNormalizer)
	if _, ok := beads.GuardedBatcherFor(bare); ok {
		t.Error("GuardedBatcherFor(decorator over an inner with no batcher) = true, want false")
	}
}

// ApplyGuarded is not a method of the decorator: a method would answer for
// every inner, including one with no batcher, and lie about the capability.
func TestRouteClearGuardedBatch_DecoratorIsNotAStructuralBatcher(t *testing.T) {
	store := beads.WithRouteChangeClearing(newSQLiteForConformance(t), identityNormalizer)
	if _, ok := store.(beads.GuardedBatcher); ok {
		t.Error("the decorator must not satisfy GuardedBatcher structurally")
	}
	if _, ok := store.(beads.GuardedBatcherHandleProvider); !ok {
		t.Error("the decorator must answer GuardedBatcherHandle")
	}
}

// The decorated stack stays a conforming guarded batcher over each engine.
func TestRouteClearGuardedBatch_ConformanceThroughDecorator(t *testing.T) {
	decorate := func(open func(t *testing.T) beads.Store) func(t *testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			return beads.WithRouteChangeClearing(open(t), identityNormalizer)
		}
	}
	for _, engine := range []struct {
		name string
		open func(t *testing.T) beads.Store
	}{
		{"SQLiteStore", func(t *testing.T) beads.Store { return newSQLiteForConformance(t) }},
		{"MemStore", func(*testing.T) beads.Store { return beads.NewMemStore() }},
	} {
		beadstest.RunGuardedBatchConformance(t, "RouteClear/"+engine.name, decorate(engine.open))
		beadstest.RunGuardedBatchConformance(t, "CachingStore/RouteClear/"+engine.name, func(t *testing.T) beads.Store {
			return newCache(t, beads.WithRouteChangeClearing(engine.open(t), identityNormalizer))
		})
	}
}
