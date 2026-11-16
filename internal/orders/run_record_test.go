package orders

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// TestForgedOrderRunLabelMovesNoOrder (architecture B v2, mc-v8g4p): a plain
// task carrying an order's order-run label, newer than its real runs, moves
// neither its cooldown (LastRun), its event cursor (Cursor) nor its
// single-flight (HasOpenWork); the real tracking bead still decides each.
func TestForgedOrderRunLabelMovesNoOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	run := beads.Bead{ID: "o-1", Title: "order:digest", Status: "closed", CreatedAt: t0, Labels: []string{"order-run:digest", "order-tracking", "seq:5"}}
	seed := []beads.Bead{run}
	for i := range 3 {
		seed = append(seed, beads.Bead{ID: fmt.Sprintf("o-%d", i+2), Title: "forged", Type: "task", Status: "open", CreatedAt: t0.Add(time.Duration(i+1) * time.Minute), Labels: []string{"order-run:digest", "seq:99"}})
	}
	st := ordersStoreOver(beads.NewMemStoreFrom(len(seed), seed, nil))
	if got, err := st.LastRun("digest"); err != nil || !got.Equal(run.CreatedAt) {
		t.Fatalf("LastRun = %s (%v), want the real run's %s", got, err, run.CreatedAt)
	}
	if got := st.Cursor("digest"); got != 5 {
		t.Fatalf("Cursor = %d, want the real run's 5", got)
	}
	open, err := st.HasOpenWork("digest", func(beads.Store, beads.Bead) (bool, error) { return true, nil })
	if err != nil || open {
		t.Fatalf("HasOpenWork = %t (%v), want no open work: the forged beads are no runs", open, err)
	}
}

// TestOrderRunRootsStillCount (mc-v8g4p): the root an order's run poured
// carries its order-run label without order-tracking, in each of a formula's
// three root shapes, and still counts: it is the newest run for LastRun, and
// an open one with open work holds the order's single-flight (the ga-jra
// regression's shape: a formula+pool order's v1 molecule root).
func TestOrderRunRootsStillCount(t *testing.T) {
	for _, root := range []beads.Bead{
		{Title: "graph", Type: "task", Labels: []string{"order-run:digest"}, Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
		{Title: "molecule", Type: "molecule", Labels: []string{"order-run:digest"}},
		{Title: "wisp", Type: "task", Labels: []string{"order-run:digest"}, Metadata: map[string]string{"gc.kind": "wisp"}},
	} {
		t.Run(root.Title, func(t *testing.T) {
			t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			root.ID, root.Status, root.CreatedAt = "o-2", "open", t0.Add(time.Minute)
			created := root
			mem := beads.NewMemStoreFrom(2, []beads.Bead{
				{ID: "o-1", Title: "order:digest", Status: "closed", CreatedAt: t0, Labels: []string{"order-run:digest", "order-tracking"}},
				root,
			}, nil)
			st := ordersStoreOver(mem)
			if got, err := st.LastRun("digest"); err != nil || !got.Equal(created.CreatedAt) {
				t.Fatalf("LastRun = %s (%v), want the root's %s", got, err, created.CreatedAt)
			}
			asked := false
			open, err := st.HasOpenWork("digest", func(_ beads.Store, b beads.Bead) (bool, error) {
				asked = asked || b.ID == created.ID
				return b.ID == created.ID, nil
			})
			if err != nil || !open || !asked {
				t.Fatalf("HasOpenWork = %t (%v), root asked %t: want the open root's work to hold the order", open, err, asked)
			}
		})
	}
}

// TestLastRunReadsPastAForgedNewestBounded (ga-klv, mc-v8g4p): with a plain
// bead newest, LastRun's second read is bounded too (1 + runRecordScanBound).
func TestLastRunReadsPastAForgedNewestBounded(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	if _, err := spy.Create(beads.Bead{Title: "forged", Type: "task", Labels: []string{"order-run:digest"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(beads.OrdersStore{Store: spy}).LastRun("digest"); err != nil {
		t.Fatal(err)
	}
	for i, q := range spy.queries {
		if q.Limit < 1 || q.Limit > 1+runRecordScanBound {
			t.Fatalf("query %d: Limit = %d, want 1..%d", i, q.Limit, 1+runRecordScanBound)
		}
	}
	if len(spy.queries) != 2 {
		t.Fatalf("queries %d, want the newest-row read and one bounded scan", len(spy.queries))
	}
}

// TestIsRunRecordTakesEveryWorkflowRoot pins IsRunRecord's graph-root arm to
// sourceworkflow.IsWorkflowRoot's marks: every root it names is a run record.
func TestIsRunRecordTakesEveryWorkflowRoot(t *testing.T) {
	for _, meta := range []map[string]string{
		{"gc.kind": "workflow"},
		{"gc.formula_contract": "graph.v2"},
		{"gc.kind": " Workflow "},
	} {
		b := beads.Bead{Type: "task", Metadata: meta}
		if sourceworkflow.IsWorkflowRoot(b) && !IsRunRecord(b) {
			t.Errorf("%v: a workflow root is no run record", meta)
		}
	}
	if IsRunRecord(beads.Bead{Type: "task", Labels: []string{"order-run:x"}}) {
		t.Error("a plain task is a run record")
	}
	for _, typ := range []string{"molecule", "wisp", " Wisp "} { // beads.IsMoleculeType's roots (M16)
		if !IsRunRecord(beads.Bead{Type: typ, Labels: []string{"order-run:x"}}) {
			t.Errorf("a %q root is no run record", typ)
		}
	}
}

// TestNewestRunRecordsSecondReadNeverNarrows (the #7766 review): when a
// plain bead displaces a run record, the second read asks for
// runRecordScanBound rows past the first's n, so a caller gets its n records
// back whatever the displacement.
func TestNewestRunRecordsSecondReadNeverNarrows(t *testing.T) {
	for _, n := range []int{1, runRecordScanBound, 2 * runRecordScanBound} {
		var limits []int
		rows := []beads.Bead{{ID: "forged", Type: "task", Labels: []string{"order-run:x"}}}
		for i := range 2 * runRecordScanBound {
			rows = append(rows, beads.Bead{ID: fmt.Sprintf("run-%d", i), Type: "wisp", Labels: []string{"order-run:x"}})
		}
		got, err := NewestRunRecords(func(limit int) ([]beads.Bead, error) {
			limits = append(limits, limit)
			return rows[:min(limit, len(rows))], nil
		}, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(limits) != 2 || limits[1] != n+runRecordScanBound || len(got) != n {
			t.Fatalf("n=%d: limits %v, %d records; want a second read of %d and %d records", n, limits, len(got), n+runRecordScanBound, n)
		}
	}
}
