package splittest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/storeref"
)

// newTestProjection builds a projection over a fresh MemStore with its own
// sink, so a test inspects only what it caused.
func newTestProjection(t *testing.T) (*Projection, *beads.MemStore, *violationSink) {
	t.Helper()
	leaf := beads.NewMemStore()
	sink := newViolationSink()
	return projectionOf(leaf, sink), leaf, sink
}

func seed(t *testing.T, s beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("seeding %+v: %v", b, err)
	}
	return created
}

func sessionBead(title string) beads.Bead {
	return beads.Bead{Title: title, Type: "session", Labels: []string{"gc:session"}}
}

// policyWrapper stands in for cmd/gc's bead policy store: a pass-through that
// declares its inner store as the conditional-writes resolution target.
type policyWrapper struct{ beads.Store }

func (p *policyWrapper) ConditionalWritesResolveTarget() beads.Store { return p.Store }

func TestProjectionMintsInfrastructureBeadsUnderTheBindingPrefix(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	work := seed(t, leaf, beads.Bead{Title: "task"})
	session := seed(t, leaf, sessionBead("s"))
	if !strings.HasPrefix(work.ID, "gc-") {
		t.Fatalf("work bead id = %q, want gc-", work.ID)
	}
	if !strings.HasPrefix(session.ID, "gcg-") {
		t.Fatalf("session bead id = %q, want gcg- (the binding's prefix)", session.ID)
	}
	if p.Root() != beads.Store(leaf) {
		t.Fatal("Root() is not the physical store")
	}
}

func TestProjectionViewsAreMemoizedAndUnwrap(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	if projectionOf(leaf, nil) != p {
		t.Fatal("a second lookup built a second projection")
	}
	if projectionOf(p.WorkView(), nil) != p || projectionOf(p.ClassView(), nil) != p {
		t.Fatal("a view did not unwrap to its own projection")
	}
	if p.WorkView() == p.ClassView() {
		t.Fatal("work and class views are the same value; code branching on graph == city would not take its split path")
	}
	if p.WorkViewOf(p.ClassView()) != p.WorkView() {
		t.Fatal("the work view of a class view is not the work view over the same front")
	}
}

// TestProjectionThroughPolicyAndCacheSharesOneRoot is the controller's shape:
// the city store is a policy wrapper over a CachingStore over the physical
// store, and the class accessor wraps the same chain. One projection, the
// mint hook on the physical root, and a session created through the class
// view is the class view's to read.
func TestProjectionThroughPolicyAndCacheSharesOneRoot(t *testing.T) {
	leaf := beads.NewMemStore()
	cache := beads.NewCachingStore(leaf, nil)
	front := &policyWrapper{Store: cache}
	sink := newViolationSink()
	p := projectionOf(front, sink)
	if p.Root() != beads.Store(leaf) {
		t.Fatalf("root = %T, want the physical MemStore under the policy wrapper and the cache", p.Root())
	}
	if projectionOf(cache, sink) != p || projectionOf(leaf, sink) != p {
		t.Fatal("the cache and the leaf under the front did not resolve to the same projection")
	}
	class := p.ClassViewOf(front)
	created, err := class.Create(sessionBead("live"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.ID, "gcg-") {
		t.Fatalf("a session created through the controller's class view got %q; the mint hook did not reach the root", created.ID)
	}
	if got, err := class.Get(created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("class view Get = %+v, %v", got, err)
	}
	if sites := sink.summaries(); len(sites) != 0 || sink.testOnlyCount() != 0 {
		t.Fatalf("a legitimate session create and read were flagged: %+v (test-only %d)", sites, sink.testOnlyCount())
	}
}

// TestProjectionDoesNotStackViews: a cache built over a WORK view (the
// controller wraps whatever the open handed it) and then asked for its class
// view must not filter twice — the class view reads past the inner view.
func TestProjectionDoesNotStackViews(t *testing.T) {
	leaf := beads.NewMemStore()
	sink := newViolationSink()
	p := projectionOf(leaf, sink)
	cache := beads.NewCachingStore(p.WorkView(), nil)
	session := seed(t, leaf, sessionBead("s"))
	class := projectionOf(cache, sink).ClassViewOf(cache)
	if projectionOf(cache, sink) != p {
		t.Fatal("a cache over a view did not resolve to the view's projection")
	}
	if got, err := class.Get(session.ID); err != nil || got.ID != session.ID {
		t.Fatalf("class view over a cache over a work view: Get(session) = %+v, %v; views stacked", got, err)
	}
}

func TestProjectionHidesWrongClassRowsByID(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	task := seed(t, leaf, beads.Bead{Title: "task"})

	if _, err := p.WorkView().Get(session.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("work view Get(session) error = %v, want ErrNotFound", err)
	}
	if _, err := p.ClassView().Get(task.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("class view Get(task) error = %v, want ErrNotFound", err)
	}
	if got, err := p.ClassView().Get(session.ID); err != nil || got.ID != session.ID {
		t.Fatalf("class view Get(session) = %+v, %v", got, err)
	}
	if sites := sink.summaries(); len(sites) != 0 {
		t.Fatalf("test-only reads produced sites: %+v", sites)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("test-only violations = %d, want 2", got)
	}
}

func TestProjectionChargesAProductionCaller(t *testing.T) {
	SetPackageUnderTest("internal/storeref")
	t.Cleanup(func() { SetPackageUnderTest("") })
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))

	if _, err := storeref.Resolve(session.ID, []beads.Store{p.WorkView()}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve through the work view error = %v, want ErrNotFound", err)
	}
	sites := sink.summaries()
	if len(sites) != 1 {
		t.Fatalf("sites = %+v, want one", sites)
	}
	site := sites[0]
	want := SiteKey{File: "internal/storeref/storeref.go", Func: "storeref.Resolve", Op: "get", View: "work", Class: "sessions", Caller: "-"}
	if site.Key != want || len(site.Lines) != 1 || site.Lines[0] <= 0 {
		t.Fatalf("site = %+v, want key %+v at one line", site, want)
	}
	if site.SampleTest != "TestProjectionChargesAProductionCaller" || site.Tests != 1 {
		t.Fatalf("attribution = %q (%d tests), want this test", site.SampleTest, site.Tests)
	}
}

func TestProjectionMarkersCoverReadsOnly(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	err := beads.FederatedRead(func() error {
		_, err := storeref.Resolve(session.ID, []beads.Store{p.WorkView()})
		return err
	})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("federated Resolve error = %v, want ErrNotFound (still hidden)", err)
	}
	if sites := sink.summaries(); len(sites) != 0 {
		t.Fatalf("a federated read was flagged: %+v", sites)
	}
	// A write under the marker is still a violation.
	_ = beads.FederatedRead(func() error {
		return convoy.SetConvoyFields(p.WorkView(), session.ID, convoy.ConvoyFields{Owner: "x"})
	})
	sites := sink.summaries()
	if len(sites) != 1 || sites[0].Key.Op != "set-metadata" {
		t.Fatalf("a write under FederatedRead was not flagged: %+v", sites)
	}
}

func TestProjectionCountsRelicReadsAsDebt(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	_ = beads.RelicRead(func() error {
		_, err := storeref.Resolve(session.ID, []beads.Store{p.WorkView()})
		return err
	})
	if sites := sink.summaries(); len(sites) != 0 {
		t.Fatalf("a relic read was a violation: %+v", sites)
	}
	if relics := summarize(sink.sightings(), kindRelic); len(relics) != 1 || relics[0].Key.Func != "storeref.Resolve" {
		t.Fatalf("relic tally = %+v, want storeref.Resolve", relics)
	}
}

func TestProjectionListsHideWrongClassRowsAndFlagOnlySelectiveQueries(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	seed(t, leaf, sessionBead("s"))
	seed(t, leaf, beads.Bead{Title: "task"})

	broad, err := p.WorkView().List(beads.ListQuery{AllowScan: true})
	if err != nil || len(broad) != 1 || broad[0].Type != "task" {
		t.Fatalf("broad List through the work view = %+v, %v; want the task only", broad, err)
	}
	if open, err := p.WorkView().ListOpen(); err != nil || len(open) != 1 {
		t.Fatalf("ListOpen = %+v, %v", open, err)
	}
	if got := sink.testOnlyCount(); got != 0 {
		t.Fatalf("broad scans were flagged %d times; they merely filter unrelated rows", got)
	}
	if labeled, err := p.WorkView().ListByLabel("gc:session", 0); err != nil || len(labeled) != 0 {
		t.Fatalf("ListByLabel(gc:session) through the work view = %+v, %v; want none", labeled, err)
	}
	if typed, err := p.WorkView().List(beads.ListQuery{Type: "session"}); err != nil || len(typed) != 0 {
		t.Fatalf("List(type=session) through the work view = %+v, %v; want none", typed, err)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("selective reads flagged %d times, want 2", got)
	}
}

// TestProjectionAppliesAdvisoryPredicatesBeforeFlagging: ListQuery.ParentIDs
// and ReadyQuery.MetadataEquals and Unassigned are advisory, so a store may
// ignore them and answer a superset the caller re-filters (MemStore does). A
// hidden row they exclude was never selected: the view hides it without
// charging the caller, and charges only the hidden rows they select.
func TestProjectionAppliesAdvisoryPredicatesBeforeFlagging(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	parent := seed(t, leaf, beads.Bead{Title: "task"})
	seed(t, leaf, sessionBead("unrelated"))
	child := sessionBead("child")
	child.ParentID = parent.ID
	seed(t, leaf, child)
	work := p.WorkView()

	if _, err := work.List(beads.ListQuery{ParentIDs: []string{parent.ID}, AllowScan: true}); err != nil {
		t.Fatal(err)
	}
	if got := sink.testOnlyCount(); got != 1 {
		t.Fatalf("a ParentIDs list flagged %d hidden row(s), want 1: the session under the parent, not the unrelated one", got)
	}

	step := map[string]string{"gc.root_bead_id": "gcg-root"}
	tagged := map[string]string{"gc.root_bead_id": "gcg-root", "k": "v"}
	seed(t, leaf, beads.Bead{Title: "tagged step", Metadata: tagged})
	seed(t, leaf, beads.Bead{Title: "assigned tagged step", Assignee: "worker", Metadata: tagged})
	seed(t, leaf, beads.Bead{Title: "other step", Metadata: step})
	if _, err := work.Ready(beads.ReadyQuery{MetadataEquals: map[string]string{"k": "v"}, Unassigned: true}); err != nil {
		t.Fatal(err)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("flags after a MetadataEquals+Unassigned ready read = %d, want 2: only the unassigned tagged step is selected", got)
	}
}

// TestProjectionLimitAppliesAfterTheFilter: a limited list must not let
// hidden rows eat its budget (a split city's store never holds them).
func TestProjectionLimitAppliesAfterTheFilter(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	seed(t, leaf, sessionBead("s1"))
	seed(t, leaf, sessionBead("s2"))
	task := seed(t, leaf, beads.Bead{Title: "task"})
	got, err := p.WorkView().List(beads.ListQuery{AllowScan: true, Limit: 1})
	if err != nil || len(got) != 1 || got[0].ID != task.ID {
		t.Fatalf("List(limit 1) through the work view = %+v, %v; want the task", got, err)
	}
	ready, err := p.WorkView().Ready(beads.ReadyQuery{Limit: 1})
	if err != nil || len(ready) != 1 || ready[0].ID != task.ID {
		t.Fatalf("Ready(limit 1) through the work view = %+v, %v; want the task", ready, err)
	}
}

// partialFront answers lists with their rows AND a partial-result error, as
// CachingStore.List does when its backing read fails on an include-closed
// list (caching_store_reads.go). Callers that honor beads.IsPartialResult use
// those rows, so a view must filter them as it filters a complete answer.
type partialFront struct{ beads.Store }

func (f *partialFront) Backing() beads.Store { return f.Store }

func (f *partialFront) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := f.Store.List(q)
	return rows, partialResult(err)
}

func (f *partialFront) ListOpen(status ...string) ([]beads.Bead, error) {
	rows, err := f.Store.ListOpen(status...)
	return rows, partialResult(err)
}

func partialResult(err error) error {
	if err != nil {
		return err
	}
	return &beads.PartialResultError{Op: "test list", Err: errors.New("one tier did not answer")}
}

// TestProjectionFiltersAPartialResultAndKeepsItsError: rows that come with a
// partial-result error are filtered and flagged like a complete answer, the
// hidden row does not eat the limit, and the error passes through unchanged.
func TestProjectionFiltersAPartialResultAndKeepsItsError(t *testing.T) {
	leaf := beads.NewMemStore()
	front := &partialFront{Store: leaf}
	sink := newViolationSink()
	p := projectionOf(front, sink)
	tagged := map[string]string{"k": "v"}
	seed(t, leaf, beads.Bead{Title: "s", Type: "session", Labels: []string{"gc:session"}, Metadata: tagged})
	task := seed(t, leaf, beads.Bead{Title: "task", Metadata: tagged})
	work := p.WorkViewOf(front)

	got, err := work.List(beads.ListQuery{Metadata: tagged, Limit: 1})
	if !beads.IsPartialResult(err) {
		t.Fatalf("partial List error = %v, want the front's partial-result error unchanged", err)
	}
	if len(got) != 1 || got[0].ID != task.ID {
		t.Fatalf("partial List(limit 1) through the work view = %+v; want the task only (the hidden session must not eat the limit)", got)
	}
	if n := sink.testOnlyCount(); n != 1 {
		t.Fatalf("the session a partial List selected was flagged %d times, want 1", n)
	}
	open, err := work.ListOpen()
	if !beads.IsPartialResult(err) || len(open) != 1 || open[0].ID != task.ID {
		t.Fatalf("partial ListOpen through the work view = %+v, %v; want the task and the partial-result error", open, err)
	}
	if n := sink.testOnlyCount(); n != 1 {
		t.Fatalf("flags after a broad partial scan = %d, want still 1 (a broad scan only filters)", n)
	}
}

func TestProjectionRefusesWritesToWrongClassRows(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	task := seed(t, leaf, beads.Bead{Title: "task"})
	work := p.WorkView()

	title := "renamed"
	checks := map[string]error{
		"update":       work.Update(session.ID, beads.UpdateOpts{Title: &title}),
		"close":        work.Close(session.ID),
		"set-metadata": work.SetMetadata(session.ID, "k", "v"),
		"dep-add":      work.DepAdd(task.ID, session.ID, "blocks"),
		"delete":       work.Delete(session.ID),
	}
	for op, err := range checks {
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("%s of a session row through the work view: error = %v, want ErrNotFound", op, err)
		}
	}
	got, err := leaf.Get(session.ID)
	if err != nil || got.Status != "open" || got.Title != "s" || got.Metadata["k"] != "" {
		t.Fatalf("the refused writes changed the row: %+v, %v", got, err)
	}
}

// TestProjectionChecksConditionalAndCASWrites: the capability paths a caller
// discovers through the resolve target land on the view's checks too.
func TestProjectionChecksConditionalAndCASWrites(t *testing.T) {
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	work := p.WorkView()

	target := work.(beads.ConditionalWritesResolveTargeter).ConditionalWritesResolveTarget()
	if target == beads.Store(leaf) {
		t.Fatal("the resolve target is the raw store; capability lookups would bypass the view")
	}
	writer, ok := beads.ConditionalWriterFor(work)
	if !ok {
		t.Fatal("conditional writes were stripped")
	}
	if err := writer.UpdateIfMatch(session.ID, 1, beads.UpdateOpts{}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("UpdateIfMatch of a session row through the work view = %v, want ErrNotFound", err)
	}
	cas, ok := beads.MetadataCASWriterFor(work)
	if !ok {
		t.Fatal("metadata CAS was stripped")
	}
	if _, err := cas.CompareAndSetMetadataKey(session.ID, "k", "", "v"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("CAS of a session row through the work view = %v, want ErrNotFound", err)
	}
	if forTarget, ok := beads.ConditionalWriterForTarget(work); !ok {
		t.Fatal("ConditionalWriterForTarget found no writer")
	} else if err := forTarget.CloseIfMatch(session.ID, 1); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("CloseIfMatch through the target = %v, want ErrNotFound", err)
	}
	if got := sink.testOnlyCount(); got != 3 {
		t.Fatalf("checked conditional writes flagged %d times, want 3", got)
	}
	if mode, _, err := beads.ResolveConditionalWriter(work); err != nil || mode != nil {
		// MemStore carries no stamped mode: unset resolves to legacy through
		// the view exactly as it does on the leaf.
		leafMode, _, leafErr := beads.ResolveConditionalWriter(leaf)
		if (mode == nil) != (leafMode == nil) || (err == nil) != (leafErr == nil) {
			t.Fatalf("ResolveConditionalWriter through the view = %v, %v; on the leaf = %v, %v", mode, err, leafMode, leafErr)
		}
	}
}

func TestProjectionPinsAMisroutedCreateToTheViewThatWroteIt(t *testing.T) {
	p, _, sink := newTestProjection(t)
	misrouted, err := p.WorkView().Create(sessionBead("misrouted"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ClassView().Get(misrouted.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("class view sees a session the work view wrote: %v", err)
	}
	if _, err := p.WorkView().Get(misrouted.ID); err != nil {
		t.Fatalf("work view lost the row it wrote: %v", err)
	}
	if got := sink.testOnlyCount(); got != 2 {
		t.Fatalf("flags = %d, want 2 (the create, and the class view's miss)", got)
	}
}

func TestProjectionExposesCheckedOptionalCapabilities(t *testing.T) {
	p, leaf, _ := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	task := seed(t, leaf, beads.Bead{Title: "task"})
	work := p.WorkView()
	if _, ok := work.(beads.StorageCreateStore); ok {
		t.Fatal("a view over a MemStore claims CreateWithStorage the leaf does not have")
	}
	if counter, ok := work.(beads.Counter); !ok {
		t.Fatal("view is not a Counter")
	} else if _, err := counter.Count(context.Background(), beads.ListQuery{AllowScan: true}); !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported so callers list", err)
	}
	if rr, ok := work.(beads.ContextReadyReader); !ok {
		t.Fatal("view is not a ContextReadyReader")
	} else if _, err := rr.ReadyContext(context.Background()); err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	batch, err := work.(beads.DependencyBatchLister).DepListBatch([]string{session.ID, task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := batch[session.ID]; has {
		t.Fatal("DepListBatch answered for a session anchor the work view does not hold")
	}
	if _, _, err := work.(beads.ExactBatchGetter).GetExactBatch([]string{task.ID}); !errors.Is(err, beads.ErrExactBatchGetUnsupported) {
		t.Fatalf("GetExactBatch over a MemStore = %v, want ErrExactBatchGetUnsupported", err)
	}
	if _, err := work.(beads.ConditionalAssigneeTransferer).TransferIfCurrent(session.ID, "a", "b"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("TransferIfCurrent of a session row = %v, want ErrNotFound", err)
	}
	if kids, err := work.(beads.ChildrenBatchLister).ChildrenOfAny([]string{task.ID}); err != nil || len(kids) != 0 {
		t.Fatalf("ChildrenOfAny = %+v, %v", kids, err)
	}
}

func TestGateViewsAreIdentityWhenTheGateIsOff(t *testing.T) {
	t.Setenv(StrictEnv, "")
	leaf := beads.NewMemStore()
	if GateWorkView(leaf) != beads.Store(leaf) || GateClassView(leaf) != beads.Store(leaf) {
		t.Fatal("the gate wrapped a store while off")
	}
	if GateWorkView(nil) != nil {
		t.Fatal("a nil store did not stay nil")
	}
}

func TestGateViewsProjectWhenTheGateIsOn(t *testing.T) {
	t.Setenv(StrictEnv, "1")
	leaf := beads.NewMemStore()
	if GateWorkView(leaf) != ProjectionOf(leaf).WorkView() || GateClassView(leaf) != ProjectionOf(leaf).ClassView() {
		t.Fatal("the gate did not hand out the projection's views")
	}
}

// TestProjectionClaimsCacheProbesOnlyOverACache: a view over a plain store
// must not look like a cache (it would read as "never live"), and a view over
// a cache must answer for it.
func TestProjectionClaimsCacheProbesOnlyOverACache(t *testing.T) {
	plain := projectionOf(beads.NewMemStore(), newViolationSink())
	if _, ok := plain.WorkView().(interface{ IsLive() bool }); ok {
		t.Fatal("a view over a MemStore claims IsLive")
	}
	cache := beads.NewCachingStore(beads.NewMemStore(), nil)
	cached := projectionOf(cache, newViolationSink()).WorkViewOf(cache)
	live, ok := cached.(interface{ IsLive() bool })
	if !ok {
		t.Fatal("a view over a CachingStore does not answer IsLive")
	}
	if live.IsLive() != cache.IsLive() {
		t.Fatal("the view's IsLive disagrees with its cache's")
	}
}

// handlesFront is a pass-through front that declares explicit store handles
// without being a cache or a storage-tier creator, as cmd/gc's bead policy
// store does: its handles are the inner store's.
type handlesFront struct{ beads.Store }

func (f *handlesFront) Backing() beads.Store { return f.Store }

func (f *handlesFront) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(f.Store)
	handles.Writer = f
	return handles
}

// storageFront is a pass-through front with a storage-tier create and no
// explicit handles, as a bd or SQLite store is.
type storageFront struct{ beads.Store }

func (f *storageFront) Backing() beads.Store { return f.Store }

func (f *storageFront) CreateWithStorage(b beads.Bead, _ beads.StorageClass) (beads.Bead, error) {
	return f.Create(b)
}

// storageHandlesFront has both, as a strict kit leaf with a storage tier does.
type storageHandlesFront struct{ *handlesFront }

func (f *storageHandlesFront) CreateWithStorage(b beads.Bead, _ beads.StorageClass) (beads.Bead, error) {
	return f.Create(b)
}

// probesFront answers a cache's liveness probes without explicit handles.
type probesFront struct{ beads.Store }

func (f *probesFront) Backing() beads.Store    { return f.Store }
func (f *probesFront) IsLive() bool            { return true }
func (f *probesFront) Stats() beads.CacheStats { return beads.CacheStats{} }

// probesHandlesFront answers the probes and declares explicit handles.
type probesHandlesFront struct{ *handlesFront }

func (f *probesHandlesFront) IsLive() bool            { return true }
func (f *probesHandlesFront) Stats() beads.CacheStats { return beads.CacheStats{} }

// storageProbesFront has a storage-tier create and the probes, no handles.
type storageProbesFront struct{ *storageFront }

func (f *storageProbesFront) IsLive() bool            { return true }
func (f *storageProbesFront) Stats() beads.CacheStats { return beads.CacheStats{} }

// TestProjectionClaimsHandlesOnlyOverAFrontWithThem: a view mirrors the three
// optional capabilities production branches on (explicit handles, the cache
// probes, the storage-tier create) exactly as its front has them. A view over
// a plain store must not claim handles: cmd/gc's controller demand read pays an
// extra live Ready read for a store that has them.
func TestProjectionClaimsHandlesOnlyOverAFrontWithThem(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		front                    func(beads.Store) beads.Store
		handles, probes, storage bool
	}{
		{name: "plain store", front: func(s beads.Store) beads.Store { return s }},
		{name: "storage-tier create", front: func(s beads.Store) beads.Store { return &storageFront{Store: s} }, storage: true},
		{name: "explicit handles", front: func(s beads.Store) beads.Store { return &handlesFront{Store: s} }, handles: true},
		{name: "handles and storage", front: func(s beads.Store) beads.Store {
			return &storageHandlesFront{handlesFront: &handlesFront{Store: s}}
		}, handles: true, storage: true},
		{name: "cache probes", front: func(s beads.Store) beads.Store { return &probesFront{Store: s} }, probes: true},
		{name: "probes and handles", front: func(s beads.Store) beads.Store {
			return &probesHandlesFront{handlesFront: &handlesFront{Store: s}}
		}, handles: true, probes: true},
		{name: "probes and storage", front: func(s beads.Store) beads.Store {
			return &storageProbesFront{storageFront: &storageFront{Store: s}}
		}, probes: true, storage: true},
		{name: "CachingStore", front: func(s beads.Store) beads.Store { return beads.NewCachingStore(s, nil) }, handles: true, probes: true, storage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			front := tc.front(beads.NewMemStore())
			p := projectionOf(front, newViolationSink())
			for _, view := range []beads.Store{p.WorkViewOf(front), p.ClassViewOf(front)} {
				_, handles := view.(interface{ Handles() beads.StoreHandles })
				_, probes := view.(cacheProbes)
				_, storage := view.(beads.StorageCreateStore)
				if handles != tc.handles || probes != tc.probes || storage != tc.storage {
					t.Fatalf("%T over %T claims handles=%v probes=%v storage=%v; the front has handles=%v probes=%v storage=%v",
						view, front, handles, probes, storage, tc.handles, tc.probes, tc.storage)
				}
				if _, isView := view.(projectionView); !isView {
					t.Fatalf("%T is not a projection view", view)
				}
			}
		})
	}
}

// exactBatchStore is a MemStore with an exact batch read, as a bd or native
// Dolt backing has.
type exactBatchStore struct{ *beads.MemStore }

func (s *exactBatchStore) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	found := make(map[string]beads.Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		b, err := s.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			unresolved = append(unresolved, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		found[id] = b
	}
	return found, unresolved, nil
}

// TestProjectionViewReadsThroughTheFrontsExplicitHandles: beads.HandlesFor on a
// view over a front with explicit handles (a CachingStore, or the policy store
// over one) gets the view's own handles. Their Live reader reads the front's
// live handle, past a stale cache, as production does; their Cached reader
// reads the front's cache. Both hide and charge wrong-class rows exactly as the
// view's own reads do, and the Writer is the view, so writes keep its guards.
func TestProjectionViewReadsThroughTheFrontsExplicitHandles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front func(*beads.CachingStore) beads.Store
	}{
		{name: "CachingStore", front: func(c *beads.CachingStore) beads.Store { return c }},
		{name: "a handles-forwarding policy store over a CachingStore", front: func(c *beads.CachingStore) beads.Store { return &handlesFront{Store: c} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := &exactBatchStore{MemStore: beads.NewMemStore()}
			cache := beads.NewCachingStoreForTest(backing, nil)
			front := tc.front(cache)
			sink := newViolationSink()
			p := projectionOf(front, sink)
			task := seed(t, backing, beads.Bead{Title: "task", Assignee: "worker"})
			blocker := seed(t, backing, beads.Bead{Title: "blocker"})
			session := seed(t, backing, beads.Bead{Title: "s", Type: "session", Labels: []string{"gc:session"}, Assignee: "worker"})
			step := seed(t, backing, beads.Bead{Title: "step", Assignee: "worker", Metadata: map[string]string{"gc.root_bead_id": "gcg-root"}})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			// Out-of-process writes the cache has not seen: a pin, a new ready
			// task and a new edge, on the backing alone.
			if err := backing.SetMetadata(task.ID, "pin", "fresh"); err != nil {
				t.Fatal(err)
			}
			late := seed(t, backing, beads.Bead{Title: "late", Assignee: "worker"})
			if err := backing.DepAdd(task.ID, blocker.ID, "related"); err != nil {
				t.Fatal(err)
			}
			if cached, err := cache.Get(task.ID); err != nil || cached.Metadata["pin"] != "" {
				t.Fatalf("precondition: cached task = %+v, %v; want the pre-write row", cached.Metadata, err)
			}

			work := p.WorkViewOf(front)
			h := beads.HandlesFor(work)
			if w, ok := h.Writer.(beads.Store); !ok || w != work {
				t.Fatalf("the handles' Writer is %T, want the view itself so writes keep its guards", h.Writer)
			}

			if got, err := h.Live.Get(task.ID); err != nil || got.Metadata["pin"] != "fresh" {
				t.Fatalf("Live.Get through the work view = %+v, %v; want the backing's row, past the stale cache", got.Metadata, err)
			}
			if got, err := h.Cached.Get(task.ID); err != nil || got.Metadata["pin"] != "" {
				t.Fatalf("Cached.Get through the work view = %+v, %v; want the cache's pre-write row", got.Metadata, err)
			}
			if rows, err := h.Live.List(beads.ListQuery{Assignee: "worker"}); err != nil || !sameIDs(rows, task.ID, late.ID) {
				t.Fatalf("Live.List(assignee) through the work view = %v, %v; want the task and the late task", beadIDs(rows), err)
			}
			if rows, err := h.Cached.List(beads.ListQuery{Assignee: "worker"}); err != nil || !sameIDs(rows, task.ID) {
				t.Fatalf("Cached.List(assignee) through the work view = %v, %v; want the cached task alone", beadIDs(rows), err)
			}
			if rows, err := h.Live.Ready(beads.ReadyQuery{Assignee: "worker"}); err != nil || !sameIDs(rows, task.ID, late.ID) {
				t.Fatalf("Live.Ready(assignee) through the work view = %v, %v; want the task and the late task", beadIDs(rows), err)
			}
			if rows, err := h.Live.Ready(beads.ReadyQuery{Assignee: "worker", Limit: 1}); err != nil || len(rows) != 1 || rows[0].ID == step.ID {
				t.Fatalf("Live.Ready(assignee, limit 1) through the work view = %v, %v; want one work row (a hidden row must not eat the limit)", beadIDs(rows), err)
			}
			if rows, err := h.Cached.Ready(beads.ReadyQuery{Assignee: "worker"}); err != nil || !sameIDs(rows, task.ID) {
				t.Fatalf("Cached.Ready(assignee) through the work view = %v, %v; want the cached task alone", beadIDs(rows), err)
			}
			if deps, err := h.Live.DepList(task.ID, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
				t.Fatalf("Live.DepList through the work view = %+v, %v; want the backing's new edge", deps, err)
			}
			if deps, err := h.Cached.DepList(task.ID, "down"); err != nil || len(deps) != 0 {
				t.Fatalf("Cached.DepList through the work view = %+v, %v; want the cache's edge set (none)", deps, err)
			}
			// Each selective list hid and charged the session and the step (two
			// lists, two each); each ready read the step alone, the session
			// being ready-excluded (three reads).
			if got, want := sink.testOnlyCount(), 7; got != want {
				t.Fatalf("selective reads through the handles flagged %d hidden rows, want %d", got, want)
			}

			before := sink.testOnlyCount()
			// Wrong-class rows stay hidden through either reader, and are charged.
			for name, r := range map[string]beads.LiveReader{"Live": h.Live, "Cached": h.Cached} {
				if _, err := r.Get(session.ID); !errors.Is(err, beads.ErrNotFound) {
					t.Fatalf("%s.Get(session) through the work view: error = %v, want ErrNotFound", name, err)
				}
				if deps, err := r.DepList(step.ID, "down"); err != nil || len(deps) != 0 {
					t.Fatalf("%s.DepList(step) through the work view = %+v, %v; want no edges", name, deps, err)
				}
			}
			if got, want := sink.testOnlyCount()-before, 4; got != want {
				t.Fatalf("hidden by-id reads through the handles flagged %d times, want %d", got, want)
			}

			batch, ok := h.Live.(beads.ExactBatchGetter)
			if !ok {
				t.Fatal("the view's live handle drops the exact batch read beads.GetMany reads through")
			}
			found, unresolved, err := batch.GetExactBatch([]string{task.ID, session.ID})
			if err != nil {
				t.Fatalf("Live.GetExactBatch: %v", err)
			}
			if _, has := found[session.ID]; has || len(found) != 1 || found[task.ID].Metadata["pin"] != "fresh" {
				t.Fatalf("Live.GetExactBatch found %+v; want the fresh task alone", found)
			}
			if len(unresolved) != 1 || unresolved[0] != session.ID {
				t.Fatalf("Live.GetExactBatch unresolved = %v, want the session", unresolved)
			}
			many, err := beads.GetMany(work, []string{task.ID, session.ID, late.ID})
			if err != nil || len(many) != 2 || many[task.ID].Metadata["pin"] != "fresh" {
				t.Fatalf("beads.GetMany through the work view = %+v, %v; want the fresh task and the late task", many, err)
			}

			// The Writer is the view: a write to a wrong-class row is refused.
			if err := h.Writer.SetMetadata(session.ID, "k", "v"); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("Writer.SetMetadata(session) through the work view: error = %v, want ErrNotFound", err)
			}
		})
	}
}

func beadIDs(rows []beads.Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}

func sameIDs(rows []beads.Bead, want ...string) bool {
	if len(rows) != len(want) {
		return false
	}
	have := make(map[string]bool, len(rows))
	for _, b := range rows {
		have[b.ID] = true
	}
	for _, id := range want {
		if !have[id] {
			return false
		}
	}
	return true
}
