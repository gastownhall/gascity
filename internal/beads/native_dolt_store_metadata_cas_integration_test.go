//go:build integration

package beads

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// openRealNativeDoltStoreForCAS opens a NativeDoltStore over REAL upstream
// native storage. The narrow CAS contract is a claim about the backend's
// transaction semantics, and the in-memory fixture used by the unit-level
// conformance cannot answer it: nativeDoltMemStorage.RunInTransaction
// snapshots for rollback and then runs the callback UNLOCKED, so it models
// atomicity but provides no isolation whatsoever.
func openRealNativeDoltStoreForCAS(t *testing.T, actor string) *NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
}

// TestNativeDoltStoreMetadataCASSequentialAgainstRealDolt exercises the
// sequential value-CAS contract — both pinned traps — against real storage.
func TestNativeDoltStoreMetadataCASSequentialAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "cas-sequential")

	b, err := store.Create(Bead{Title: "real-dolt-cas"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID

	// Trap 1: expected "" claims an ABSENT key.
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "one"); err != nil || !ok {
		t.Fatalf("claim absent key: (%v, %v), want (true, nil)", ok, err)
	}
	// ...and also a PRESENT-AND-EMPTY key.
	if err := store.SetMetadata(id, "k", ""); err != nil {
		t.Fatalf("SetMetadata clear: %v", err)
	}
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "two"); err != nil || !ok {
		t.Fatalf("claim empty-valued key: (%v, %v), want (true, nil)", ok, err)
	}
	// ...but never a non-empty one.
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "three"); err != nil || ok {
		t.Fatalf("claim non-empty key with empty expected: (%v, %v), want (false, nil)", ok, err)
	}

	// Trap 2: a genuine mismatch is (false, nil), never an error.
	ok, err := store.CompareAndSetMetadataKey(id, "k", "WRONG", "four")
	if err != nil {
		t.Fatalf("value-mismatch CAS returned error: %v (want nil)", err)
	}
	if ok {
		t.Fatal("value-mismatch CAS returned true (want false)")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["k"] != "two" {
		t.Fatalf("value = %q, want %q", got.Metadata["k"], "two")
	}
}

// TestNativeDoltStoreMetadataCASContentionAgainstRealDolt is the load-bearing
// test for the lease lane: under concurrency exactly ONE racer may win a claim
// from a single starting value. This is the property the in-memory fixture
// cannot evaluate, and the property D3/D5 leases and target_scope member
// declaration actually depend on — a CAS that admits two winners hands the
// same lease to two holders.
func TestNativeDoltStoreMetadataCASContentionAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "cas-contention")

	b, err := store.Create(Bead{Title: "real-dolt-cas-contention"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID
	if err := store.SetMetadata(id, "lease", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(racer int) {
			defer wg.Done()
			holder := "holder-" + strconv.Itoa(racer)
			<-start
			ok, err := store.CompareAndSetMetadataKey(id, "lease", "", holder)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok {
				winners = append(winners, holder)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("racer returned an error (a lost race must be (false, nil)): %v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d %v, want exactly 1 — no mutual exclusion, so this CAS cannot carry a lease",
			len(winners), winners)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["lease"] != winners[0] {
		t.Fatalf("stored lease = %q, want the sole winner %q", got.Metadata["lease"], winners[0])
	}
}

// TestNativeDoltStoreMetadataCASContentionAcrossIndependentHandles is the
// multi-writer leg, and it is the one that actually decides whether this CAS
// can carry a lease.
//
// The single-handle contention test above cannot distinguish a fence enforced
// by the DATABASE from exclusion accidentally provided by shared in-process
// state (a connection pool, a handle-level lock). The gascity Dolt database is
// multi-writer by design — the bd CLI, other gascity processes and graph-apply
// all write it — so a guard that only holds within one store handle is not a
// fence at all, which is precisely why a store-maintained counter was rejected
// as a revision token.
//
// Racing two INDEPENDENTLY OPENED storage handles over the same database
// directory reproduces that condition inside one test binary: the handles
// share no Go-level state, so any exclusion observed here is enforced below
// them.
func TestNativeDoltStoreMetadataCASContentionAcrossIndependentHandles(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".beads")

	openHandle := func(actor string) *NativeDoltStore {
		t.Helper()
		storage, err := beadslib.OpenBestAvailable(ctx, dir)
		if err != nil {
			t.Skipf("upstream native beads storage unavailable: %v", err)
		}
		t.Cleanup(func() {
			if err := storage.Close(); err != nil {
				t.Logf("close upstream storage (%s): %v", actor, err)
			}
		})
		if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
			t.Fatalf("set issue prefix (%s): %v", actor, err)
		}
		return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
	}

	writerA := openHandle("cas-writer-a")
	writerB := openHandle("cas-writer-b")

	b, err := writerA.Create(Bead{Title: "cross-handle-cas-contention"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID
	if err := writerA.SetMetadata(id, "lease", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	// The second handle must observe the bead before racing for it, otherwise
	// a miss proves nothing about the fence.
	if got, err := writerB.Get(id); err != nil || got.ID != id {
		t.Fatalf("second handle cannot see bead %q: (%v, %v)", id, got.ID, err)
	}

	type result struct {
		holder string
		won    bool
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, racer := range []struct {
		store  *NativeDoltStore
		holder string
	}{{writerA, "holder-A"}, {writerB, "holder-B"}} {
		go func(s *NativeDoltStore, holder string) {
			<-start
			won, err := s.CompareAndSetMetadataKey(id, "lease", "", holder)
			results <- result{holder: holder, won: won, err: err}
		}(racer.store, racer.holder)
	}
	close(start)

	var winners []string
	for range 2 {
		r := <-results
		if r.err != nil {
			// A conflict surfaced as an error is NOT contract-conformant: the
			// contract says a lost race is (false, nil). Report it as the
			// contract violation it is rather than tolerating it.
			t.Errorf("racer %s returned an error (a lost race must be (false, nil)): %v", r.holder, r.err)
			continue
		}
		if r.won {
			winners = append(winners, r.holder)
		}
	}
	if t.Failed() {
		return
	}
	if len(winners) != 1 {
		t.Fatalf("winners across independent handles = %d %v, want exactly 1 — the fence does not hold "+
			"between writers, so this CAS cannot carry a lease in the multi-writer Dolt database",
			len(winners), winners)
	}

	// Both handles must agree on who holds the lease.
	for name, s := range map[string]*NativeDoltStore{"writerA": writerA, "writerB": writerB} {
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("%s Get: %v", name, err)
		}
		if got.Metadata["lease"] != winners[0] {
			t.Fatalf("%s sees lease %q, want the sole winner %q", name, got.Metadata["lease"], winners[0])
		}
	}
}
