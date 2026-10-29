package orders

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// listSpyStore records every List query it receives, then delegates to the
// embedded store. Because it embeds the Store INTERFACE (not the concrete type),
// its method set carries only the Store methods, so HandlesFor cannot find a
// Handles() implementation and falls back to the logical live/cached wrappers —
// which is exactly what lets this spy observe the query.Live flag those wrappers
// set.
type listSpyStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *listSpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	return s.Store.List(q)
}

// TestLastRunCursorUnionAcrossDistinctStores is the MANDATORY two-class
// characterization test. An order whose ONLY order-run evidence is a wisp /
// molecule root (a graph-class bead, no order-tracking tracking bead) must still
// report the correct LastRun and Cursor when the orders leg and the graph leg are
// two DISTINCT stores — proving the reads union across classes instead of
// assuming a single colocated store.
func TestLastRunCursorUnionAcrossDistinctStores(t *testing.T) {
	ordersLeg := beads.NewMemStore()
	graphLeg := beads.NewMemStore()

	// The graph leg holds only a wisp root: it carries the order-run label and the
	// event cursor (order:<scoped> + seq:<N>) that the dispatcher stamps on the
	// molecule root, but NOT the order-tracking label (that lives on tracking
	// beads in the orders leg, which here is empty).
	root, err := graphLeg.Create(beads.Bead{
		Title:  "wisp: digest",
		Type:   "molecule",
		Labels: []string{"order-run:digest", "order:digest", "seq:7"},
	})
	if err != nil {
		t.Fatal(err)
	}

	twoLeg := NewStoreWithGraph(
		beads.OrdersStore{Store: ordersLeg},
		beads.GraphStore{Store: graphLeg},
	)

	gotLast, err := twoLeg.LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !gotLast.Equal(root.CreatedAt) {
		t.Fatalf("LastRun() = %s, want the graph wisp root's CreatedAt %s", gotLast, root.CreatedAt)
	}
	if got := twoLeg.Cursor("digest"); got != 7 {
		t.Fatalf("Cursor() = %d, want 7 from the graph wisp root seq", got)
	}

	// Regression guard: without the graph leg (the single-store-assumption bug
	// the correction forbids), the orders leg alone sees no evidence.
	ordersOnly := NewStore(beads.OrdersStore{Store: ordersLeg})
	if got, err := ordersOnly.LastRun("digest"); err != nil || !got.IsZero() {
		t.Fatalf("orders-leg-only LastRun() = %s, err=%v; want zero (evidence lives in the graph leg)", got, err)
	}
	if got := ordersOnly.Cursor("digest"); got != 0 {
		t.Fatalf("orders-leg-only Cursor() = %d, want 0 (evidence lives in the graph leg)", got)
	}
}

// TestMixedLegStoresDedupsSharedStore proves the single-store city (both legs
// wrapping ONE underlying store) reads that store exactly once — the union does
// not double-read, so the verdict stays byte-identical to the pre-split behavior.
func TestMixedLegStoresDedupsSharedStore(t *testing.T) {
	shared := &listSpyStore{Store: beads.NewMemStore()}
	if _, err := shared.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	shared.queries = nil

	st := NewStoreWithGraph(
		beads.OrdersStore{Store: shared},
		beads.GraphStore{Store: shared},
	)
	if _, err := st.LastRun("digest"); err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if len(shared.queries) != 1 {
		t.Fatalf("LastRun issued %d List calls, want 1 (deduped shared store)", len(shared.queries))
	}
}

// Cursor reduces its rows to MaxSeqFromLabels — a max over seq, NOT over the
// created_at sort key — so it must NOT opt into the backing's bounded
// created-desc read. The backing breaks created_at ties by id ASC, so a bounded
// backing read drops the newest largest-id row that carries the max seq when a
// same-second burst exceeds the limit, regressing the event cursor into replaying
// consumed events. The read stays exact by fetching the full candidate set and
// letting ApplyListQuery cut the canonical (created_at DESC, id DESC) prefix.
func TestCursorDoesNotOptIntoBackingCreatedLimit(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	if _, err := spy.Create(beads.Bead{Title: "run", Labels: []string{"order-run:digest", "order-tracking", "seq:9"}}); err != nil {
		t.Fatal(err)
	}
	spy.queries = nil
	st := NewStore(beads.OrdersStore{Store: spy})

	if got := st.Cursor("digest"); got != 9 {
		t.Fatalf("Cursor() = %d, want 9", got)
	}
	if len(spy.queries) == 0 {
		t.Fatal("Cursor issued no List query")
	}
	for i, q := range spy.queries {
		if q.Sort != beads.SortCreatedDesc {
			t.Errorf("Cursor query[%d] Sort = %q, want SortCreatedDesc", i, q.Sort)
		}
		if q.AllowBackingCreatedLimit {
			t.Errorf("Cursor query[%d] set AllowBackingCreatedLimit; its max(seq) reduction needs the canonical id-DESC prefix, which a bounded id-ASC backing read would truncate", i)
		}
	}
}

// TestRecentRunsAllOpenRunsUseLiveTier pins the read tier: the dispatch cooldown /
// single-flight index folds MUST bypass the caching layer (Live), because the
// cache-bypass is the duplicate-dispatch guarantee.
func TestRecentRunsAllOpenRunsUseLiveTier(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	if _, err := spy.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	st := NewStore(beads.OrdersStore{Store: spy})

	spy.queries = nil
	if _, err := st.RecentRunsAll(2048); err != nil {
		t.Fatalf("RecentRunsAll(): %v", err)
	}
	assertAllLive(t, "RecentRunsAll", spy.queries)

	spy.queries = nil
	if _, err := st.OpenRuns(); err != nil {
		t.Fatalf("OpenRuns(): %v", err)
	}
	assertAllLive(t, "OpenRuns", spy.queries)

	// The stale sweep clears the gate OpenRuns holds, so it must see the same
	// Live set, not the cache snapshot.
	spy.queries = nil
	if _, err := st.StaleOpenRuns(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	assertAllLive(t, "StaleOpenRuns", spy.queries)
	// One query per status the strict gate counts and every backing selects
	// exactly (an in_progress jam must still be swept); open first, because a
	// claim moves a bead from open to in_progress.
	assertSweepQueryShape(t, "StaleOpenRuns", spy.queries, "open", "in_progress")

	spy.queries = nil
	if _, err := st.StaleNotClosedRuns(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	assertAllLive(t, "StaleNotClosedRuns", spy.queries)
	// One query for every not-closed status: the whole set the strict gate
	// counts, a hand-parked one included.
	assertSweepQueryShape(t, "StaleNotClosedRuns", spy.queries, "")

	spy.queries = nil
	if _, err := st.OrphanedOpenRuns(); err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	assertAllLive(t, "OrphanedOpenRuns", spy.queries)
	// One query for every not-closed status, a hand-parked one included.
	assertSweepQueryShape(t, "OrphanedOpenRuns", spy.queries, "")
}

// assertSweepQueryShape pins a sweep reader's queries: one per status in
// statuses, in that order ("" is every not-closed status), each for the
// tracking beads across both tiers with no backing limit (the caller
// post-filters before applying its close budget).
func assertSweepQueryShape(t *testing.T, name string, queries []beads.ListQuery, statuses ...string) {
	t.Helper()
	if len(queries) != len(statuses) {
		t.Fatalf("%s issued %d queries %+v, want one per status %q", name, len(queries), queries, statuses)
	}
	for i, q := range queries {
		if q.Label != "order-tracking" || q.IncludeClosed || q.Status != statuses[i] || q.Limit != 0 || q.TierMode != beads.TierBoth {
			t.Fatalf("%s query[%d] = %+v, want Label=order-tracking, IncludeClosed=false, Status=%q, Limit=0, TierMode=TierBoth", name, i, q, statuses[i])
		}
		if q.Sort != beads.SortCreatedAsc {
			t.Fatalf("%s query[%d].Sort = %q, want SortCreatedAsc (the caller's close budget must drain the oldest jam)", name, i, q.Sort)
		}
	}
}

func assertAllLive(t *testing.T, name string, queries []beads.ListQuery) {
	t.Helper()
	if len(queries) == 0 {
		t.Fatalf("%s issued no List query", name)
	}
	for i, q := range queries {
		if !q.Live {
			t.Fatalf("%s query[%d].Live = false, want true (must bypass the caching layer)", name, i)
		}
	}
}

// TestRecentRunsAllFoldsTrackingBeads proves RecentRunsAll decodes tracking beads
// (with the legacy order:<title> name fallback) and skips beads with no
// resolvable order name — matching the dispatcher's index fold.
func TestRecentRunsAllFoldsTrackingBeads(t *testing.T) {
	mem := beads.NewMemStore()
	// Labeled tracking bead.
	if _, err := mem.Create(beads.Bead{Title: "order:a", Labels: []string{"order-run:a", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	// Legacy tracking bead: order-tracking label but only the order:<title>
	// prefix, no order-run label. The title fallback must still resolve it.
	if _, err := mem.Create(beads.Bead{Title: "order:legacy", Labels: []string{"order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	// Foreign order-tracking bead with no resolvable name: skipped.
	if _, err := mem.Create(beads.Bead{Title: "unrelated", Labels: []string{"order-tracking"}}); err != nil {
		t.Fatal(err)
	}

	runs, err := NewStore(beads.OrdersStore{Store: mem}).RecentRunsAll(2048)
	if err != nil {
		t.Fatalf("RecentRunsAll(): %v", err)
	}
	got := map[string]bool{}
	for _, r := range runs {
		got[r.Scoped] = true
	}
	want := map[string]bool{"a": true, "legacy": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RecentRunsAll scoped names = %v, want %v", got, want)
	}
}

// TestHasOpenWorkUnionsGraphLeg proves HasOpenWork finds an open wisp root that
// lives only in the graph leg, via the injected wisp-walk predicate.
func TestHasOpenWorkUnionsGraphLeg(t *testing.T) {
	ordersLeg := beads.NewMemStore()
	graphLeg := beads.NewMemStore()
	if _, err := graphLeg.Create(beads.Bead{Title: "wisp: digest", Type: "molecule", Labels: []string{"order-run:digest"}}); err != nil {
		t.Fatal(err)
	}
	st := NewStoreWithGraph(beads.OrdersStore{Store: ordersLeg}, beads.GraphStore{Store: graphLeg})

	// Predicate: a molecule root is treated as open work.
	wispWalk := func(_ beads.Store, root beads.Bead) (bool, error) {
		return beads.IsMoleculeType(root.Type), nil
	}
	open, err := st.HasOpenWork("digest", wispWalk)
	if err != nil {
		t.Fatalf("HasOpenWork(): %v", err)
	}
	if !open {
		t.Fatalf("HasOpenWork() = false, want true (open wisp root in the graph leg)")
	}
}

// TestHasOpenWorkOpenTrackingBead proves an open order-tracking bead in the
// orders leg is in-flight work without consulting the wisp walk.
func TestHasOpenWorkOpenTrackingBead(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	st := NewStore(beads.OrdersStore{Store: mem})
	open, err := st.HasOpenWork("digest", func(beads.Store, beads.Bead) (bool, error) {
		t.Fatalf("wisp walk must not run for an order-tracking bead")
		return false, nil
	})
	if err != nil {
		t.Fatalf("HasOpenWork(): %v", err)
	}
	if !open {
		t.Fatalf("HasOpenWork() = false, want true (open tracking bead)")
	}
}

// TestMarkFailedSingleUpdate proves MarkFailed emits exactly ONE Update whose
// labels are the wisp-failed outcome plus the event cursor pair, byte-identical
// to the dispatcher's original markTrackingFailure.
func TestMarkFailedSingleUpdate(t *testing.T) {
	st, rec := recordingOrdersStore()
	seeded, err := st.store.Create(beads.Bead{Title: "order:rig/agent", Labels: []string{"order-run:rig/agent", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	rec.Reset()

	cursor := EventCursor(9)
	if err := st.MarkFailed(seeded.ID, "rig/agent", RunOutcomeWispFailed, &cursor); err != nil {
		t.Fatalf("MarkFailed(): %v", err)
	}
	updates := rec.CallsForOp("Update")
	if len(updates) != 1 {
		t.Fatalf("want exactly 1 Update, got %d", len(updates))
	}
	want := []string{"wisp", "wisp-failed", "order:rig/agent", "seq:9"}
	if !reflect.DeepEqual(updates[0].Opts.Labels, want) {
		t.Fatalf("labels = %v, want %v", updates[0].Opts.Labels, want)
	}
}

// TestMarkFailedNoCursor proves a nil cursor stamps only the outcome labels.
func TestMarkFailedNoCursor(t *testing.T) {
	st, rec := recordingOrdersStore()
	seeded, err := st.store.Create(beads.Bead{Title: "order:rig/agent", Labels: []string{"order-run:rig/agent", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	if err := st.MarkFailed(seeded.ID, "rig/agent", RunOutcomeWispFailed, nil); err != nil {
		t.Fatalf("MarkFailed(): %v", err)
	}
	updates := rec.CallsForOp("Update")
	if len(updates) != 1 {
		t.Fatalf("want 1 Update, got %d", len(updates))
	}
	want := []string{"wisp", "wisp-failed"}
	if !reflect.DeepEqual(updates[0].Opts.Labels, want) {
		t.Fatalf("labels = %v, want %v", updates[0].Opts.Labels, want)
	}
}

// TestGetAndRunDetail proves Get projects a tracking bead onto an OrderRun and
// RunDetail additionally surfaces the exec-gate output.
func TestGetAndRunDetail(t *testing.T) {
	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{
		Title:  "order:digest",
		Labels: []string{"order-run:digest", "order-tracking", "exec"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.SetMetadataBatch(created.ID, map[string]string{
		convergence.FieldGateExitCode: "0",
		convergence.FieldGateStdout:   "ok",
	}); err != nil {
		t.Fatal(err)
	}
	st := NewStore(beads.OrdersStore{Store: mem})

	run, err := st.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if run.Scoped != "digest" || run.Outcome != RunOutcomeExec {
		t.Fatalf("Get() = %+v, want scoped=digest outcome=exec", run)
	}

	detail, err := st.RunDetail(created.ID)
	if err != nil {
		t.Fatalf("RunDetail(): %v", err)
	}
	if detail.Run.Scoped != "digest" {
		t.Fatalf("RunDetail().Run.Scoped = %q, want digest", detail.Run.Scoped)
	}
	if detail.Gate.ExitCode != "0" || detail.Gate.CombinedOutput() != "ok" {
		t.Fatalf("RunDetail().Gate = %+v, want exit=0 output=ok", detail.Gate)
	}
}

// TestCloseRunsBatchVerify proves CloseRuns closes and verifies the batch,
// stamping close_reason.
func TestCloseRunsBatchVerify(t *testing.T) {
	mem := beads.NewMemStore()
	var ids []string
	for _, name := range []string{"a", "b"} {
		b, err := mem.Create(beads.Bead{Title: "order:" + name, Labels: []string{"order-run:" + name, "order-tracking"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	st := NewStore(beads.OrdersStore{Store: mem})
	n, err := st.CloseRuns(context.Background(), ids, "done")
	if err != nil {
		t.Fatalf("CloseRuns(): %v", err)
	}
	if n != 2 {
		t.Fatalf("CloseRuns() closed %d, want 2", n)
	}
	for _, id := range ids {
		got, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("bead %s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != "done" {
			t.Fatalf("bead %s close_reason = %q, want done", id, got.Metadata["close_reason"])
		}
	}
}

// TestStaleOpenRunsCutoff proves StaleOpenRuns returns open tracking runs at or
// before the cutoff and excludes fresher ones.
func TestStaleOpenRunsCutoff(t *testing.T) {
	mem := beads.NewMemStore()
	old, err := mem.Create(beads.Bead{Title: "order:old", Labels: []string{"order-run:old", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(beads.OrdersStore{Store: mem})

	// cutoff after the bead's creation → stale.
	stale, err := st.StaleOpenRuns(old.CreatedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if len(stale) != 1 || stale[0].Scoped != "old" {
		t.Fatalf("StaleOpenRuns(after) = %+v, want the old run", stale)
	}
	// cutoff before the bead's creation → not stale.
	fresh, err := st.StaleOpenRuns(old.CreatedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if len(fresh) != 0 {
		t.Fatalf("StaleOpenRuns(before) = %+v, want none", fresh)
	}
}

// TestStaleNotClosedRunsListsEveryNotClosedStatusAtOrBeforeCutoff proves the
// on-demand sweep's read: every tracking bead the strict open-work gate counts
// that is at or before the cutoff, oldest-first. That includes one parked by
// hand, which the watchdog's StaleOpenRuns does not read, and a pre-dispatch
// trigger-env-failure marker, which is the stale sweep's to close (the orphan
// sweep skips it).
func TestStaleNotClosedRunsListsEveryNotClosedStatusAtOrBeforeCutoff(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	tracking := func(id, status string, age time.Duration, extra ...string) beads.Bead {
		return beads.Bead{
			ID:        id,
			Title:     "order:digest",
			Status:    status,
			Labels:    append([]string{"order-run:digest", "order-tracking"}, extra...),
			CreatedAt: base.Add(age),
		}
	}
	seed := []beads.Bead{
		tracking("gc-marker", "open", 3*time.Minute, "trigger-env-failed"),
		tracking("gc-parked", "blocked", 2*time.Minute),
		tracking("gc-fresh", "open", 10*time.Minute),
		tracking("gc-claimed", "in_progress", time.Minute),
		tracking("gc-closed", "closed", 4*time.Minute),
		tracking("gc-open", "open", 0),
	}
	st := ordersStoreOver(beads.NewMemStoreFrom(len(seed), seed, nil))
	cutoff := base.Add(5 * time.Minute)

	stale, err := st.StaleNotClosedRuns(cutoff)
	if err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	if got, want := runIDs(stale), []string{"gc-open", "gc-claimed", "gc-parked", "gc-marker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleNotClosedRuns() IDs = %v, want every not-closed bead at or before the cutoff, oldest-first %v", got, want)
	}
	bounded, err := st.StaleOpenRuns(cutoff)
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if got, want := runIDs(bounded), []string{"gc-open", "gc-claimed", "gc-marker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleOpenRuns() IDs = %v, want %v (the parked bead is the not-closed read's alone)", got, want)
	}
}

// TestOrphanedOpenRunsExcludesTriggerEnvFailed proves the pre-dispatch
// trigger-env-failure markers are excluded from the orphaned sweep read.
func TestOrphanedOpenRunsExcludesTriggerEnvFailed(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "order:a", Labels: []string{"order-run:a", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Create(beads.Bead{Title: "order:b", Labels: []string{"order-run:b", "order-tracking", "trigger-env-failed"}}); err != nil {
		t.Fatal(err)
	}
	runs, err := NewStore(beads.OrdersStore{Store: mem}).OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	if len(runs) != 1 || runs[0].Scoped != "a" {
		t.Fatalf("OrphanedOpenRuns() = %+v, want only a (trigger-env-failed excluded)", runs)
	}
}

// TestClosedRunsForRetentionBestEffortName proves closed tracking beads are
// returned including those with no resolvable order name (Scoped ""), so the
// caller can route them to the legacy retention bucket.
func TestClosedRunsForRetentionBestEffortName(t *testing.T) {
	mem := beads.NewMemStore()
	named, err := mem.Create(beads.Bead{Title: "order:a", Labels: []string{"order-run:a", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := mem.Create(beads.Bead{Title: "foreign", Labels: []string{"order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(named.ID); err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(foreign.ID); err != nil {
		t.Fatal(err)
	}
	runs, err := NewStore(beads.OrdersStore{Store: mem}).ClosedRunsForRetention()
	if err != nil {
		t.Fatalf("ClosedRunsForRetention(): %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ClosedRunsForRetention() returned %d runs, want 2 (including the unresolvable-name bead)", len(runs))
	}
}

// primedCacheWithBackingOnlyTracking primes a CachingStore to its live snapshot
// over an empty MemStore, then writes an open tracking bead straight to the
// backing: the dispatcher's uncached-handle shape. The cache never absorbs it.
func primedCacheWithBackingOnlyTracking(t *testing.T) (*beads.CachingStore, beads.Bead) {
	t.Helper()
	backing := beads.NewMemStore()
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	written, err := backing.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.ListByLabel("order-tracking", 0)
	if err != nil {
		t.Fatalf("cache.ListByLabel: %v", err)
	}
	if beadsContain(cached, written.ID) {
		t.Fatalf("cache.ListByLabel() listed backing-only bead %s; the fixture no longer serves the snapshot", written.ID)
	}
	return cache, written
}

// TestStaleOpenRunsBypassesCache: a tracking bead written behind a live cache
// snapshot is visible to OpenRuns, so the stale sweep that clears OpenRuns'
// gate must see it too (#5832).
func TestStaleOpenRunsBypassesCache(t *testing.T) {
	cache, written := primedCacheWithBackingOnlyTracking(t)

	runs, err := ordersStoreOver(cache).StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if !runsContain(runs, written.ID) {
		t.Fatalf("StaleOpenRuns() = %v; missed backing-only bead %s (read the cache snapshot, not the Live tier)", runs, written.ID)
	}
}

// TestStaleNotClosedRunsBypassesCache is the same proof for the on-demand
// sweep's read (#5832).
func TestStaleNotClosedRunsBypassesCache(t *testing.T) {
	cache, written := primedCacheWithBackingOnlyTracking(t)

	runs, err := ordersStoreOver(cache).StaleNotClosedRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	if !runsContain(runs, written.ID) {
		t.Fatalf("StaleNotClosedRuns() = %v; missed backing-only bead %s (read the cache snapshot, not the Live tier)", runs, written.ID)
	}
}

// TestOrphanedOpenRunsBypassesCache: the startup orphan sweep must see a
// tracking bead the cache never absorbed, and still skip a trigger-env-failure
// marker written the same way (#5832).
func TestOrphanedOpenRunsBypassesCache(t *testing.T) {
	cache, written := primedCacheWithBackingOnlyTracking(t)
	marker, err := cache.Backing().Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking", "trigger-env-failed"}})
	if err != nil {
		t.Fatal(err)
	}

	runs, err := ordersStoreOver(cache).OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	if !runsContain(runs, written.ID) {
		t.Fatalf("OrphanedOpenRuns() = %v; missed backing-only bead %s (read the cache snapshot, not the Live tier)", runs, written.ID)
	}
	if runsContain(runs, marker.ID) {
		t.Fatalf("OrphanedOpenRuns() = %v; included trigger-env-failure marker %s", runs, marker.ID)
	}
}

// TestSweepReadsOldestFirst: the sweep readers return tracking beads
// oldest-first (CreatedAt ascending, ties by ID ascending), independent of the
// backing's storage order and of which status read found each one, so a
// budgeted close drains the oldest jam first.
func TestSweepReadsOldestFirst(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	tracking := func(id, status string, age time.Duration) beads.Bead {
		return beads.Bead{
			ID:        id,
			Title:     "order:digest",
			Status:    status,
			Labels:    []string{"order-run:digest", "order-tracking"},
			CreatedAt: base.Add(age),
		}
	}
	// Stored scrambled, open and in_progress interleaved by age; gc-a and gc-b
	// share a CreatedAt.
	seed := []beads.Bead{
		tracking("gc-e", "open", 4*time.Minute),
		tracking("gc-b", "open", time.Minute),
		tracking("gc-d", "in_progress", 3*time.Minute),
		tracking("gc-a", "in_progress", time.Minute),
		tracking("gc-f", "open", 2*time.Minute),
		tracking("gc-c", "open", 0),
	}
	want := []string{"gc-c", "gc-a", "gc-b", "gc-f", "gc-d", "gc-e"}
	st := ordersStoreOver(beads.NewMemStoreFrom(len(seed), seed, nil))

	stale, err := st.StaleOpenRuns(time.Now())
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if got := runIDs(stale); !reflect.DeepEqual(got, want) {
		t.Errorf("StaleOpenRuns() IDs = %v, want oldest-first %v", got, want)
	}
	notClosed, err := st.StaleNotClosedRuns(time.Now())
	if err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	if got := runIDs(notClosed); !reflect.DeepEqual(got, want) {
		t.Errorf("StaleNotClosedRuns() IDs = %v, want oldest-first %v", got, want)
	}
	orphaned, err := st.OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	if got := runIDs(orphaned); !reflect.DeepEqual(got, want) {
		t.Errorf("OrphanedOpenRuns() IDs = %v, want oldest-first %v", got, want)
	}
}

// TestSweepReadsKeepInProgressAndExcludeClosed guards the sweep readers' status
// semantics: an in_progress tracking bead is still swept (a Status:"open" filter
// would strand it gating its order) and a closed one is not.
func TestSweepReadsKeepInProgressAndExcludeClosed(t *testing.T) {
	mem := beads.NewMemStore()
	mk := func(status string) beads.Bead {
		b, err := mem.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking"}})
		if err != nil {
			t.Fatal(err)
		}
		if status != "open" {
			if err := mem.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	openBead, inProgress, closedBead := mk("open"), mk("in_progress"), mk("closed")
	st := ordersStoreOver(mem)

	stale, err := st.StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	notClosed, err := st.StaleNotClosedRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	orphaned, err := st.OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	want := []string{openBead.ID, inProgress.ID}
	if got := runIDs(stale); !reflect.DeepEqual(got, want) {
		t.Errorf("StaleOpenRuns() IDs = %v, want %v (closed %s excluded)", got, want, closedBead.ID)
	}
	if got := runIDs(notClosed); !reflect.DeepEqual(got, want) {
		t.Errorf("StaleNotClosedRuns() IDs = %v, want %v (closed %s excluded)", got, want, closedBead.ID)
	}
	if got := runIDs(orphaned); !reflect.DeepEqual(got, want) {
		t.Errorf("OrphanedOpenRuns() IDs = %v, want %v (closed %s excluded)", got, want, closedBead.ID)
	}
}

// claimAfterFirstReadStore claims a tracking bead the moment its first list
// read returns, as another process can between two of a reader's queries.
type claimAfterFirstReadStore struct {
	beads.Store
	claim string
	reads int
}

func (s *claimAfterFirstReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	list, err := s.Store.List(q)
	if err != nil {
		return nil, err
	}
	s.reads++
	if s.reads == 1 {
		inProgress := "in_progress"
		if err := s.Update(s.claim, beads.UpdateOpts{Status: &inProgress}); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// TestStaleOpenRunsListsABeadClaimedBetweenItsReadsOnce: a claim moves a
// tracking bead forward, from open to in_progress, so the stale sweep reads open
// first. A bead claimed between its two reads is then read by both and listed
// once, where reading in_progress first would miss it until the next tick.
func TestStaleOpenRunsListsABeadClaimedBetweenItsReadsOnce(t *testing.T) {
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	st := ordersStoreOver(&claimAfterFirstReadStore{Store: mem, claim: b.ID})

	runs, err := st.StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if got, want := runIDs(runs), []string{b.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleOpenRuns() IDs = %v, want %v exactly once", got, want)
	}
}

func runIDs(runs []OrderRun) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

func beadsContain(list []beads.Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

func runsContain(runs []OrderRun, id string) bool {
	for _, r := range runs {
		if r.ID == id {
			return true
		}
	}
	return false
}
