//go:build gascity_native_beads

package beads

import "testing"

// TestDoltliteReadStoreShadowsAtomicTerminalClose pins the wrapper obligation
// the ConditionalWriter shadows established: a capability promoted from the
// embedded *BdStore must not hand callers a closer that writes through bd while
// this wrapper's SQL read caches go on serving the pre-close row.
//
// Unlike the four fenced ConditionalWriter verbs, this capability is forwarded
// rather than degraded: the fused terminal close fences on bd's --if-status
// guard against a row BdStore reads back itself, so it never depends on the
// revision-less doltlite read path the F2 veto exists for.
func TestDoltliteReadStoreShadowsAtomicTerminalClose(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	scripted := v59Bd(map[string]string{"state": "awake"})
	store.BdStore = NewBdStore("/city", scripted.runner)

	closer, ok := AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatal("DoltliteReadStore over a status-guard-capable bd does not advertise the atomic terminal close")
	}
	if _, isWrapper := closer.(*DoltliteReadStore); !isWrapper {
		t.Fatalf("resolved closer = %T, want *DoltliteReadStore so the read caches are invalidated", closer)
	}

	store.orderRunMu.Lock()
	store.orderRunHash = "stale"
	store.orderRunMu.Unlock()
	store.readyMu.Lock()
	store.readyHash = "stale"
	store.readyMu.Unlock()

	closed, err := closer.CloseWithMetadataIfMatch("ga-1", 41, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch through the doltlite wrapper: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("closed row = status %q state %q, want closed/drained", closed.Status, closed.Metadata["state"])
	}

	store.orderRunMu.Lock()
	orderRunHash := store.orderRunHash
	store.orderRunMu.Unlock()
	store.readyMu.Lock()
	readyHash := store.readyHash
	store.readyMu.Unlock()
	if orderRunHash != "" || readyHash != "" {
		t.Fatalf("read caches survived the terminal close: orderRunHash=%q readyHash=%q", orderRunHash, readyHash)
	}
}
