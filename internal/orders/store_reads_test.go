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
	if _, err := shared.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest"}}); err != nil {
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
		Labels: []string{"order-run:digest", "exec"},
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
