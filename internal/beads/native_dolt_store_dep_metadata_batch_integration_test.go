//go:build integration

package beads

import (
	"fmt"
	"testing"
)

// TestNativeDoltStoreDepMetadataBatchAgreesWithDepMetadata runs on a throwaway
// sql-server (startTestDoltServer: a free non-reserved port, stopped by its
// process): the batched payload read answers, edge for edge, what DepMetadata
// answers, carrying payloads included, "{}" and absent edges as none.
func TestNativeDoltStoreDepMetadataBatchAgreesWithDepMetadata(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	ids := map[string]string{}
	for _, title := range []string{"root", "gate", "plain", "far"} {
		b, err := store.Create(Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		ids[title] = b.ID
	}
	edges := []struct{ from, to, kind, payload string }{
		{"gate", "root", "waits-for", `{"gate":"any-children"}`},
		{"plain", "root", "blocks", ""},
		{"far", "gate", "blocks", "{}"},
	}
	for _, e := range edges {
		if err := store.DepAdd(ids[e.from], ids[e.to], e.kind); err != nil {
			t.Fatal(err)
		}
		if e.payload == "" {
			continue
		}
		res, err := db.Exec("UPDATE dependencies SET metadata = ? WHERE issue_id = ?", e.payload, ids[e.from])
		if err != nil {
			t.Fatalf("writing the payload: %v", err)
		}
		// "{}" is the column's default, so that write changes nothing.
		if n, _ := res.RowsAffected(); n != 1 && e.payload != "{}" {
			t.Fatalf("the payload write on %s -> %s touched %d rows, want 1", e.from, e.to, n)
		}
	}
	anchors := []string{ids["root"], ids["gate"], ids["plain"], ids["far"], "gc-absent"}
	got, err := store.DepMetadataBatch(anchors)
	if err != nil {
		t.Fatalf("DepMetadataBatch: %v", err)
	}
	carrying := 0
	for _, e := range edges {
		payload, carried, err := store.DepMetadata(ids[e.from], ids[e.to])
		if err != nil {
			t.Fatal(err)
		}
		batch, inBatch := got[DepPair{IssueID: ids[e.from], DependsOnID: ids[e.to]}]
		if inBatch != carried || batch != payload {
			t.Fatalf("edge %s -> %s: batch (%q, %v), DepMetadata (%q, %v)", e.from, e.to, batch, inBatch, payload, carried)
		}
		if carried {
			carrying++
		}
	}
	if carrying != 1 || len(got) != 1 {
		t.Fatalf("DepMetadataBatch = %v, want exactly the one carrying edge", got)
	}
}

// TestNativeDoltStoreDepBatchesAgreeAcrossChunkBoundaries (review S): with
// more anchors than one EdgeReader read carries (nativeDepListBatchChunk),
// DepListBatch and DepMetadataBatch still answer every anchor, edge and
// payload DepList and DepMetadata answer one at a time.
func TestNativeDoltStoreDepBatchesAgreeAcrossChunkBoundaries(t *testing.T) {
	const anchors = 2*nativeDepListBatchChunk + 5
	store, db := openServerNativeStoreForTest(t)
	ids := make([]string, 0, anchors)
	for i := 0; i < anchors; i++ {
		b, err := store.Create(Bead{Title: fmt.Sprintf("anchor %d", i), Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	want := 0
	for i := 1; i < anchors; i++ {
		if err := store.DepAdd(ids[i], ids[i-1], "blocks"); err != nil {
			t.Fatal(err)
		}
		if i%3 != 0 {
			continue
		}
		if _, err := db.Exec("UPDATE dependencies SET metadata = ? WHERE issue_id = ?", fmt.Sprintf(`{"n":%d}`, i), ids[i]); err != nil {
			t.Fatal(err)
		}
		want++
	}
	listed, err := store.DepListBatch(ids)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := store.DepMetadataBatch(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != anchors || len(payloads) != want {
		t.Fatalf("DepListBatch answered %d anchors (want %d), DepMetadataBatch %d payloads (want %d)", len(listed), anchors, len(payloads), want)
	}
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(listed[id]) != len(deps) {
			t.Fatalf("DepListBatch[%s] = %v, want DepList's %v", id, listed[id], deps)
		}
		for _, d := range deps {
			payload, carried, err := store.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := payloads[DepPair{IssueID: id, DependsOnID: d.DependsOnID}]
			if ok != carried || got != payload {
				t.Fatalf("DepMetadataBatch %s -> %s = (%q, %v), want (%q, %v)", id, d.DependsOnID, got, ok, payload, carried)
			}
		}
	}
}

// TestNativeDoltStoreDepRemoveBatchRemovesInOneCommit: the batch removes every
// edge it names, a pair that is not there included, in one Dolt commit where
// DepRemove takes one per edge, and leaves every other edge in place.
func TestNativeDoltStoreDepRemoveBatchRemovesInOneCommit(t *testing.T) {
	store, db := openServerNativeStoreForTest(t)
	var ids []string
	for i := 0; i < 5; i++ {
		b, err := store.Create(Bead{Title: fmt.Sprintf("row %d", i), Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	for i := 1; i < len(ids); i++ {
		if err := store.DepAdd(ids[i], ids[0], "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	commits := func() int {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM dolt_log").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := commits()
	if err := store.DepRemoveBatch([]DepPair{{ids[1], ids[0]}, {ids[2], ids[0]}, {ids[3], ids[0]}, {ids[0], ids[4]}}); err != nil {
		t.Fatalf("DepRemoveBatch: %v", err)
	}
	if got := commits() - before; got != 1 {
		t.Fatalf("DepRemoveBatch took %d commit(s), want 1", got)
	}
	up, err := store.DepList(ids[0], "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(up) != 1 || up[0].IssueID != ids[4] {
		t.Fatalf("the dependents left of %s = %+v, want only %s", ids[0], up, ids[4])
	}
}
