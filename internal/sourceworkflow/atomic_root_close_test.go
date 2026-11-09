package sourceworkflow

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// atomicTxStore is a store whose Tx commits atomically; it counts its Txs.
type atomicTxStore struct {
	*beads.MemStore
	txs int
}

func (s *atomicTxStore) AtomicTx() bool { return true }

func (s *atomicTxStore) Tx(msg string, fn func(beads.Tx) error) error {
	s.txs++
	return s.MemStore.Tx(msg, fn)
}

// TestCloseWorkflowRootMarkerTakesTheAtomicTxThroughATarget pins that the
// root's close-only marker is written in one atomic Tx when the store has one,
// reached through the molecule target (typed-stores T5a) as it was through the
// raw store.
func TestCloseWorkflowRootMarkerTakesTheAtomicTxThroughATarget(t *testing.T) {
	for name, target := range map[string]func(beads.Store) beads.MoleculeTarget{
		"a graph target": beadstest.GraphMolecule,
		"a work target":  beadstest.WorkMolecule,
	} {
		store := &atomicTxStore{MemStore: beads.NewMemStore()}
		root, err := store.Create(beads.Bead{Title: "root", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		n, err := CloseWorkflowSubtreeAs(target(store), root.ID, "canceled", "test cancel", map[string]string{"gc.cancel_requested": "true"})
		if err != nil || n != 1 {
			t.Fatalf("%s: CloseWorkflowSubtreeAs = %d, %v; want 1, nil", name, n, err)
		}
		if store.txs != 1 {
			t.Errorf("%s: the root closed in %d Txs, want 1: the target hid the store's atomic Tx", name, store.txs)
		}
		got, err := store.Get(root.ID)
		if err != nil || got.Status != "closed" || got.Metadata["gc.cancel_requested"] != "true" {
			t.Errorf("%s: root = %+v (%v), want closed with the marker", name, got, err)
		}
	}
}
