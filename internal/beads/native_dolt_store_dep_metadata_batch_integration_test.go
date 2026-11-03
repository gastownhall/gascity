//go:build integration

package beads

import (
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
