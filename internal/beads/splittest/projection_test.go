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
