package beads

import (
	"sync"
	"testing"
)

func TestAtomicCloseMemStoreIsOptIn(t *testing.T) {
	if _, ok := AtomicConditionalCloserFor(NewMemStore()); ok {
		t.Fatal("plain MemStore unexpectedly exposes atomic terminal close")
	}
	store := NewAtomicCloseMemStore()
	closer, ok := AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatal("opt-in atomic MemStore does not expose atomic terminal close")
	}
	created, err := store.Create(Bead{Title: "atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{
		"state": "drained",
	})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Revision != created.Revision+1 || closed.Metadata["state"] != "drained" {
		t.Fatalf("closed bead = %#v, want one fresh terminal revision", closed)
	}
	closed.Metadata["state"] = "corrupted caller copy"
	fresh, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Metadata["state"] != "drained" {
		t.Fatalf("returned bead aliases store metadata: %#v", fresh)
	}
}

func TestAtomicCloseMemStoreRejectsStaleRevisionWithoutTerminalMutation(t *testing.T) {
	store := NewAtomicCloseMemStore()
	closer, ok := AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatal("atomic closer unavailable")
	}
	created, err := store.Create(Bead{Title: "stale atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision-1, map[string]string{"state": "drained"}); !IsPreconditionFailed(err) {
		t.Fatalf("stale close error = %v, want precondition failure", err)
	}
	fresh, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Status != "open" || fresh.Revision != created.Revision || fresh.Metadata["state"] != "" {
		t.Fatalf("stale close mutated row: %#v", fresh)
	}
}

func TestAtomicCloseMemStoreHasOneSameRevisionWinner(t *testing.T) {
	store := NewAtomicCloseMemStore()
	closer, ok := AtomicConditionalCloserFor(store)
	if !ok {
		t.Fatal("atomic closer unavailable")
	}
	created, err := store.Create(Bead{Title: "racing atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	type result struct {
		bead Bead
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, winner := range []string{"first", "second"} {
		winner := winner
		go func() {
			ready.Done()
			<-start
			bead, closeErr := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{
				"state": "drained", "winner": winner,
			})
			results <- result{bead: bead, err: closeErr}
		}()
	}
	ready.Wait()
	close(start)

	wins, losses := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			wins++
			if result.bead.Status != "closed" || result.bead.Metadata["state"] != "drained" {
				t.Fatalf("winner returned non-terminal row: %#v", result.bead)
			}
			continue
		}
		if !IsPreconditionFailed(result.err) {
			t.Fatalf("losing close error = %v, want precondition failure", result.err)
		}
		losses++
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("same-revision close winners/losses = %d/%d, want 1/1", wins, losses)
	}
	fresh, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get final row: %v", err)
	}
	if fresh.Status != "closed" || fresh.Metadata["state"] != "drained" || fresh.Metadata["winner"] == "" || fresh.Revision != created.Revision+1 {
		t.Fatalf("final row = %#v, want one atomic terminal winner", fresh)
	}
}
