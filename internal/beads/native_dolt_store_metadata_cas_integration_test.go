//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypesAgainstRealDolt
// retains one real-storage proof for the raw JSON metadata boundary. The fast
// in-memory test owns the branch detail; this test proves the CAS preserves the
// exact durable sibling representation exposed by upstream Dolt.
func TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypesAgainstRealDolt(t *testing.T) {
	ctx := context.Background()
	store := openRealNativeDoltStoreForCAS(t, "cas-mixed-metadata")
	created, err := store.Create(Bead{Title: "real Dolt mixed metadata CAS"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seed := json.RawMessage(`{
		"lease":"old",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`)
	storage, release, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("acquire storage for fixture: %v", err)
	}
	if err := storage.UpdateIssue(
		ctx,
		created.ID,
		map[string]interface{}{"metadata": seed},
		"mixed-metadata-fixture",
	); err != nil {
		release()
		t.Fatalf("seed mixed metadata: %v", err)
	}
	release()
	storage, release, err = store.acquireStorage()
	if err != nil {
		t.Fatalf("reacquire storage for pre-CAS read: %v", err)
	}
	preCAS, err := storage.GetIssue(ctx, created.ID)
	release()
	if err != nil {
		t.Fatalf("GetIssue before CAS: %v", err)
	}
	var preRaw map[string]json.RawMessage
	if err := json.Unmarshal(preCAS.Metadata, &preRaw); err != nil {
		t.Fatalf("decode pre-CAS metadata: %v", err)
	}
	largeNumberBefore := string(preRaw["large_number_sibling"])
	if largeNumberBefore == "" {
		t.Fatal("pre-CAS metadata lacks the large numeric sibling")
	}

	swapped, err := store.CompareAndSetMetadataKey(created.ID, "lease", "old", "1")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", swapped, err)
	}

	storage, release, err = store.acquireStorage()
	if err != nil {
		t.Fatalf("reacquire storage for readback: %v", err)
	}
	issue, err := storage.GetIssue(ctx, created.ID)
	release()
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	assertMixedMetadataCASResult(t, issue.Metadata, largeNumberBefore)
}

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
		t.Fatalf("open upstream native beads storage: %v", err)
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

// TestNativeDoltStoreConditionalWriterRequireAgainstRealOpenBestAvailable
// proves that the exact upstream production constructor resolves the required
// conditional-write capability and executes all three revision-fenced verbs.
func TestNativeDoltStoreConditionalWriterRequireAgainstRealOpenBestAvailable(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-writer-require")
	store.stampConditionalWritesMode(gate.Require, false)

	writer, diagnostic, err := ResolveConditionalWriter(store)
	if err != nil || diagnostic != nil || writer == nil {
		t.Fatalf("ResolveConditionalWriter = (%T, %+v, %v), want writer, nil, nil", writer, diagnostic, err)
	}

	created, err := store.Create(Bead{Title: "conditional-writer-real"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if created.Revision == 0 {
		t.Fatal("revision after create = 0, want a live token")
	}
	title := "conditional-writer-updated"
	if err := writer.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if updated.Title != title {
		t.Fatalf("title after update = %q, want %q", updated.Title, title)
	}
	if updated.Revision == created.Revision {
		t.Fatalf("revision after update = %d, want a fresh token", updated.Revision)
	}

	if err := writer.CloseIfMatch(updated.ID, updated.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	closed, err := store.Get(updated.ID)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("status after CloseIfMatch = %q, want closed", closed.Status)
	}
	if closed.Revision == updated.Revision {
		t.Fatalf("revision after close = %d, want a fresh token", closed.Revision)
	}

	if err := writer.DeleteIfMatch(closed.ID, closed.Revision); err != nil {
		t.Fatalf("DeleteIfMatch: %v", err)
	}
	if _, err := store.Get(closed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
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

// TestNativeDoltStoreAtomicConditionalCloseAcrossIndependentHandles proves
// the atomic terminal-write fence against the actual OpenBestAvailable
// backend. The fast native fixture owns retry branch coverage; this test owns
// the database isolation and rollback boundary shared by independent handles.
func TestNativeDoltStoreAtomicConditionalCloseAcrossIndependentHandles(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".beads")
	openHandle := func(actor string) *NativeDoltStore {
		t.Helper()
		storage, err := beadslib.OpenBestAvailable(ctx, dir)
		if err != nil {
			t.Fatalf("open upstream native beads storage (%s): %v", actor, err)
		}
		t.Cleanup(func() {
			if err := storage.Close(); err != nil {
				t.Errorf("close upstream storage (%s): %v", actor, err)
			}
		})
		if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
			t.Fatalf("set issue prefix (%s): %v", actor, err)
		}
		return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
	}

	writerA := openHandle("atomic-close-A")
	writerB := openHandle("atomic-close-B")
	created, err := writerA.Create(Bead{Title: "real atomic conditional close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snapshot, err := writerA.Get(created.ID)
	if err != nil {
		t.Fatalf("writerA Get: %v", err)
	}
	if peer, err := writerB.Get(created.ID); err != nil || peer.Revision != snapshot.Revision {
		t.Fatalf("writerB snapshot = (%#v, %v), want revision %d", peer, err, snapshot.Revision)
	}

	type result struct {
		bead Bead
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, writer := range []*NativeDoltStore{writerA, writerB} {
		go func(store *NativeDoltStore) {
			<-start
			bead, err := store.CloseWithMetadataIfMatch(created.ID, snapshot.Revision, map[string]string{"winner": store.actor})
			results <- result{bead: bead, err: err}
		}(writer)
	}
	close(start)

	var winner Bead
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			if winner.ID != "" {
				t.Fatalf("multiple successful terminal writes: %#v and %#v", winner, result.bead)
			}
			winner = result.bead
		case IsPreconditionFailed(result.err):
			if result.bead.ID != "" {
				t.Fatalf("losing close returned %#v, want zero bead", result.bead)
			}
		default:
			t.Fatalf("contending close error = %v, want precondition failure", result.err)
		}
	}
	if winner.ID == "" || winner.Status != "closed" || winner.Metadata["winner"] == "" {
		t.Fatalf("winner = %#v, want exact closed row", winner)
	}

	staleCreated, err := writerA.Create(Bead{Title: "real atomic close stale rollback", Metadata: map[string]string{"before": "keep"}})
	if err != nil {
		t.Fatalf("Create stale bead: %v", err)
	}
	if err := writerB.SetMetadata(staleCreated.ID, "intervening", "write"); err != nil {
		t.Fatalf("intervening SetMetadata: %v", err)
	}
	before, err := writerA.Get(staleCreated.ID)
	if err != nil {
		t.Fatalf("Get before stale close: %v", err)
	}
	closed, err := writerA.CloseWithMetadataIfMatch(staleCreated.ID, staleCreated.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("stale CloseWithMetadataIfMatch error = %v, want precondition failure", err)
	}
	if closed.ID != "" {
		t.Fatalf("stale close returned %#v, want zero bead", closed)
	}
	after, err := writerB.Get(staleCreated.ID)
	if err != nil {
		t.Fatalf("Get after stale close: %v", err)
	}
	if after.Status != before.Status || after.Metadata["before"] != "keep" || after.Metadata["intervening"] != "write" || after.Metadata["state"] != "" {
		t.Fatalf("stale close mutated real row: before=%#v after=%#v", before, after)
	}
}

// TestNativeDoltStoreCloseWithMetadataIfMatchMidBatchFailureLeavesZeroRows
// proves the ONE-REQUEST atomicity CloseWithMetadataIfMatch's own doc comment
// claims ("ONE REQUEST, TWO ITEMS, ONE FENCE") against the REAL backend, the
// same way TestNativeDoltStoreApplyGraphPlanMidPlanFailureLeavesZeroRows pins
// it for ApplyGraphPlanWithStorage.
//
// This used to drive the failure through the CLOSE item (an unforced close
// refusing an issue with an open parent-child dependent,
// issueops.ErrCloseOpenChildren, after the update item had already landed).
// The G3 review's HIGH 1 fix gave the close item Force: true -- matching
// every other close this store issues, and restoring the old CloseIssueInTx
// path's policy-free behavior -- so that refusal is gone on purpose; see
// TestNativeDoltStoreCloseWithMetadataIfMatchForcesPastOpenChildren below for
// the proof that the fix works. Force also removes any second guard the
// close item could still fail on: CloseItem has no ExpectedVersion of its
// own, and UpdateItem's "already-touched" rule forbids this request's update
// item and close item from fencing the same row twice. So the only real
// refusal left reachable in this two-item, one-id shape is the UPDATE item's
// own ExpectedVersion, driven stale by a genuine intervening write from an
// independent handle against the same real backend -- proving a precondition
// miss on the FIRST item still refuses the WHOLE request and leaves the real
// row completely untouched, not just the metadata key the update item would
// have written.
func TestNativeDoltStoreCloseWithMetadataIfMatchMidBatchFailureLeavesZeroRows(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".beads")
	openHandle := func(actor string) *NativeDoltStore {
		t.Helper()
		storage, err := beadslib.OpenBestAvailable(ctx, dir)
		if err != nil {
			t.Fatalf("open upstream native beads storage (%s): %v", actor, err)
		}
		t.Cleanup(func() {
			if err := storage.Close(); err != nil {
				t.Errorf("close upstream storage (%s): %v", actor, err)
			}
		})
		if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
			t.Fatalf("set issue prefix (%s): %v", actor, err)
		}
		return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
	}

	writerA := openHandle("close-with-metadata-midfail-A")
	writerB := openHandle("close-with-metadata-midfail-B")

	created, err := writerA.Create(Bead{Title: "real mid-batch close", Metadata: map[string]string{"sibling": "before"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := writerA.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before stale close: %v", err)
	}

	// Independent handle, genuine write: bumps the row's real revision out
	// from under writerA's snapshot before writerA's close request builds
	// its update item's ExpectedVersion.
	if err := writerB.SetMetadata(created.ID, "intervening", "write"); err != nil {
		t.Fatalf("intervening SetMetadata: %v", err)
	}

	closed, err := writerA.CloseWithMetadataIfMatch(created.ID, before.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want precondition failure", err)
	}
	if closed.ID != "" {
		t.Fatalf("CloseWithMetadataIfMatch result = %#v, want zero bead on failure", closed)
	}

	after, err := writerB.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after refused close: %v", err)
	}
	if after.Status != before.Status || after.Metadata["sibling"] != "before" || after.Metadata["intervening"] != "write" || after.Metadata["state"] != "" {
		t.Fatalf("refused close left a partial mutation against the real backend:\n got: %#v\nwant status=%q sibling=before intervening=write state=\"\"", after, before.Status)
	}
}

// TestNativeDoltStoreCloseWithMetadataIfMatchForcesPastOpenChildren pins the
// G3 review's HIGH 1 fix: CloseWithMetadataIfMatch's close item now carries
// Force: true, so a parent with a real, open parent-child dependent (the
// exact row shape issueops.ErrCloseOpenChildren guards against) closes
// instead of refusing -- matching the old CloseIssueInTx path this method
// replaced, which was policy-free. The child itself must stay untouched:
// Force on the parent's close item bypasses the PARENT close's own policy,
// not a cascading close of its children.
func TestNativeDoltStoreCloseWithMetadataIfMatchForcesPastOpenChildren(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "close-with-metadata-open-children")

	parent, err := store.Create(Bead{Title: "real forced close parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := store.Create(Bead{Title: "real forced close child", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("child.ParentID = %q, want %q", child.ParentID, parent.ID)
	}

	closed, err := store.CloseWithMetadataIfMatch(parent.ID, parent.Revision, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch with an open child: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("closed = %#v, want status closed and metadata state=drained", closed)
	}

	fresh, err := store.Get(parent.ID)
	if err != nil {
		t.Fatalf("Get parent after forced close: %v", err)
	}
	if fresh.Status != "closed" {
		t.Fatalf("parent.Status = %q, want closed", fresh.Status)
	}

	childAfter, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after parent's forced close: %v", err)
	}
	if childAfter.Status == "closed" {
		t.Fatalf("child.Status = %q, want untouched by the parent's forced close", childAfter.Status)
	}
}

// TestNativeDoltStoreDeleteIfMatchRewritesNeighborTextThroughFacade pins the
// G3 MED-3 behavioral change, documented on DeleteIfMatch: routing the
// delete through issueops.Deleter now rewrites a surviving GRAPH NEIGHBOR's
// text that cites the deleted id to `[deleted:<id>]`, bumping the neighbor's
// own revision — something the raw tx.DeleteIssue this replaced never did.
// issueops.DeleteRequest has no option to suppress this (checked against the
// pinned beads v1.3.1 Deleter doc), so it is kept as a deliberate alignment
// with bd's own delete semantics rather than worked around.
func TestNativeDoltStoreDeleteIfMatchRewritesNeighborTextThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "delete-if-match-neighbor-rewrite")

	parent, err := store.Create(Bead{Title: "real delete rewrite parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := store.Create(Bead{
		Title:       "real delete rewrite child",
		ParentID:    parent.ID,
		Description: "see " + parent.ID + " for context",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("child.ParentID = %q, want %q", child.ParentID, parent.ID)
	}
	childBeforeRevision := child.Revision

	if err := store.DeleteIfMatch(parent.ID, parent.Revision); err != nil {
		t.Fatalf("DeleteIfMatch parent: %v", err)
	}

	if _, err := store.Get(parent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get parent after delete: err = %v, want ErrNotFound", err)
	}

	childAfter, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after parent's delete: %v", err)
	}
	wantDescription := "see [deleted:" + parent.ID + "] for context"
	if childAfter.Description != wantDescription {
		t.Fatalf("child.Description = %q, want %q (the role's neighbor-text rewrite)", childAfter.Description, wantDescription)
	}
	if childAfter.Revision == childBeforeRevision {
		t.Fatalf("child.Revision = %d, unchanged from before the delete; want it bumped by the rewrite", childAfter.Revision)
	}
}

// nativeTransferIfCurrentFixture creates a bead and, through UpdateIfMatch,
// stamps it in_progress and assigned to assignee -- the state TransferIfCurrent
// requires of ExpectedAssignee/ExpectedStatus to authorize a move.
func nativeTransferIfCurrentFixture(t *testing.T, store *NativeDoltStore, title, assignee string) Bead {
	t.Helper()
	bead, err := store.Create(Bead{Title: title})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.UpdateIfMatch(bead.ID, bead.Revision, UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
		t.Fatalf("UpdateIfMatch (fixture setup): %v", err)
	}
	fixture, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after fixture setup: %v", err)
	}
	if fixture.Assignee != assignee || fixture.Status != status {
		t.Fatalf("fixture = %+v, want Assignee=%q Status=%q", fixture, assignee, status)
	}
	return fixture
}

// TestNativeDoltStoreTransferIfCurrentMovesAnInProgressBeadThroughFacade pins
// the success path against real Dolt: an in_progress bead assigned to
// fromAssignee moves to toAssignee, and the move is NOT the ordinary
// anti-steal-fenced write -- ExpectedAssignee alone authorizes it (see the
// method's own doc).
func TestNativeDoltStoreTransferIfCurrentMovesAnInProgressBeadThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "transfer-if-current-success")
	bead := nativeTransferIfCurrentFixture(t, store, "transfer success", "claude-gcg-1")

	moved, err := store.TransferIfCurrent(bead.ID, "claude-gcg-1", "gcg-1")
	if err != nil || !moved {
		t.Fatalf("TransferIfCurrent = (%v, %v), want (true, nil)", moved, err)
	}

	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after transfer: %v", err)
	}
	if after.Assignee != "gcg-1" {
		t.Fatalf("after.Assignee = %q, want gcg-1", after.Assignee)
	}
	if after.Status != "in_progress" {
		t.Fatalf("after.Status = %q, want in_progress (unchanged)", after.Status)
	}
}

// TestNativeDoltStoreTransferIfCurrentSameAssigneeShortCircuitsThroughFacade
// pins the fromAssignee==toAssignee short circuit: it reports (true, nil)
// without ever reaching the backend, so it succeeds even for a bead this
// store has never heard of.
func TestNativeDoltStoreTransferIfCurrentSameAssigneeShortCircuitsThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "transfer-if-current-same")
	moved, err := store.TransferIfCurrent("gc-does-not-exist", "gcg-1", "gcg-1")
	if err != nil || !moved {
		t.Fatalf("TransferIfCurrent = (%v, %v), want (true, nil)", moved, err)
	}
}

// TestNativeDoltStoreTransferIfCurrentPreconditionMissThroughFacade pins the
// two readback outcomes of a lost precondition: a foreign holder reports
// (false, nil), and a bead already carrying toAssignee (a retried write whose
// first attempt already committed) reports (true, nil) -- both without error,
// matching BdStore.TransferIfCurrent's bd-exit-13 readback contract.
func TestNativeDoltStoreTransferIfCurrentPreconditionMissThroughFacade(t *testing.T) {
	for _, tc := range []struct {
		name, holder string
		want         bool
	}{
		{name: "foreign holder", holder: "someone-else", want: false},
		{name: "already transferred", holder: "gcg-1", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openRealNativeDoltStoreForCAS(t, "transfer-if-current-miss-"+tc.name)
			bead := nativeTransferIfCurrentFixture(t, store, "transfer precondition miss", tc.holder)

			moved, err := store.TransferIfCurrent(bead.ID, "claude-gcg-1", "gcg-1")
			if err != nil || moved != tc.want {
				t.Fatalf("TransferIfCurrent = (%v, %v), want (%v, nil)", moved, err, tc.want)
			}
		})
	}
}

// TestNativeDoltStoreTransferIfCurrentUnresolvableIDThroughFacade pins the
// not-found case: an id this store has never minted reports (false, nil),
// matching BdStore.TransferIfCurrent's isBdIssueNotFound branch.
func TestNativeDoltStoreTransferIfCurrentUnresolvableIDThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "transfer-if-current-not-found")
	moved, err := store.TransferIfCurrent("gc-does-not-exist", "claude-gcg-1", "gcg-1")
	if err != nil || moved {
		t.Fatalf("TransferIfCurrent = (%v, %v), want (false, nil)", moved, err)
	}
}
