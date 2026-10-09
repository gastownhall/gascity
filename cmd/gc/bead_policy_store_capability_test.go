package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The policy wrapper is the store a cmd/gc caller actually holds, and wrapping
// a store is how capabilities get lost — silently, because the caller's type
// assertion simply stops matching and it takes a slower or weaker path.
//
// TestEmittingClassStoreKeepsEveryEngineCapability has pinned that for the
// emitting class store since it was written. The policy wrapper had no such
// pin, and it is the one the recovery walk holds: on a live hosted city
// `gc storage recover-stranded` asked it for the batched dep read, was told no
// by a failed assertion against *main.beadPolicyGraphStore, and read 231 beads
// one round trip at a time over a link that drops MySQL handshakes (ga-50tsx).
// The batch existed on the engine underneath the whole time.
func TestBeadPolicyStoreForwardsTheBatchedDepRead(t *testing.T) {
	// Both shapes wrapStoreWithBeadPolicies can return: the plain policy store,
	// and the graph-apply variant a graph-capable engine produces. The recovery
	// walk held the second one.
	for _, tc := range []struct {
		name    string
		wrapped beads.Store
	}{
		{"policy store", wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{})},
		{"policy graph store", wrapStoreWithBeadPolicies(mustOpenSQLiteStoreForCapabilityTest(t), &config.City{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := beads.DepListBatchFor(tc.wrapped); !ok {
				t.Fatalf("%T does not advertise DepListBatch; a caller asking this wrapper for the batch is told no "+
					"by a failed type assertion and silently reads one anchor per round trip (ga-50tsx)", tc.wrapped)
			}
		})
	}
}

// The engine the recovery's SOURCE is served from really does have the
// capability the wrapper forwards — otherwise the forwarding test above would
// pass over a tree where nothing can batch and prove nothing.
//
// SQLiteStore is deliberately absent: it backs the local binding the recovery
// writes INTO, where a round trip is a function call, so it never needed a
// batch. Forwarding over it returns ErrDepListBatchUnsupported and the caller
// falls back, which is the case the next test pins.
func TestBatchedDepReadExistsOnTheSourceEngines(t *testing.T) {
	for _, engine := range []reflect.Type{
		reflect.TypeOf(&beads.NativeDoltStore{}),
		reflect.TypeOf(&beads.MemStore{}),
	} {
		if _, ok := engine.MethodByName("DepListBatch"); !ok {
			t.Errorf("%s has no DepListBatch, so forwarding it through a wrapper cannot help any caller", engine)
		}
	}
}

// A forwarded capability whose backing store does not have it must say so,
// not answer with an empty graph.
//
// This is the failure mode the sentinel exists to prevent: a wrapper that
// forwards DepListBatch and returns (nil, nil) over a store that cannot batch
// hands the caller "every anchor has no edges", which a dependency walk cannot
// distinguish from the truth.
func TestBeadPolicyStoreDepListBatchReportsAnUnsupportedBacking(t *testing.T) {
	wrapped := wrapStoreWithBeadPolicies(storeWithoutBatchedDepRead{Store: beads.NewMemStore()}, &config.City{})
	batch, ok := beads.DepListBatchFor(wrapped)
	if !ok {
		t.Fatal("the policy wrapper does not advertise DepListBatch at all, so the capability is invisible again")
	}
	got, err := batch.DepListBatch([]string{"gc-1"})
	if !errors.Is(err, beads.ErrDepListBatchUnsupported) {
		t.Fatalf("DepListBatch over a non-batching backing = (%v, %v), want ErrDepListBatchUnsupported", got, err)
	}
	if got != nil {
		t.Errorf("DepListBatch returned a %d-entry map alongside its refusal; a caller reading it sees a graph with no edges", len(got))
	}
}

// TestBeadPolicyStoreDepListBatchAgreesWithDepList is the correctness control
// for the forward: a wrapper that reaches a DIFFERENT answer than the single
// read it batches is worse than no wrapper at all.
func TestBeadPolicyStoreDepListBatchAgreesWithDepList(t *testing.T) {
	leaf := beads.NewMemStore()
	a := mustCreateInfraBead(t, leaf, beads.Bead{Title: "a", Type: "message"})
	b := mustCreateInfraBead(t, leaf, beads.Bead{Title: "b", Type: "message"})
	if err := leaf.DepAdd(b.ID, a.ID, "blocks"); err != nil {
		t.Fatalf("seeding the edge: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(leaf, &config.City{})
	batch, ok := beads.DepListBatchFor(wrapped)
	if !ok {
		t.Fatal("the policy wrapper does not advertise DepListBatch")
	}

	batched, err := batch.DepListBatch([]string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	compare := func() string {
		for _, id := range []string{a.ID, b.ID} {
			single, err := wrapped.DepList(id, "down")
			if err != nil {
				return "DepList(" + id + "): " + err.Error()
			}
			if len(single) == 0 && len(batched[id]) == 0 {
				continue
			}
			if !reflect.DeepEqual(single, batched[id]) {
				return "anchor " + id + " differs"
			}
		}
		return ""
	}
	if diff := compare(); diff != "" {
		t.Fatalf("the wrapper's batch does not answer what its own DepList answers: %s", diff)
	}

	// The control. Add an edge between the two reads and require the same
	// comparison to go red, so a comparator that could not see a difference
	// cannot pass for one that did.
	if err := leaf.DepAdd(a.ID, b.ID, "related"); err != nil {
		t.Fatalf("mutating between the reads: %v", err)
	}
	if diff := compare(); diff == "" {
		t.Fatal("the comparison passed after an edge was added between the two reads, so it cannot fail and proves nothing")
	}
}

// storeWithoutBatchedDepRead hides a leaf store's batch capability, standing in
// for a backing engine that never had one. Embedding the INTERFACE is what hides
// it: Go promotes only the interface's own methods, which is the same mechanism
// that lost the capability in production.
type storeWithoutBatchedDepRead struct{ beads.Store }

// mustOpenSQLiteStoreForCapabilityTest opens the graph-capable engine, so the
// graph-apply shape of the policy wrapper is pinned too.
func mustOpenSQLiteStoreForCapabilityTest(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("opening the sqlite engine: %v", err)
	}

	return store
}
