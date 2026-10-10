package beadstest

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestOpRecordingStoreRecordsReadsAndWrites(t *testing.T) {
	rec := NewOpRecordingStore(nil)
	created, err := rec.Create(beads.Bead{Title: "mail", Type: "message", Assignee: "worker"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := rec.Get(created.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := rec.Get("missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if _, err := rec.List(beads.ListQuery{Type: "message", Assignees: []string{"worker", "gc-1"}}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := rec.ListByMetadata(map[string]string{"alias": "worker"}, 0); err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if err := rec.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ops := rec.Ops()
	kinds := make([]string, 0, len(ops))
	for _, op := range ops {
		kinds = append(kinds, op.Kind)
	}
	want := []string{OpCreate, OpGet, OpGet, OpList, OpList, OpClose}
	if len(kinds) != len(want) {
		t.Fatalf("ops = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("ops = %v, want %v", kinds, want)
		}
	}
	if !ops[2].NotFound() || ops[1].NotFound() {
		t.Fatalf("NotFound: hit=%v miss=%v, want false/true", ops[1].NotFound(), ops[2].NotFound())
	}
	if ops[4].Method != "ListByMetadata" || ops[4].Query.Metadata["alias"] != "worker" {
		t.Fatalf("ListByMetadata recorded as %+v, want its List-composed query", ops[4])
	}
}

func TestOpRecordingStoreDeepCopiesTheQuery(t *testing.T) {
	rec := NewOpRecordingStore(nil)
	assignees := []string{"worker"}
	meta := map[string]string{"alias": "worker"}
	if _, err := rec.List(beads.ListQuery{Assignees: assignees, Metadata: meta}); err != nil {
		t.Fatalf("List: %v", err)
	}
	assignees[0] = "MUTATED"
	meta["alias"] = "MUTATED"
	got := rec.Ops()[0].Query
	if got.Assignees[0] != "worker" || got.Metadata["alias"] != "worker" {
		t.Fatalf("recorded query changed with the caller's slices: %+v", got)
	}
}

func TestOpRecordingStoreForwardsConditionalWrites(t *testing.T) {
	rec := NewOpRecordingStore(beads.NewMemStore())
	created, err := rec.Create(beads.Bead{Title: "fenced", Metadata: map[string]string{"state": "asleep"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	swapped, err := rec.CompareAndSetMetadataKey(created.ID, "state", "asleep", "active")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", swapped, err)
	}
	if ops := rec.Ops(); ops[len(ops)-1].Kind != OpCompareAndSetMetadataKey {
		t.Fatalf("last op = %s, want %s", ops[len(ops)-1].Kind, OpCompareAndSetMetadataKey)
	}
}

func TestOpRecordingStoreRefusesACapabilityTheDelegateLacks(t *testing.T) {
	rec := NewOpRecordingStore(storeWithoutCapabilities{beads.NewMemStore()})
	if _, err := rec.DepListBatch([]string{"gc-1"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DepListBatch over a store without it = %v, want ErrUnsupported", err)
	}
}

// storeWithoutCapabilities hides every optional capability of its store.
type storeWithoutCapabilities struct{ beads.Store }

func TestWireModelPricesRecordedOps(t *testing.T) {
	model := WireModel{Handshake: 2, LedgerRows: 4000}
	ops := []RecordedOp{
		{Kind: OpGet},
		{Kind: OpGet, Err: beads.ErrNotFound},
		{Kind: OpList, Method: "List", Query: beads.ListQuery{Label: "gc:session"}},
		{Kind: OpList, Method: "List", Query: beads.ListQuery{Type: "wisp", AllowScan: true}},
		{Kind: OpUpdate},
		{Kind: OpClose},
		{Kind: OpGetLocalString},
	}
	cost := model.Price(1, ops)
	// open 2 + Gets 2 + keyed list 1 + unkeyed list 20 pages + Update 1 + Close 2.
	if cost.Requests != 28 {
		t.Fatalf("Requests = %d, want 28 (%s)", cost.Requests, cost)
	}
	if cost.Unkeyed != 1 || cost.GetMisses != 1 || cost.Writes != 2 || cost.Lists != 2 {
		t.Fatalf("cost = %s, want 1 unkeyed, 1 miss, 2 writes, 2 lists", cost)
	}
	if len(cost.UnkeyedQueries) != 1 {
		t.Fatalf("UnkeyedQueries = %v, want the one walk", cost.UnkeyedQueries)
	}
}
