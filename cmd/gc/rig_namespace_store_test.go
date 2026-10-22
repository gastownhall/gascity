package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The guarded store fixtures model city B with guarded rig "beta" on a store
// city A (rig "alpha") also writes. rgsSeed holds one row per class of the
// worked examples; want marks the rows B may act on.

type rgsSeedRow struct {
	bead beads.Bead
	keep bool
}

func rgsSeed() []rgsSeedRow {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []rgsSeedRow{
		{bead: beads.Bead{ID: "bt-own", Title: "own", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.intake")}, keep: true},
		{bead: beads.Bead{ID: "bt-bare", Title: "bare", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.intake")}},
		{bead: beads.Bead{ID: "bt-alpha", Title: "alpha", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.intake")}},
		{bead: beads.Bead{ID: "bt-ownpool", Title: "own pool", Status: "in_progress", Assignee: poolIDB}, keep: true},
		{bead: beads.Bead{ID: "bt-otherpool", Title: "other pool", Status: "in_progress", Assignee: poolIDA}},
		{bead: beads.Bead{ID: "bt-short", Title: "short", Status: "in_progress", Assignee: shortIDB}},
		{bead: beads.Bead{ID: "bt-person", Title: "person", Status: "in_progress", Assignee: "alice"}},
		{bead: beads.Bead{ID: "bt-mint", Title: "mint", Metadata: rnsMD(beadmeta.DeferredRoutedToMetadataKey, "beta/crew.worker", beadmeta.RunTargetMetadataKey, "crew.worker")}, keep: true},
		{bead: beads.Bead{ID: "bt-orphan", Title: "orphan", Metadata: rnsMD(beadmeta.SessionIDMetadataKey, poolIDB)}, keep: true},
		{bead: beads.Bead{ID: "bt-orphan-a", Title: "orphan a", Metadata: rnsMD(beadmeta.SessionIDMetadataKey, poolIDA)}},
		{bead: beads.Bead{ID: "bt-stamped", Title: "stamped", Metadata: rnsMD(rigNamespaceOwnerStampKey, ownerStampB)}, keep: true},
		{bead: beads.Bead{ID: "bt-stamped-a", Title: "stamped a", Metadata: rnsMD(rigNamespaceOwnerStampKey, "alpha/")}},
		{bead: beads.Bead{ID: "bt-unattr", Title: "unattributed"}, keep: true},
	}
	for i := range rows {
		b := &rows[i].bead
		b.Type = "task"
		if b.Status == "" {
			b.Status = "open"
		}
		b.CreatedAt = base.Add(time.Duration(i) * time.Minute)
	}
	return rows
}

func rgsSeedBeads() []beads.Bead {
	var out []beads.Bead
	for _, r := range rgsSeed() {
		out = append(out, r.bead)
	}
	return out
}

func rgsWantIDs() []string {
	var out []string
	for _, r := range rgsSeed() {
		if r.keep {
			out = append(out, r.bead.ID)
		}
	}
	sort.Strings(out)
	return out
}

func rgsIDs(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	sort.Strings(out)
	return out
}

// rgsCountingStore counts the discovery reads that reach the inner store.
type rgsCountingStore struct {
	beads.Store
	lists atomic.Int64
	ready atomic.Int64
}

func (s *rgsCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.List(q)
}

func (s *rgsCountingStore) ListByAssignee(a, st string, l int) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.ListByAssignee(a, st, l)
}

func (s *rgsCountingStore) ListByMetadata(f map[string]string, l int, o ...beads.QueryOpt) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.ListByMetadata(f, l, o...)
}

func (s *rgsCountingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.ready.Add(1)
	return s.Store.Ready(q...)
}

// rgsGuard returns city B's guard over a spy mem store holding rgsSeed, the
// mem store, and B's session store (to fail it).
func rgsGuard(t *testing.T) (*rigNamespaceStore, *beads.MemStore, *rnsCountingStore) {
	t.Helper()
	ns, sessions := rnsCityBNamespace(t)
	spy := &rgsSpyStore{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
	return newRigNamespaceStore(spy, ns), spy.MemStore, sessions
}

func assertIDs(t *testing.T, what string, got []beads.Bead, want []string) {
	t.Helper()
	if g := rgsIDs(got); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("%s returned %v, want %v", what, g, want)
	}
}

func TestRigNamespaceStoreDiscoveryReadsKeepOnlyThisCitysRows(t *testing.T) {
	g, _, _ := rgsGuard(t)
	want := rgsWantIDs()

	rows, err := g.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertIDs(t, "List", rows, want)

	open, err := g.ListOpen("")
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	for _, b := range open {
		if c := g.ns.classify(b); c == rowForeign {
			t.Fatalf("ListOpen returned Foreign row %s", b.ID)
		}
	}

	for _, h := range []struct {
		name string
		r    beads.CachedReader
	}{{"Handles().Cached", g.Handles().Cached}, {"Handles().Live", g.Handles().Live}} {
		rows, err := h.r.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatalf("%s.List: %v", h.name, err)
		}
		assertIDs(t, h.name+".List", rows, want)
	}
	if w := g.Handles().Writer; w != beads.Writer(g) {
		t.Fatalf("Handles().Writer = %T, want the guard itself: the inner writer would bypass it", w)
	}

	ready, err := g.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range ready {
		if g.ns.classify(b) == rowForeign {
			t.Fatalf("Ready returned Foreign row %s", b.ID)
		}
	}
}

func TestRigNamespaceStoreStructuralReadsPassThrough(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	if _, err := g.Get("bt-alpha"); err != nil {
		t.Fatalf("Get of a Foreign row: %v; reads by id are never filtered", err)
	}
	rows, err := g.List(beads.ListQuery{IDs: []string{"bt-alpha", "bt-own"}})
	if err != nil {
		t.Fatalf("List by IDs: %v", err)
	}
	assertIDs(t, "List{IDs}", rows, []string{"bt-alpha", "bt-own"})

	parent, err := mem.Create(beads.Bead{Title: "parent", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := mem.Create(beads.Bead{Title: "foreign child", Type: "task", ParentID: parent.ID, Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	kids, err := g.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	assertIDs(t, "Children", kids, []string{child.ID})
	byParent, err := g.List(beads.ListQuery{ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List{ParentID}", byParent, []string{child.ID})
	batch, err := g.ChildrenOfAny([]string{parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "ChildrenOfAny", batch, []string{child.ID})
	if err := mem.DepAdd("bt-own", child.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	deps, err := g.DepList("bt-own", "down")
	if err != nil || len(deps) != 1 {
		t.Fatalf("DepList = %v, %v; dependency edges to a Foreign row must stay visible", deps, err)
	}
}

func TestRigNamespaceStoreIdentityPredicatesGateBeforeReading(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsCountingStore{Store: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
	g := newRigNamespaceStore(inner, ns)

	refused := []struct {
		name string
		read func() ([]beads.Bead, error)
	}{
		{"assignee alice", func() ([]beads.Bead, error) { return g.List(beads.ListQuery{Assignee: "alice"}) }},
		{"short id", func() ([]beads.Bead, error) { return g.List(beads.ListQuery{Assignee: shortIDB}) }},
		{"other city's pool", func() ([]beads.Bead, error) { return g.List(beads.ListQuery{Assignee: poolIDA}) }},
		{"bare route", func() ([]beads.Bead, error) {
			return g.List(beads.ListQuery{Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.intake")})
		}},
		{"run target", func() ([]beads.Bead, error) {
			return g.List(beads.ListQuery{Metadata: rnsMD(beadmeta.RunTargetMetadataKey, "crew.worker")})
		}},
		{"run target, spaced key", func() ([]beads.Bead, error) {
			return g.List(beads.ListQuery{Metadata: rnsMD(" "+beadmeta.RunTargetMetadataKey+" ", "crew.worker")})
		}},
		{"assignees all foreign", func() ([]beads.Bead, error) {
			return g.List(beads.ListQuery{Assignees: []string{"alice", shortIDB}})
		}},
		{"list by assignee", func() ([]beads.Bead, error) { return g.ListByAssignee("alice", "", 0) }},
		{"list by metadata", func() ([]beads.Bead, error) {
			return g.ListByMetadata(rnsMD(beadmeta.ExecutionRoutedToMetadataKey, "alpha/x"), 0)
		}},
	}
	for _, tc := range refused {
		rows, err := tc.read()
		if err != nil || len(rows) != 0 {
			t.Fatalf("%s: got %v, %v; want empty", tc.name, rgsIDs(rows), err)
		}
	}
	if n := inner.lists.Load(); n != 0 {
		t.Fatalf("unadmitted identity predicates reached the inner store %d times; want no backing read", n)
	}
	if rows, _ := g.Ready(beads.ReadyQuery{Assignee: "alice"}); len(rows) != 0 || inner.ready.Load() != 0 {
		t.Fatalf("Ready{Assignee: alice} = %v after %d reads; want empty without reading", rgsIDs(rows), inner.ready.Load())
	}

	rows, err := g.List(beads.ListQuery{Assignee: poolIDB})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List{own pool}", rows, []string{"bt-ownpool"})
	rows, err = g.List(beads.ListQuery{Assignees: []string{"alice", poolIDB}})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List{Assignees: alice, own pool}", rows, []string{"bt-ownpool"})
	rows, err = g.List(beads.ListQuery{Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.intake")})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List{route beta/...}", rows, []string{"bt-own"})
}

// rgsOrderedStore answers bounded ordered ready reads in created order.
type rgsOrderedStore struct {
	*beads.MemStore
	reads atomic.Int64
}

func (s *rgsOrderedStore) ReadyOrdered(q beads.ReadyQuery, _ beads.ReadyOrder) ([]beads.Bead, bool, error) {
	s.reads.Add(1)
	limit := q.Limit
	q.Limit = 0
	rows, err := s.Ready(q)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, true, nil
}

func TestRigNamespaceStoreBoundedReadsRefillThePage(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var seed []beads.Bead
	for i := 0; i < 50; i++ {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("bt-f%02d", i), Title: "foreign", Type: "task", Status: "open", Labels: []string{"lbl"},
			CreatedAt: base.Add(time.Duration(i) * time.Second), Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.intake"),
		})
	}
	seed = append(seed, beads.Bead{
		ID: "bt-mine", Title: "mine", Type: "task", Status: "open", Labels: []string{"lbl"},
		CreatedAt: base.Add(time.Hour), Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.intake"),
	})
	inner := &rgsOrderedStore{MemStore: beads.NewMemStoreFrom(1000, seed, nil)}
	g := newRigNamespaceStore(inner, ns)

	rows, err := g.List(beads.ListQuery{Status: "open", Limit: 1, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "List{Limit: 1}", rows, []string{"bt-mine"})

	rows, ordered, err := g.ReadyOrdered(beads.ReadyQuery{Limit: 1}, beads.ReadyOrderOldest)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "ReadyOrdered{Limit: 1}", rows, []string{"bt-mine"})
	if !ordered {
		t.Fatal("ReadyOrdered reported ordered=false for a page that is a correct prefix")
	}
	if inner.reads.Load() < 2 {
		t.Fatalf("ReadyOrdered read %d times; a page emptied by the filter must be re-read with a larger window", inner.reads.Load())
	}

	// A short page still means exhausted: a limit past every kept row
	// returns them all, ordered.
	rows, ordered, err = g.ReadyOrdered(beads.ReadyQuery{Limit: 5}, beads.ReadyOrderOldest)
	if err != nil || !ordered {
		t.Fatalf("ReadyOrdered{Limit: 5} = ordered %v, err %v", ordered, err)
	}
	assertIDs(t, "ReadyOrdered{Limit: 5}", rows, []string{"bt-mine"})

	labeled, err := g.ListByLabel("lbl", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, "ListByLabel{limit 1}", labeled, []string{"bt-mine"})
}

func TestRigNamespaceStoreCountAgreesWithList(t *testing.T) {
	g, _, _ := rgsGuard(t)
	n, err := g.Count(context.Background(), beads.ListQuery{Assignee: "alice"})
	if err != nil || n != 0 {
		t.Fatalf("Count{Assignee: alice} = %d, %v; want 0, nil (List returns nothing)", n, err)
	}
	if _, err := g.Count(context.Background(), beads.ListQuery{AllowScan: true}); !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("Count of a filtered read = %v; want ErrCountUnsupported so callers count List", err)
	}
}

func TestRigNamespaceStoreReportsPartialResultsOnClassificationErrors(t *testing.T) {
	g, _, sessions := rgsGuard(t)
	sessions.setErr(errors.New("sessions store down"))
	rows, err := g.List(beads.ListQuery{AllowScan: true})
	if !beads.IsPartialResult(err) {
		t.Fatalf("List with an unreadable sessions store = %v; want a partial result", err)
	}
	for _, b := range rows {
		if b.ID == "bt-ownpool" || b.ID == "bt-orphan" {
			t.Fatalf("List kept %s, whose admission needs the unreadable sessions store", b.ID)
		}
	}
	if got := rgsIDs(rows); !containsString(got, "bt-own") {
		t.Fatalf("List dropped the lexically owned row: %v", got)
	}
	cache := beads.NewCachingStoreForTest(beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanNS, _ := rnsCityBNamespace(t)
	clean, ok := newRigNamespaceStore(cache, cleanNS).CachedList(beads.ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("precondition: CachedList over a primed cache declined a clean read")
	}
	assertIDs(t, "CachedList over a primed cache", clean, rgsWantIDs())
	// A fresh resolver, so no answer is remembered from the clean read.
	downNS, downSessions := rnsCityBNamespace(t)
	downSessions.setErr(errors.New("sessions store down"))
	if _, ok := newRigNamespaceStore(cache, downNS).CachedList(beads.ListQuery{AllowScan: true}); ok {
		t.Fatal("CachedList answered a partial read as complete")
	}
	if _, err := g.List(beads.ListQuery{Assignee: poolIDB2}); !beads.IsPartialResult(err) {
		t.Fatalf("List{Assignee: undecidable pool id} = %v; want a partial result", err)
	}
}

// ---------------------------------------------------------------------------
// Writes.

// rgsSpyStore records the claim-shaped calls that reach it and answers them
// with err.
type rgsSpyStore struct {
	*beads.MemStore
	calls []string
	err   error
}

var (
	_ beads.ConditionalAssigneeTransferer = (*rgsSpyStore)(nil)
	_ beads.AssignmentGuardedUpdater      = (*rgsSpyStore)(nil)
	_ beads.BatchDeleter                  = (*rgsSpyStore)(nil)
	_ beads.AtomicConditionalCloser       = (*rgsSpyStore)(nil)
	_ beads.GraphApplyStore               = (*rgsSpyStore)(nil)
)

func (s *rgsSpyStore) record(call string, args ...any) {
	s.calls = append(s.calls, fmt.Sprint(append([]any{call}, args...)...))
}

func (s *rgsSpyStore) TransferIfCurrent(id, from, to string) (bool, error) {
	s.record("transfer", id, from, to)
	return s.err == nil, s.err
}

func (s *rgsSpyStore) UpdateIfAssignment(id, status, assignee string, opts beads.UpdateOpts) (bool, error) {
	s.record("update-if-assignment", id, status, assignee, opts.Title)
	return s.err == nil, s.err
}

func (s *rgsSpyStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.record("claim", id, assignee)
	return beads.Bead{ID: id, Assignee: assignee}, s.err == nil, s.err
}

func (s *rgsSpyStore) DeleteBatch(ids []string) error {
	s.record("delete-batch", ids)
	return s.err
}

func (s *rgsSpyStore) CloseWithMetadataIfMatch(id string, rev int64, md map[string]string) (beads.Bead, error) {
	s.record("close-with-metadata", id, rev, md)
	return beads.Bead{ID: id}, s.err
}

func (s *rgsSpyStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.record("graph-apply", len(plan.Nodes))
	return &beads.GraphApplyResult{}, s.err
}

func TestRigNamespaceStoreFencesEveryWriteToAForeignRow(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	foreignIDs := []string{"bt-bare", "bt-alpha", "bt-otherpool", "bt-short", "bt-person", "bt-orphan-a", "bt-stamped-a"}
	strp := func(s string) *string { return &s }
	for _, id := range foreignIDs {
		spy := &rgsSpyStore{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}
		g := newRigNamespaceStore(spy, ns)
		before, err := spy.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		cw, ok := g.ConditionalWriterHandle()
		if !ok {
			t.Fatal("the guard over a mem store hides its conditional writer")
		}
		cas, _ := g.MetadataCASWriterHandle()
		closer, ok := g.AtomicConditionalCloserHandle()
		if !ok {
			t.Fatal("the guard hides the inner store's atomic closer")
		}
		batcher, ok := g.GuardedBatcherHandle()
		if !ok {
			t.Fatal("the guard hides the inner store's guarded batches")
		}
		applier, ok := g.GraphApplyHandle()
		if !ok {
			t.Fatal("the guard hides the inner store's graph apply")
		}
		rev := before.Revision
		writes := map[string]func() error{
			"Update":          func() error { return g.Update(id, beads.UpdateOpts{Title: strp("x")}) },
			"UpdateReporting": func() error { _, err := g.UpdateReporting(id, beads.UpdateOpts{Title: strp("x")}); return err },
			"SetMetadata":     func() error { return g.SetMetadata(id, "k", "v") },
			"SetMetadataBatch": func() error {
				return g.SetMetadataBatch(id, map[string]string{"k": "v"})
			},
			"Close":         func() error { return g.Close(id) },
			"Reopen":        func() error { return g.Reopen(id) },
			"CloseAll":      func() error { _, err := g.CloseAll([]string{"bt-own", id}, nil); return err },
			"Delete":        func() error { return g.Delete(id) },
			"DeleteBatch":   func() error { return g.DeleteBatch([]string{id}) },
			"DepAdd":        func() error { return g.DepAdd(id, "bt-own", "blocks") },
			"DepRemove":     func() error { return g.DepRemove(id, "bt-own") },
			"UpdateIfMatch": func() error { return cw.UpdateIfMatch(id, rev, beads.UpdateOpts{Title: strp("x")}) },
			"CloseIfMatch":  func() error { return cw.CloseIfMatch(id, rev) },
			"DeleteIfMatch": func() error { return cw.DeleteIfMatch(id, rev) },
			"CompareAndSetMetadataKey": func() error {
				_, err := cas.CompareAndSetMetadataKey(id, "k", "", "v")
				return err
			},
			"CloseWithMetadataIfMatch": func() error { _, err := closer.CloseWithMetadataIfMatch(id, rev, nil); return err },
			"ReleaseIfCurrent":         func() error { _, err := g.ReleaseIfCurrent(id, before.Assignee); return err },
			"TransferIfCurrent":        func() error { _, err := g.TransferIfCurrent(id, before.Assignee, poolIDB); return err },
			"UpdateIfAssignment": func() error {
				_, err := g.UpdateIfAssignment(id, before.Status, before.Assignee, beads.UpdateOpts{Title: strp("x")})
				return err
			},
			"Claim": func() error { _, _, err := g.Claim(id, poolIDB); return err },
			"ApplyGuarded": func() error {
				_, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: id, Opts: beads.UpdateOpts{Title: strp("x")}}}}})
				return err
			},
			"ApplyGraphPlan edge": func() error {
				_, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
					Nodes: []beads.GraphApplyNode{{Key: "n", Title: "n"}},
					Edges: []beads.GraphApplyEdge{{FromID: id, ToKey: "n"}},
				})
				return err
			},
			"Tx Update": func() error {
				return g.Tx("t", func(tx beads.Tx) error { return tx.Update(id, beads.UpdateOpts{Title: strp("x")}) })
			},
			"Tx Close": func() error { return g.Tx("t", func(tx beads.Tx) error { return tx.Close(id) }) },
			"Tx SetMetadataBatch": func() error {
				return g.Tx("t", func(tx beads.Tx) error { return tx.SetMetadataBatch(id, map[string]string{"k": "v"}) })
			},
		}
		for name, write := range writes {
			err := write()
			var nsErr *rigNamespaceError
			if !errors.Is(err, errRigNamespace) || !errors.As(err, &nsErr) {
				t.Fatalf("%s on Foreign row %s = %v; want a *rigNamespaceError", name, id, err)
			}
		}
		after, err := spy.Get(id)
		if err != nil {
			t.Fatalf("Foreign row %s is gone after refused writes: %v", id, err)
		}
		if after.Revision != before.Revision || after.Title != before.Title || after.Status != before.Status {
			t.Fatalf("Foreign row %s changed under refused writes: %+v -> %+v", id, before, after)
		}
		if len(spy.calls) != 0 {
			t.Fatalf("refused writes to %s reached the inner store: %v", id, spy.calls)
		}
	}
}

func TestRigNamespaceStoreStampsEveryCreate(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	spy := &rgsSpyStore{MemStore: beads.NewMemStore()}
	g := newRigNamespaceStore(spy, ns)

	created, err := g.Create(beads.Bead{Title: "a", Type: "task", Metadata: map[string]string{
		rigNamespaceOwnerStampKey:              "alpha/",
		" " + rigNamespaceOwnerStampKey + "  ": "alpha/",
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stored, _ := spy.Get(created.ID)
	if got := stored.Metadata[rigNamespaceOwnerStampKey]; got != ownerStampB {
		t.Fatalf("creation stamp = %q, want %q (the guard replaces a caller's value)", got, ownerStampB)
	}
	for k := range stored.Metadata {
		if k != rigNamespaceOwnerStampKey && strings.TrimSpace(k) == rigNamespaceOwnerStampKey {
			t.Fatalf("a spaced spelling of the creation stamp key survived the create: %q", k)
		}
	}
	if g.ns.classify(stored) != rowOwned {
		t.Fatal("a row the guard created is not Owned")
	}

	created, err = g.CreateWithStorage(beads.Bead{Title: "b", Type: "task"}, beads.StorageEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := spy.Get(created.ID); stored.Metadata[rigNamespaceOwnerStampKey] != ownerStampB {
		t.Fatal("CreateWithStorage did not stamp the row")
	}

	if err := g.Tx("t", func(tx beads.Tx) error {
		b, err := tx.Create(beads.Bead{Title: "c", Type: "task"})
		if err != nil {
			return err
		}
		// A row this transaction created is this city's own.
		return tx.SetMetadataBatch(b.ID, map[string]string{beadmeta.RoutedToMetadataKey: "alpha/x"})
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	batcher, _ := g.GuardedBatcherHandle()
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Create: &beads.Bead{ID: "bt-pinned", Title: "p", Type: "task"}}}}); err != nil {
		t.Fatalf("ApplyGuarded create: %v", err)
	}
	if stored, _ := spy.Get("bt-pinned"); stored.Metadata[rigNamespaceOwnerStampKey] != ownerStampB {
		t.Fatal("a guarded batch create was not stamped")
	}

	var applied *beads.GraphApplyPlan
	applier := rigNamespaceGraphApplier{s: g, inner: graphApplierFunc(func(_ context.Context, p *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
		applied = p
		return &beads.GraphApplyResult{}, nil
	})}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "n", Title: "n", Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}}}}
	if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if applied.Nodes[0].Metadata[rigNamespaceOwnerStampKey] != ownerStampB {
		t.Fatal("a graph node was not stamped")
	}
	if plan.Nodes[0].Metadata[rigNamespaceOwnerStampKey] != "alpha/" {
		t.Fatal("the guard mutated the caller's plan instead of copying it")
	}

	refusedCreates := map[string]beads.Bead{
		"bare route":         {Title: "x", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.intake")},
		"bare deferred":      {Title: "x", Metadata: rnsMD(beadmeta.DeferredRoutedToMetadataKey, "crew.intake")},
		"foreign assignee":   {Title: "x", Assignee: "alice"},
		"short id assignee":  {Title: "x", Assignee: shortIDB},
		"foreign deferred a": {Title: "x", Metadata: rnsMD(beadmeta.DeferredAssigneeMetadataKey, "alice")},
		"empty segment":      {Title: "x", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta//x")},
	}
	for name, b := range refusedCreates {
		if _, err := g.Create(b); !errors.Is(err, errRigNamespace) {
			t.Fatalf("Create with %s = %v; want refused", name, err)
		}
	}
	if _, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "n", Title: "n", MetadataRefs: map[string]string{rigNamespaceOwnerStampKey: "n"}}}}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a graph metadata reference into the creation stamp = %v; want refused", err)
	}

	deny := newRigNamespaceStore(beads.NewMemStore(), denyAllRigNamespace(nil))
	if _, err := deny.Create(beads.Bead{Title: "x"}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a deny-all namespace created a row: %v", err)
	}
}

type graphApplierFunc func(context.Context, *beads.GraphApplyPlan) (*beads.GraphApplyResult, error)

func (f graphApplierFunc) ApplyGraphPlan(ctx context.Context, p *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	return f(ctx, p)
}

func TestRigNamespaceStoreCreationStampIsWriteOnce(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	cw, _ := g.ConditionalWriterHandle()
	closer, _ := g.AtomicConditionalCloserHandle()
	stamped, _ := mem.Get("bt-stamped")
	for name, write := range map[string]func() error{
		"set on Owned row": func() error { return g.SetMetadata("bt-own", rigNamespaceOwnerStampKey, ownerStampB) },
		"change":           func() error { return g.SetMetadata("bt-stamped", rigNamespaceOwnerStampKey, "beta/x") },
		"spaced key":       func() error { return g.SetMetadata("bt-stamped", " "+rigNamespaceOwnerStampKey, "alpha/") },
		"unset": func() error {
			return g.Update("bt-stamped", beads.UpdateOpts{UnsetMetadata: []string{rigNamespaceOwnerStampKey}})
		},
		"case-folded variant": func() error { return g.SetMetadata("bt-stamped", rigNamespaceOwnerStampKey, "Beta/") },
		"set on unattributed": func() error { return g.SetMetadata("bt-unattr", rigNamespaceOwnerStampKey, ownerStampB) },
		"compare and set": func() error {
			_, err := cw.CompareAndSetMetadataKey("bt-stamped", rigNamespaceOwnerStampKey, ownerStampB, "")
			return err
		},
		"close with metadata": func() error {
			_, err := closer.CloseWithMetadataIfMatch("bt-stamped", stamped.Revision, map[string]string{rigNamespaceOwnerStampKey: ""})
			return err
		},
		"update if match": func() error {
			return cw.UpdateIfMatch("bt-stamped", stamped.Revision, beads.UpdateOpts{Metadata: map[string]string{rigNamespaceOwnerStampKey: "x/"}})
		},
		"tx metadata on owned": func() error {
			return g.Tx("t", func(tx beads.Tx) error {
				return tx.SetMetadataBatch("bt-own", map[string]string{rigNamespaceOwnerStampKey: ownerStampB})
			})
		},
	} {
		if err := write(); !errors.Is(err, errRigNamespace) {
			t.Fatalf("%s: %v; want refused", name, err)
		}
	}
	if err := g.SetMetadata("bt-stamped", rigNamespaceOwnerStampKey, ownerStampB); err != nil {
		t.Fatalf("re-sending the stored stamp unchanged: %v", err)
	}
	if err := g.Update("bt-own", beads.UpdateOpts{UnsetMetadata: []string{rigNamespaceOwnerStampKey}}); err != nil {
		t.Fatalf("unsetting an absent stamp is a no-op: %v", err)
	}
}

func TestRigNamespaceStoreAssigneeAndRouteRules(t *testing.T) {
	g, _, _ := rgsGuard(t)
	strp := func(s string) *string { return &s }
	refused := map[string]func() error{
		"claim for alice":          func() error { _, _, err := g.Claim("bt-unattr", "alice"); return err },
		"assign a short id":        func() error { return g.Update("bt-own", beads.UpdateOpts{Assignee: strp(shortIDB)}) },
		"assign another city pool": func() error { return g.Update("bt-own", beads.UpdateOpts{Assignee: strp(poolIDA)}) },
		"bare route on own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.RoutedToMetadataKey, "crew.intake")
		},
		"foreign route on unattributed row": func() error {
			return g.SetMetadata("bt-unattr", beadmeta.RoutedToMetadataKey, "alpha/crew.intake")
		},
		"foreign deferred assignee on unattributed row": func() error {
			return g.SetMetadata("bt-unattr", beadmeta.DeferredAssigneeMetadataKey, "alice")
		},
		"release from a foreign holder": func() error { _, err := g.ReleaseIfCurrent("bt-own", "alice"); return err },
		"transfer to a foreign holder":  func() error { _, err := g.TransferIfCurrent("bt-ownpool", poolIDB, "alice"); return err },
	}
	for name, write := range refused {
		if err := write(); !errors.Is(err, errRigNamespace) {
			t.Fatalf("%s: %v; want refused", name, err)
		}
	}
	allowed := map[string]func() error{
		"hand-off of an own row": func() error {
			return g.SetMetadata("bt-own", beadmeta.RoutedToMetadataKey, "alpha/crew.intake")
		},
		"admitted route on unattributed row": func() error {
			return g.SetMetadata("bt-unattr", beadmeta.RoutedToMetadataKey, "beta/crew.worker")
		},
		"assign own pool session": func() error { return g.Update("bt-mint", beads.UpdateOpts{Assignee: strp(poolIDB)}) },
		"release own pool row":    func() error { return g.Update("bt-ownpool", beads.UpdateOpts{Assignee: strp("")}) },
		"close own row":           func() error { return g.Close("bt-orphan") },
	}
	for name, write := range allowed {
		if err := write(); err != nil {
			t.Fatalf("%s: %v; want allowed", name, err)
		}
	}
}

func TestRigNamespaceStoreErrorMessage(t *testing.T) {
	g, _, _ := rgsGuard(t)
	err := g.Close("bt-person")
	if err == nil {
		t.Fatal("closing a person's row was allowed")
	}
	msg := err.Error()
	for _, want := range []string{`rig "beta"`, `federation = "rig"`, "close of bt-person", `"alice"`, "beta/"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q does not mention %q", msg, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Each write path keeps every value rule and its fence; these pin each path
// on its own.

// rgsFlakyGetStore fails every read by id with a transient error while its
// writes still land, as a busy server can.
type rgsFlakyGetStore struct {
	*rgsSpyStore
}

func (s *rgsFlakyGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("transient read failure")
}

func (s *rgsFlakyGetStore) Update(id string, opts beads.UpdateOpts) error {
	s.record("update", id)
	return s.MemStore.Update(id, opts)
}

func TestRigNamespaceStoreFenceRowReadErrorFailsClosed(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsFlakyGetStore{rgsSpyStore: &rgsSpyStore{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}}
	g := newRigNamespaceStore(inner, ns)
	title := "x"
	if err := g.Update("bt-person", beads.UpdateOpts{Title: &title}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("Update of a row whose live read failed = %v; want a refusal", err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("a write the guard could not judge reached the inner store: %v", inner.calls)
	}
}

func TestRigNamespaceStoreCloseAllMetadataFollowsTheValueRules(t *testing.T) {
	g, _, _ := rgsGuard(t)
	if _, err := g.CloseAll([]string{"bt-own"}, map[string]string{rigNamespaceOwnerStampKey: "alpha/"}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("CloseAll setting another namespace's creation stamp = %v; want refused", err)
	}
	if _, err := g.CloseAll([]string{"bt-unattr"}, map[string]string{beadmeta.RoutedToMetadataKey: "alpha/crew.intake"}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("CloseAll routing an unattributed row outside the namespace = %v; want refused", err)
	}
}

type rgsDepMetaStore struct {
	*rgsSpyStore
}

func (s *rgsDepMetaStore) DepAddWithMetadata(issueID, dependsOnID, _, _ string) error {
	s.record("dep-add-with-metadata", issueID, dependsOnID)
	return s.err
}

func TestRigNamespaceStoreDepAddWithMetadataFencesTheIssueSide(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsDepMetaStore{rgsSpyStore: &rgsSpyStore{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}}
	g := newRigNamespaceStore(inner, ns)
	if err := g.DepAddWithMetadata("bt-person", "bt-own", "blocks", `{"k":"v"}`); !errors.Is(err, errRigNamespace) {
		t.Fatalf("DepAddWithMetadata on a Foreign row = %v; want refused", err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("a refused edge write reached the inner store: %v", inner.calls)
	}
}

func TestRigNamespaceStoreTxCreateIsStamped(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	var id string
	if err := g.Tx("t", func(tx beads.Tx) error {
		b, err := tx.Create(beads.Bead{Title: "c", Type: "task", Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}})
		id = b.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := mem.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Metadata[rigNamespaceOwnerStampKey]; got != ownerStampB {
		t.Fatalf("a transaction's create carries stamp %q, want %q", got, ownerStampB)
	}
	if err := g.Tx("t", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{Title: "d", Type: "task", Assignee: "alice"})
		return err
	}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a transaction's create for a foreign assignee = %v; want refused", err)
	}
}

func TestRigNamespaceStoreUpdateIfAssignmentChecksValues(t *testing.T) {
	g, _, _ := rgsGuard(t)
	alice := "alice"
	if _, err := g.UpdateIfAssignment("bt-ownpool", "in_progress", poolIDB, beads.UpdateOpts{Assignee: &alice}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("UpdateIfAssignment handing an own row to alice = %v; want refused", err)
	}
	if _, err := g.UpdateIfAssignment("bt-ownpool", "in_progress", poolIDB, beads.UpdateOpts{Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("UpdateIfAssignment setting the creation stamp = %v; want refused", err)
	}
}

type rgsBatchSpy struct {
	got beads.GuardedBatch
}

func (b *rgsBatchSpy) ApplyGuarded(_ context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	b.got = batch
	return beads.GuardedResult{}, nil
}

func TestRigNamespaceStoreGuardedBatchPinsTheJudgedRevision(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	spy := &rgsBatchSpy{}
	batcher := rigNamespaceBatcher{s: g, inner: spy}
	title := "x"
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: "bt-own", Opts: beads.UpdateOpts{Title: &title}}}}}); err != nil {
		t.Fatal(err)
	}
	row, err := mem.Get("bt-own")
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.got.Items) != 1 || spy.got.Items[0].Update == nil {
		t.Fatalf("the batch reached the inner batcher as %+v", spy.got)
	}
	u := spy.got.Items[0].Update
	if u.ExpectedRevision == nil || *u.ExpectedRevision != row.Revision {
		t.Fatalf("an unguarded batch update was not pinned to the judged revision %d: %v", row.Revision, u.ExpectedRevision)
	}
}

func TestRigNamespaceStoreGuardedBatchChecksUpdateValues(t *testing.T) {
	g, _, _ := rgsGuard(t)
	batcher, ok := g.GuardedBatcherHandle()
	if !ok {
		t.Fatal("no guarded batcher")
	}
	alice := "alice"
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: "bt-own", Opts: beads.UpdateOpts{Assignee: &alice}}}}}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a guarded batch handing an own row to alice = %v; want refused", err)
	}
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: "bt-stamped", Opts: beads.UpdateOpts{Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}}}}}}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a guarded batch changing the creation stamp = %v; want refused", err)
	}
}

type rgsForeignIDStore struct {
	*rgsSpyStore
	last beads.Bead
}

func (s *rgsForeignIDStore) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	s.record("create-foreign-id", b.ID)
	s.last = b
	return b, s.err
}

func TestRigNamespaceStoreCreateWithForeignIDIsStamped(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := &rgsForeignIDStore{rgsSpyStore: &rgsSpyStore{MemStore: beads.NewMemStore()}}
	g := newRigNamespaceStore(inner, ns)
	created, err := g.CreateWithForeignID(beads.Bead{ID: "bt-x", Title: "x", Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}})
	if err != nil || created.ID != "bt-x" {
		t.Fatalf("CreateWithForeignID = %+v, %v", created, err)
	}
	if got := inner.last.Metadata[rigNamespaceOwnerStampKey]; got != ownerStampB {
		t.Fatalf("CreateWithForeignID wrote stamp %q, want %q", got, ownerStampB)
	}
	if _, err := g.CreateWithForeignID(beads.Bead{ID: "bt-y", Title: "y", Assignee: "alice"}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("CreateWithForeignID for a foreign assignee = %v; want refused", err)
	}
}

func TestRigNamespaceStoreUpdateReportingChecksValues(t *testing.T) {
	g, _, _ := rgsGuard(t)
	alice := "alice"
	if row, err := g.UpdateReporting("bt-own", beads.UpdateOpts{Assignee: &alice}); !errors.Is(err, errRigNamespace) || row.Status != "" {
		t.Fatalf("UpdateReporting handing an own row to alice = %+v, %v; want refused", row, err)
	}
	if _, err := g.UpdateReporting("bt-stamped", beads.UpdateOpts{Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("UpdateReporting changing the creation stamp = %v; want refused", err)
	}
}

func TestErrRigNamespaceIsTheConfigSentinel(t *testing.T) {
	g, _, _ := rgsGuard(t)
	if err := g.Close("bt-person"); !errors.Is(err, config.ErrRigNamespace) {
		t.Fatalf("a guarded-store refusal = %v; want it to wrap config.ErrRigNamespace", err)
	}
}

// ---------------------------------------------------------------------------
// Conditional writes, transactions and claim stamps.

// a revision-pinned write whose live read fails is refused, never judged
// as an unattributed row.
func TestRigNamespaceStoreConditionalWriteReadErrorFailsClosed(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)
	inner := &rgsFlakyGetStore{rgsSpyStore: &rgsSpyStore{MemStore: mem}}
	g := newRigNamespaceStore(inner, ns)
	cw, ok := g.ConditionalWriterHandle()
	if !ok {
		t.Fatal("no conditional writer")
	}
	closer, ok := g.AtomicConditionalCloserHandle()
	if !ok {
		t.Fatal("no atomic closer")
	}
	before, err := mem.Get("bt-person")
	if err != nil {
		t.Fatal(err)
	}
	title := "x"
	for name, write := range map[string]func() error{
		"UpdateIfMatch": func() error { return cw.UpdateIfMatch("bt-person", before.Revision, beads.UpdateOpts{Title: &title}) },
		"CloseIfMatch":  func() error { return cw.CloseIfMatch("bt-person", before.Revision) },
		"CloseWithMetadataIfMatch": func() error {
			_, err := closer.CloseWithMetadataIfMatch("bt-person", before.Revision, nil)
			return err
		},
		"DeleteIfMatch": func() error { return cw.DeleteIfMatch("bt-person", before.Revision) },
	} {
		if err := write(); !errors.Is(err, errRigNamespace) {
			t.Fatalf("%s of a row whose live read failed = %v; want a refusal", name, err)
		}
	}
	after, err := mem.Get("bt-person")
	if err != nil {
		t.Fatalf("Foreign row gone after refused writes: %v", err)
	}
	if after.Revision != before.Revision || after.Title != before.Title || after.Status != before.Status {
		t.Fatalf("Foreign row changed under writes the guard could not judge: %+v -> %+v", before, after)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("calls reached the inner store: %v", inner.calls)
	}
}

// a transaction's update applies every value rule on rows that are not
// Foreign.
func TestRigNamespaceStoreTxUpdateChecksValues(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	alice := "alice"
	if err := g.Tx("t", func(tx beads.Tx) error {
		return tx.Update("bt-own", beads.UpdateOpts{Assignee: &alice})
	}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("Tx update handing an own row to alice = %v; want refused", err)
	}
	if err := g.Tx("t", func(tx beads.Tx) error {
		return tx.Update("bt-stamped", beads.UpdateOpts{Metadata: map[string]string{rigNamespaceOwnerStampKey: "alpha/"}})
	}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("Tx update changing the creation stamp = %v; want refused", err)
	}
	if b, _ := mem.Get("bt-own"); b.Assignee == alice {
		t.Fatal("a refused Tx update landed")
	}
}

// rgsCondSpy records the revision-fenced writes that reach it.
type rgsCondSpy struct {
	*rgsSpyStore
}

func (s *rgsCondSpy) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.record("update-if-match", id, rev)
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *rgsCondSpy) CloseIfMatch(id string, rev int64) error {
	s.record("close-if-match", id, rev)
	return s.MemStore.CloseIfMatch(id, rev)
}

func (s *rgsCondSpy) DeleteIfMatch(id string, rev int64) error {
	s.record("delete-if-match", id, rev)
	return s.MemStore.DeleteIfMatch(id, rev)
}

// W58, W60a-d: a stale revision is answered by the guard itself; the inner
// writer is never reached.
func TestRigNamespaceStoreStaleRevisionNeverReachesTheInnerWriter(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	spy := &rgsCondSpy{rgsSpyStore: &rgsSpyStore{MemStore: beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)}}
	g := newRigNamespaceStore(spy, ns)
	cw, ok := g.ConditionalWriterHandle()
	if !ok {
		t.Fatal("no conditional writer")
	}
	closer, ok := g.AtomicConditionalCloserHandle()
	if !ok {
		t.Fatal("no atomic closer")
	}
	own, _ := spy.Get("bt-own")
	stale := own.Revision + 1000
	title := "x"
	for name, write := range map[string]func() error{
		"UpdateIfMatch": func() error { return cw.UpdateIfMatch("bt-own", stale, beads.UpdateOpts{Title: &title}) },
		"CloseIfMatch":  func() error { return cw.CloseIfMatch("bt-own", stale) },
		"DeleteIfMatch": func() error { return cw.DeleteIfMatch("bt-own", stale) },
		"CloseWithMetadataIfMatch": func() error {
			_, err := closer.CloseWithMetadataIfMatch("bt-own", stale, nil)
			return err
		},
	} {
		spy.calls = nil
		var pf *beads.PreconditionFailedError
		if err := write(); !errors.As(err, &pf) {
			t.Fatalf("%s at a stale revision = %v; want a precondition failure", name, err)
		}
		if len(spy.calls) != 0 {
			t.Fatalf("%s at a stale revision reached the inner writer: %v", name, spy.calls)
		}
	}
}

// a claim stamp under a spelling the key trims to is checked.
func TestRigNamespaceStoreSpacedClaimStampKeyIsChecked(t *testing.T) {
	g, _, _ := rgsGuard(t)
	if err := g.SetMetadata("bt-unattr", " "+beadmeta.SessionIDMetadataKey+" ", poolIDA); !errors.Is(err, errRigNamespace) {
		t.Fatalf("another city's session stamp under a spaced key = %v; want refused", err)
	}
}

// a stale revision is a precondition failure even on a Foreign row.
func TestRigNamespaceStoreStaleRevisionBeforeJudging(t *testing.T) {
	g, mem, _ := rgsGuard(t)
	cw, _ := g.ConditionalWriterHandle()
	row, _ := mem.Get("bt-person")
	title := "x"
	var pf *beads.PreconditionFailedError
	if err := cw.UpdateIfMatch("bt-person", row.Revision+1000, beads.UpdateOpts{Title: &title}); !errors.As(err, &pf) {
		t.Fatalf("UpdateIfMatch of a Foreign row at a stale revision = %v; want a precondition failure", err)
	}
}

// inside a transaction, a row that is not stored is judged
// unattributed: a route outside the namespace on it is refused by the guard.
func TestRigNamespaceStoreTxMissingRowIsUnattributed(t *testing.T) {
	g, _, _ := rgsGuard(t)
	err := g.Tx("t", func(tx beads.Tx) error {
		return tx.Update("bt-missing", beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "alpha/crew.intake"}})
	})
	if !errors.Is(err, errRigNamespace) {
		t.Fatalf("Tx route hand-off on a row that is not stored = %v; want the guard's refusal", err)
	}
}

// The writes below are this city's own and must land: a claim stamp that
// cannot decide the row's class (the row keeps an owner key) is bookkeeping,
// unless it names a pool session.

func rgsOwnRow(t *testing.T, g *rigNamespaceStore, inner *beads.MemStore, assignee string, md map[string]string) string {
	t.Helper()
	b, err := inner.Create(beads.Bead{Title: "own", Type: "task", Status: "in_progress", Assignee: assignee, Metadata: md})
	if err != nil {
		t.Fatal(err)
	}
	if c := g.ns.classify(b); c != rowOwned {
		t.Fatalf("precondition: row %s classifies %v, want owned", b.ID, c)
	}
	return b.ID
}

// TestRigNamespaceStoreStrandsAnOwnNamedSeat stamps the stranded claim of
// this city's own named session (a short, store-minted id) on the row it
// held, as the release lane does.
func TestRigNamespaceStoreStrandsAnOwnNamedSeat(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := beads.NewMemStore()
	g := newRigNamespaceStore(inner, ns)
	stack := wrapStoreWithBeadPolicies(g, &config.City{})
	named := rgsOwnRow(t, g, inner, "beta/crew.worker", nil)
	if err := stampStranded(stack, named, shortIDB, "beta/crew.worker"); err != nil {
		t.Fatalf("stranding an own named seat: %v", err)
	}
	pool := rgsOwnRow(t, g, inner, poolIDB, nil)
	if err := stampStranded(stack, pool, poolIDB, poolIDB); err != nil {
		t.Fatalf("stranding an own pool seat: %v", err)
	}
}

// TestRigNamespaceStoreAcceptsAPredictedSessionName creates graph steps routed
// to one of the rig's agents with the session name graph routing predicts
// before the session exists: refusing them would keep an on-demand agent from
// ever being woken for its work.
func TestRigNamespaceStoreAcceptsAPredictedSessionName(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	stack := wrapStoreWithBeadPolicies(newRigNamespaceStore(beads.NewMemStore(), ns), &config.City{})
	for _, md := range []map[string]string{
		rnsMD(beadmeta.RoutedToMetadataKey, "beta/witness", beadmeta.SessionNameMetadataKey, "beta--witness"),
		rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker", beadmeta.SessionNameMetadataKey, "beta--crew__worker"),
	} {
		if _, err := stack.Create(beads.Bead{Title: "step", Type: "task", Metadata: md}); err != nil {
			t.Fatalf("create with %v: %v", md, err)
		}
	}
	leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("bt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(leaf) })
	applier, ok := beads.GraphApplyFor(newRigNamespaceStore(leaf, ns))
	if !ok {
		t.Fatal("no graph apply through the guard")
	}
	if _, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "wf", Type: "task", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/witness", beadmeta.SessionNameMetadataKey, "beta--witness")},
		{Key: "s1", Title: "step", Type: "task", ParentKey: "root", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/witness", beadmeta.SessionNameMetadataKey, "beta--witness")},
	}}); err != nil {
		t.Fatalf("graph instantiation for the rig's own agent: %v", err)
	}
	if _, err := stack.Create(beads.Bead{Title: "step", Type: "task", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/witness", beadmeta.SessionNameMetadataKey, poolNameA)}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("a create stamped with another city's pool session name = %v; want refused", err)
	}
}

// TestRigNamespaceStoreStampsSessionIdentityWithWorkDir writes the
// session-identity batch of a named session whose recorded name lies outside
// the namespace, with the work_dir stamp, on the row it holds.
func TestRigNamespaceStoreStampsSessionIdentityWithWorkDir(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	inner := beads.NewMemStore()
	g := newRigNamespaceStore(inner, ns)
	stack := wrapStoreWithBeadPolicies(g, &config.City{})
	id := rgsOwnRow(t, g, inner, "beta/crew.worker", nil)
	if err := stack.SetMetadataBatch(id, map[string]string{beadmeta.SessionNameMetadataKey: "crew.worker", beadmeta.WorkDirMetadataKey: "/w"}); err != nil {
		t.Fatalf("session identity batch: %v", err)
	}
	if b, _ := inner.Get(id); b.Metadata[beadmeta.WorkDirMetadataKey] != "/w" {
		t.Fatal("work_dir was not written")
	}
}

// TestRigNamespaceStoreClaimStampRefusalNamesTheRule pins the refusal text of
// an in-namespace name with no session record: it is not "outside".
func TestRigNamespaceStoreClaimStampRefusalNamesTheRule(t *testing.T) {
	g, _, _ := rgsGuard(t)
	err := g.SetMetadata("bt-unattr", beadmeta.SessionNameMetadataKey, "beta--ghost")
	if !errors.Is(err, errRigNamespace) {
		t.Fatalf("an unrecorded in-namespace session name on an unattributed row = %v; want refused", err)
	}
	if msg := err.Error(); strings.Contains(msg, "is outside") || !strings.Contains(msg, "names no session") {
		t.Fatalf("refusal %q misdescribes an in-namespace name", msg)
	}
}

// rgsProxiedEngine has beads.ProxiedStore's shape for the guarded update: it
// carries no UpdateIfAssignment of its own and declares its write leaf as the
// conditional-writes resolve target, as ProxiedStore does.
type rgsProxiedEngine struct {
	*beads.MemStore
	leaf beads.Store
}

func (p rgsProxiedEngine) ConditionalWritesResolveTarget() beads.Store { return p.leaf }

// TestRigNamespaceStoreUpdateIfAssignmentThroughAProxiedEngine: the guard
// reaches UpdateIfAssignment with beads.As, which resolves it on a proxied
// engine's write leaf (AssignmentGuardedUpdaterFor). The guard forwards it
// there after its own checks, the leaf an unguarded rig's caller reaches; a
// Foreign row is still refused before the leaf is asked, and an engine whose
// leaf lacks the capability still answers unsupported.
func TestRigNamespaceStoreUpdateIfAssignmentThroughAProxiedEngine(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStoreFrom(1000, rgsSeedBeads(), nil)
	leaf := &rgsSpyStore{MemStore: mem}
	engine := rgsProxiedEngine{MemStore: mem, leaf: leaf}
	if _, direct := beads.Store(engine).(beads.AssignmentGuardedUpdater); direct {
		t.Fatal("fixture: the proxied engine must not carry UpdateIfAssignment itself")
	}
	if u, ok := beads.AssignmentGuardedUpdaterFor(engine); !ok || u != beads.AssignmentGuardedUpdater(leaf) {
		t.Fatal("fixture: the proxied engine must resolve UpdateIfAssignment on its write leaf")
	}
	g := newRigNamespaceStore(engine, ns)
	title := "retitled"
	if ok, err := g.UpdateIfAssignment("bt-ownpool", "in_progress", poolIDB, beads.UpdateOpts{Title: &title}); err != nil || !ok {
		t.Fatalf("UpdateIfAssignment on an own row through a proxied engine = %v, %v; want it forwarded to the write leaf", ok, err)
	}
	if len(leaf.calls) != 1 || !strings.HasPrefix(leaf.calls[0], "update-if-assignment") {
		t.Fatalf("write leaf calls = %q; want the one guarded update", leaf.calls)
	}
	if _, err := g.UpdateIfAssignment("bt-otherpool", "in_progress", poolIDA, beads.UpdateOpts{Title: &title}); !errors.Is(err, errRigNamespace) {
		t.Fatalf("UpdateIfAssignment on another city's row = %v; want refused", err)
	}
	if len(leaf.calls) != 1 {
		t.Fatalf("write leaf calls = %q; a refused update reached the leaf", leaf.calls)
	}

	if _, has := beads.Store(mem).(beads.AssignmentGuardedUpdater); has {
		t.Skip("MemStore carries UpdateIfAssignment; the unsupported case needs a leaf without it")
	}
	bare := newRigNamespaceStore(rgsProxiedEngine{MemStore: mem, leaf: mem}, ns)
	if _, err := bare.UpdateIfAssignment("bt-ownpool", "in_progress", poolIDB, beads.UpdateOpts{Title: &title}); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("UpdateIfAssignment through a proxied engine whose leaf lacks it = %v; want ErrConditionalWriteUnsupported", err)
	}
}

// TestRigNamespaceStoreFiltersReadyBlockedByAbsent: the absent-blocker
// listing through policy(guard(store)) is a discovery read. It keeps this
// city's row and drops another city's, and a query for an assignee outside
// the namespace reads nothing.
func TestRigNamespaceStoreFiltersReadyBlockedByAbsent(t *testing.T) {
	ns, _ := rnsCityBNamespace(t)
	mem := beads.NewMemStore()
	own, err := mem.Create(beads.Bead{Title: "own", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "beta/crew.worker"}})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := mem.Create(beads.Bead{Title: "foreign", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "alpha/crew.worker"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{own.ID, foreign.ID} {
		if err := mem.DepAdd(id, "zz-absent-1", "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	raw, ok := beads.ReadyBlockedByAbsentFor(mem)
	if !ok {
		t.Fatal("fixture: MemStore lists rows blocked by an absent target")
	}
	if rows, err := raw.ReadyBlockedByAbsent(beads.ReadyQuery{}); err != nil || len(rows) != 2 {
		t.Fatalf("fixture: unguarded listing = %v, %v; want both rows", rgsIDs(rows), err)
	}
	stack := wrapStoreWithBeadPolicies(newRigNamespaceStore(mem, ns), nil)
	reader, ok := beads.ReadyBlockedByAbsentFor(stack)
	if !ok {
		t.Fatal("the guarded stack hid the absent-blocker listing")
	}
	rows, err := reader.ReadyBlockedByAbsent(beads.ReadyQuery{})
	if ids := rgsIDs(rows); err != nil || len(ids) != 1 || ids[0] != own.ID {
		t.Fatalf("guarded listing = %v, %v; want only %s", ids, err, own.ID)
	}
	rows, err = reader.ReadyBlockedByAbsent(beads.ReadyQuery{Assignee: "alpha/crew.worker"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("listing for another namespace's assignee = %v, %v; want none", rgsIDs(rows), err)
	}

	// A row whose admission needs an unreadable sessions store is dropped and
	// the listing reports itself partial; so does a query whose assignee
	// cannot be decided, which reads nothing.
	pool, err := mem.Create(beads.Bead{Title: "pool", Type: "task", Assignee: poolIDB2})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.DepAdd(pool.ID, "zz-absent-1", "blocks"); err != nil {
		t.Fatal(err)
	}
	if rows, err := raw.ReadyBlockedByAbsent(beads.ReadyQuery{}); err != nil || len(rows) != 3 {
		t.Fatalf("fixture: unguarded listing = %v, %v; want all three rows", rgsIDs(rows), err)
	}
	downNS, downSessions := rnsCityBNamespace(t)
	downSessions.setErr(errors.New("sessions store down"))
	down, ok := beads.ReadyBlockedByAbsentFor(wrapStoreWithBeadPolicies(newRigNamespaceStore(mem, downNS), nil))
	if !ok {
		t.Fatal("the guarded stack hid the absent-blocker listing")
	}
	rows, err = down.ReadyBlockedByAbsent(beads.ReadyQuery{})
	if !beads.IsPartialResult(err) {
		t.Fatalf("listing with an unreadable sessions store = %v, %v; want a partial result", rgsIDs(rows), err)
	}
	if ids := rgsIDs(rows); len(ids) != 1 || ids[0] != own.ID {
		t.Fatalf("partial listing = %v; want only %s (the pool row's admission is undecidable)", ids, own.ID)
	}
	rows, err = down.ReadyBlockedByAbsent(beads.ReadyQuery{Assignee: poolIDB2})
	if !beads.IsPartialResult(err) || len(rows) != 0 {
		t.Fatalf("listing for an undecidable assignee = %v, %v; want none, partial", rgsIDs(rows), err)
	}
}
