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

	// StaleOpenRuns feeds the stale-tracking close sweep, whose whole job is
	// recovering tracking beads the dispatcher wrote through its own uncached
	// handle — a cached read never observes them (#5832).
	spy.queries = nil
	if _, err := st.StaleOpenRuns(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	assertAllLive(t, "StaleOpenRuns", spy.queries)
	assertSweepQueryShape(t, "StaleOpenRuns", spy.queries)

	// OrphanedOpenRuns feeds the startup sweep, which cleans up after a
	// controller that died holding tracking beads — a cache primed by the very
	// process doing the sweeping cannot be trusted to contain them.
	spy.queries = nil
	if _, err := st.OrphanedOpenRuns(); err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	assertAllLive(t, "OrphanedOpenRuns", spy.queries)
	assertSweepQueryShape(t, "OrphanedOpenRuns", spy.queries)
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

// assertSweepQueryShape pins the sweep readers' list-query contract: both tiers
// (legacy issues + wisp), no closed rows, and no status filter — an in_progress
// tracking bead must still be returned, or the sweep strands it behind its
// order's open-work gate forever.
func assertSweepQueryShape(t *testing.T, name string, queries []beads.ListQuery) {
	t.Helper()
	for i, q := range queries {
		if q.Label != labelOrderTracking {
			t.Errorf("%s query[%d].Label = %q, want %q", name, i, q.Label, labelOrderTracking)
		}
		if q.TierMode != beads.TierBoth {
			t.Errorf("%s query[%d].TierMode = %v, want TierBoth", name, i, q.TierMode)
		}
		if q.IncludeClosed {
			t.Errorf("%s query[%d].IncludeClosed = true, want false (the sweep only closes open beads)", name, i)
		}
		if q.Status != "" {
			t.Errorf("%s query[%d].Status = %q, want none (in_progress tracking beads must be swept too)", name, i, q.Status)
		}
		if q.Limit != 0 {
			t.Errorf("%s query[%d].Limit = %d, want 0 (callers post-filter before the close budget)", name, i, q.Limit)
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

// TestStaleAndOrphanedSweepsReadTheLiveTierLikeOpenRuns proves BOTH sweep
// readers bypass the caching snapshot the way their sibling OpenRuns does: a
// tracking bead written straight to the backing after the cache was primed is
// visible to OpenRuns and must be visible to both sweeps too (#5832).
func TestStaleAndOrphanedSweepsReadTheLiveTierLikeOpenRuns(t *testing.T) {
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	written, err := backing.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", labelOrderTracking}})
	if err != nil {
		t.Fatal(err)
	}
	store := ordersStoreOver(cache)

	open, err := store.OpenRuns()
	if err != nil {
		t.Fatalf("OpenRuns: %v", err)
	}
	if !runsContain(open, written.ID) {
		t.Fatalf("OpenRuns() = %v; want it to see %s (sibling baseline)", open, written.ID)
	}
	stale, err := store.StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns: %v", err)
	}
	if !runsContain(stale, written.ID) {
		t.Errorf("StaleOpenRuns() = %v; missed %s that OpenRuns sees (read the cache snapshot, not the Live tier)", stale, written.ID)
	}
	orphaned, err := store.OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns: %v", err)
	}
	if !runsContain(orphaned, written.ID) {
		t.Errorf("OrphanedOpenRuns() = %v; missed %s that OpenRuns sees (read the cache snapshot, not the Live tier)", orphaned, written.ID)
	}
}

// TestStaleOpenRunsSeesBeadsWrittenBehindTheCache proves StaleOpenRuns bypasses
// the caching snapshot. Tracking beads are created and closed through the
// dispatcher's own uncached handle, so a CachingStore primed before the write
// never absorbs them: a non-Live read returns zero rows forever while the
// (Live) open-work gate sees the very same beads and holds the order shut
// (#5832).
func TestStaleOpenRunsSeesBeadsWrittenBehindTheCache(t *testing.T) {
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(): %v", err)
	}

	written, err := backing.Create(beads.Bead{
		Title:  "order:digest",
		Labels: []string{"order-run:digest", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Vacuity guard: the primed snapshot is live and never absorbed the write,
	// so the non-Live label read must miss it — otherwise the test proves
	// nothing about the read tier.
	snapshot, err := cache.ListByLabel(labelOrderTracking, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("ListByLabel(snapshot): %v", err)
	}
	if beadsContainID(snapshot, written.ID) {
		t.Fatalf("vacuity violated: snapshot ListByLabel already lists %s; the cache absorbed the backing write", written.ID)
	}

	stale, err := ordersStoreOver(cache).StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if !runsContain(stale, written.ID) {
		t.Fatalf("StaleOpenRuns() = %v; missed %s written behind the cache (read the snapshot, not the Live tier)", stale, written.ID)
	}
}

// TestStaleOpenRunsKeepsInProgressAndExcludesClosed pins the two status
// semantics the Live rewrite must preserve. The pre-fix read passed no Status
// filter, so an in_progress tracking bead was returned; adding Status:"open"
// would silently narrow the sweep and leave in_progress beads gating their
// orders forever — a regression in the same direction as the bug being fixed.
func TestStaleOpenRunsKeepsInProgressAndExcludesClosed(t *testing.T) {
	store := beads.NewMemStore()
	mk := func(title, status string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: title, Labels: []string{"order-run:digest", labelOrderTracking}})
		if err != nil {
			t.Fatal(err)
		}
		if status != "open" {
			if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	openBead := mk("order:digest open", "open")
	inProgress := mk("order:digest running", "in_progress")
	closedBead := mk("order:digest done", "closed")

	runs, err := ordersStoreOver(store).StaleOpenRuns(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	got := make(map[string]bool, len(runs))
	for _, r := range runs {
		got[r.ID] = true
	}
	if !got[openBead.ID] {
		t.Errorf("open tracking bead %s missing from StaleOpenRuns", openBead.ID)
	}
	if !got[inProgress.ID] {
		t.Errorf("in_progress tracking bead %s missing from StaleOpenRuns — a Status:\"open\" filter would strand it", inProgress.ID)
	}
	if got[closedBead.ID] {
		t.Errorf("closed tracking bead %s must not be swept", closedBead.ID)
	}
}

func beadsContainID(list []beads.Bead, id string) bool {
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
