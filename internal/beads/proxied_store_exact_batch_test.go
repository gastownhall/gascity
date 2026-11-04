package beads

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// relocatingBdLeaf is a bd leaf that refuses an id-scoped read of the gcg-
// namespace, as a BdStore with a relocated graph class does
// (guardRelocatedClassIDs), and that answers an exact batch read from the
// ledger it shares with the native leaf, recording each one.
type relocatingBdLeaf struct {
	*recordingLeaf
}

func (l *relocatingBdLeaf) guardRelocatedClassIDs(op string, ids ...string) error {
	for _, id := range ids {
		if strings.HasPrefix(id, "gcg-") {
			return fmt.Errorf("%w: %s", ErrBdSQLClassRelocated, op)
		}
	}
	return nil
}

func (l *relocatingBdLeaf) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	l.note("GetExactBatch")
	found := make(map[string]Bead, len(ids))
	var unresolved []string
	for _, id := range ids {
		b, err := l.Store.Get(id)
		switch {
		case err == nil:
			found[id] = b
		case errors.Is(err, ErrNotFound):
			unresolved = append(unresolved, id)
		default:
			return nil, nil, err
		}
	}
	return found, unresolved, nil
}

// newRelocatingSplitFixture is newSplitFixture with a bd leaf that guards the
// gcg- namespace and has an exact batch read. The shared ledger holds prx-1 and
// a gcg-1 row the native leaf could read if it were asked.
func newRelocatingSplitFixture(t *testing.T) (*ProxiedStore, *relocatingBdLeaf) {
	t.Helper()
	storage := &nativeDoltMemStorage{store: &MemStore{storageOnly: true, IDPrefix: "prx", HonorExplicitIDs: true}}
	native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
	native.idPrefix = "prx"
	writeLeaf := newNativeDoltStoreForTest(storage)
	writeLeaf.idPrefix = "prx"
	for _, id := range []string{"prx-1", "gcg-1"} {
		if _, err := writeLeaf.CreateWithForeignID(Bead{ID: id, Title: id, Type: "task"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	bd := &relocatingBdLeaf{recordingLeaf: &recordingLeaf{Store: writeLeaf}}
	store, err := NewProxiedStore(native, bd, PinForTest("/scope", "", "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	return store, bd
}

// TestProxiedStoreGetExactBatchReadsTheNativeLeaf: while the native leaf
// serves, the batch is its keyed read, and an id in a relocated class's
// reserved namespace is never forwarded to it (H5): it is left unresolved, so
// a caller that resolves it with Get gets the typed refusal, and GetMany reads
// it as a bead this ledger does not hold. The bd leaf is not asked.
func TestProxiedStoreGetExactBatchReadsTheNativeLeaf(t *testing.T) {
	store, bd := newRelocatingSplitFixture(t)
	found, unresolved, err := store.GetExactBatch([]string{"prx-1", "gcg-1", "prx-missing"})
	if err != nil {
		t.Fatalf("GetExactBatch: %v", err)
	}
	if len(found) != 1 || found["prx-1"].ID != "prx-1" {
		t.Errorf("found = %+v, want prx-1 alone: gcg-1 is in a relocated namespace", found)
	}
	if want := []string{"gcg-1", "prx-missing"}; !slices.Equal(unresolved, want) {
		t.Errorf("unresolved = %v, want %v in input order", unresolved, want)
	}
	if calls := bd.took(); len(calls) != 0 {
		t.Errorf("the native read reached the bd leaf: %v", calls)
	}
}

// TestProxiedStoreGetExactBatchFollowsTheDemotion: a demoted store answers with
// the bd leaf's own batch, and with ErrExactBatchGetUnsupported when that leaf
// has none, so a caller falls back to Get.
func TestProxiedStoreGetExactBatchFollowsTheDemotion(t *testing.T) {
	store, bd := newRelocatingSplitFixture(t)
	store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
	found, unresolved, err := store.GetExactBatch([]string{"prx-1", "prx-missing"})
	if err != nil {
		t.Fatalf("demoted GetExactBatch: %v", err)
	}
	if len(found) != 1 || !slices.Equal(unresolved, []string{"prx-missing"}) {
		t.Errorf("demoted GetExactBatch = %+v, %v; want prx-1 found, prx-missing unresolved", found, unresolved)
	}
	if calls := bd.took(); !slices.Equal(calls, []string{"GetExactBatch"}) {
		t.Errorf("demoted bd leaf calls = %v, want its own batch read once", calls)
	}

	batchless, _, _ := newSplitFixture(t)
	batchless.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
	if _, _, err := batchless.GetExactBatch([]string{"prx-1"}); !errors.Is(err, ErrExactBatchGetUnsupported) {
		t.Fatalf("demoted GetExactBatch over a batchless bd leaf = %v, want ErrExactBatchGetUnsupported", err)
	}
}
