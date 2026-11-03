package beads

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func openBatchTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

// TestSQLiteCreateWithForeignIDBatchIsAllOrNothing: the batch keeps every id
// and every field, as CreateWithForeignID does row by row, and one duplicate
// rolls the whole call back.
func TestSQLiteCreateWithForeignIDBatchIsAllOrNothing(t *testing.T) {
	s := openBatchTestSQLite(t)
	at := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	rows := []Bead{
		{ID: "mc-1", Title: "one", Type: "message", Status: "open", CreatedAt: at, Metadata: StringMap{"k": "v"}, Labels: []string{"a"}},
		{ID: "mc-2", Title: "two", Type: "session", Status: "closed", CreatedAt: at.Add(time.Second), Ephemeral: true},
	}
	if err := s.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatalf("CreateWithForeignIDBatch: %v", err)
	}
	reference := openBatchTestSQLite(t)
	for _, b := range rows {
		want, err := reference.CreateWithForeignID(b)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", b.ID, err)
		}
		got.Revision, want.Revision = 0, 0
		if !reflect.DeepEqual(got, want) {
			t.Errorf("batch row %s = %+v, want what CreateWithForeignID stores: %+v", b.ID, got, want)
		}
	}

	err := s.CreateWithForeignIDBatch([]Bead{{ID: "mc-3", Title: "three", Type: "message"}, {ID: "mc-1", Title: "again", Type: "message"}})
	if err == nil {
		t.Fatal("a batch holding an id the store already holds was accepted")
	}
	if _, err := s.Get("mc-3"); err == nil {
		t.Fatal("a refused batch left mc-3 behind: the call was not all or nothing")
	}
	if err := s.CreateWithForeignIDBatch([]Bead{{ID: " ", Title: "blank"}}); err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("a blank id = %v, want refused", err)
	}
}

// TestSQLiteDepBatchesAgreeWithTheirSingleEdgeForms: DepAddBatch stores each
// edge and payload as DepAddWithMetadata does, DepListBatch lists what DepList
// lists per held anchor and nothing for an absent one, and DepMetadataBatch
// answers what DepMetadata answers edge by edge.
func TestSQLiteDepBatchesAgreeWithTheirSingleEdgeForms(t *testing.T) {
	s := openBatchTestSQLite(t)
	if err := s.CreateWithForeignIDBatch([]Bead{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}); err != nil {
		t.Fatal(err)
	}
	edges := []DepWithPayload{
		{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}, Payload: `{"gate":"any-children"}`},
		{Dep: Dep{IssueID: "a", DependsOnID: "c", Type: "blocks"}},
		{Dep: Dep{IssueID: "b", DependsOnID: "c", Type: "blocks"}, Payload: "{}"},
		{Dep: Dep{IssueID: "c", DependsOnID: "external:x", Type: "tracks"}, Payload: `{"why":"x"}`},
	}
	if err := s.DepAddBatch(edges); err != nil {
		t.Fatalf("DepAddBatch: %v", err)
	}
	// An edge row whose anchor the store does not hold (a dangling deps
	// row): DepListBatch keys by held anchor, so it still gets no entry.
	if err := s.DepAddBatch([]DepWithPayload{{Dep: Dep{IssueID: "ghost", DependsOnID: "a", Type: "blocks"}}}); err != nil {
		t.Fatal(err)
	}
	listed, err := s.DepListBatch([]string{"a", "b", "c", "absent", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"absent", "ghost"} {
		if _, ok := listed[id]; ok {
			t.Errorf("anchor %s, which the store does not hold, has an entry", id)
		}
	}
	payloads, err := s.DepMetadataBatch([]string{"a", "b", "c", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		want, err := s.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		if got := listed[id]; len(got) != len(want) {
			t.Errorf("DepListBatch[%s] = %v, want DepList's %v", id, got, want)
		}
		for _, d := range want {
			payload, carried, err := s.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			got, inBatch := payloads[DepPair{IssueID: id, DependsOnID: d.DependsOnID}]
			if inBatch != carried || got != payload {
				t.Errorf("DepMetadataBatch %s -> %s = (%q, %v), want DepMetadata's (%q, %v)", id, d.DependsOnID, got, inBatch, payload, carried)
			}
		}
	}
	if len(payloads) != 2 {
		t.Errorf("DepMetadataBatch = %v, want exactly the two carrying edges", payloads)
	}
	if err := s.DepAddBatch([]DepWithPayload{{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}}}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DepMetadataBatch([]string{"a"}); len(again) != 0 {
		t.Errorf("re-adding an edge with no payload left %v: it must clear the sidecar as DepAddWithMetadata does", again)
	}
}
