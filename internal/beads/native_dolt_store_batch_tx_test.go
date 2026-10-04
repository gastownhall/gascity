package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// recordingBatchApplier captures the ApplyBatch request a Store.Tx built
// without applying it, so a test asserts the ITEMS the store composed rather
// than the state some double happened to reach.
type recordingBatchApplier struct {
	requests []issueops.ApplyBatchRequest
	result   issueops.ApplyBatchResult
	err      error
}

var _ issueops.BatchApplier = (*recordingBatchApplier)(nil)

func (a *recordingBatchApplier) ApplyBatch(_ context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	a.requests = append(a.requests, req)
	return a.result, a.err
}

// batchTxSpy is a storage double whose RunInTransaction refuses the way the
// http backend's does: a typed *beadslib.ErrUnsupported, returned WITHOUT
// invoking the callback.
type batchTxSpy struct {
	*nativeDoltStorageSpy
	applier    *recordingBatchApplier
	applierErr error
	txRan      bool
}

func newBatchTxSpy(issues ...*beadslib.Issue) *batchTxSpy {
	byID := make(map[string]*beadslib.Issue, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = issue
	}
	spy := &batchTxSpy{applier: &recordingBatchApplier{}}
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			issue, ok := byID[id]
			if !ok {
				return nil, nil
			}
			return cloneNativeIssueForTest(issue), nil
		},
		runInTransaction: func(_ context.Context, _ string, _ func(beadslib.Transaction) error) error {
			spy.txRan = true
			return &beadslib.ErrUnsupported{Op: "RunInTransaction", Backend: "http"}
		},
	}
	return spy
}

func (s *batchTxSpy) BatchApplier() (issueops.BatchApplier, error) {
	if s.applierErr != nil {
		return nil, s.applierErr
	}
	return s.applier, nil
}

func nativeIssueWithMetadata(t *testing.T, id string, metadata map[string]string) *beadslib.Issue {
	t.Helper()
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshaling test metadata: %v", err)
	}
	return &beadslib.Issue{ID: id, Title: id, Status: beadslib.StatusOpen, Metadata: raw}
}

func soleBatchRequest(t *testing.T, applier *recordingBatchApplier) issueops.ApplyBatchRequest {
	t.Helper()
	if len(applier.requests) != 1 {
		t.Fatalf("ApplyBatch called %d times, want exactly 1 — the whole callback must land as ONE request", len(applier.requests))
	}
	return applier.requests[0]
}

// TestTxFallsBackToOneBatchWhenTransactionsAreUnsupported is the seam this
// whole file exists for: the http backend refuses RunInTransaction, and the
// metadata-then-close callback every reconciler close site writes has to reach
// the server as ONE atomic request rather than as two writes or none.
func TestTxFallsBackToOneBatchWhenTransactionsAreUnsupported(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-1", map[string]string{"state": "active"}))
	store := newNativeDoltStoreForTest(spy)

	err := store.Tx("gc: close session gc-1", func(tx Tx) error {
		if err := tx.SetMetadataBatch("gc-1", map[string]string{
			"state":        "gc_swept",
			"close_reason": "session terminated: swept by gc",
		}); err != nil {
			return err
		}
		return tx.Close("gc-1")
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !spy.txRan {
		t.Fatal("the native transaction was never attempted; the batch path must be the FALLBACK, not the default")
	}

	req := soleBatchRequest(t, spy.applier)
	if req.Actor != "native-test" {
		t.Errorf("ApplyBatchRequest.Actor = %q, want the store actor", req.Actor)
	}
	if len(req.Items) != 2 {
		t.Fatalf("batch carried %d items, want the update+close pair: %+v", len(req.Items), req.Items)
	}
	if req.Items[0].Kind != issueops.ItemUpdate || req.Items[0].Update == nil {
		t.Fatalf("item 0 = %+v, want an update item", req.Items[0])
	}
	if got := req.Items[0].Update.Target.ID; got != "gc-1" {
		t.Errorf("update target = %q, want gc-1 by id", got)
	}
	if req.Items[0].Update.Target.Key != "" {
		t.Errorf("update target carries key %q; an existing row is named by id", req.Items[0].Update.Target.Key)
	}
	if raw, ok := req.Items[0].Update.Patch.Metadata.Set["close_reason"]; !ok {
		t.Errorf("update patch metadata = %+v, want the close_reason the callback staged", req.Items[0].Update.Patch.Metadata.Set)
	} else if string(raw) != `"session terminated: swept by gc"` {
		t.Errorf("close_reason patch = %s, want the JSON-encoded reason", raw)
	}
	if req.Items[1].Kind != issueops.ItemClose || req.Items[1].Close == nil {
		t.Fatalf("item 1 = %+v, want a close item", req.Items[1])
	}
	if got := req.Items[1].Close.Target.ID; got != "gc-1" {
		t.Errorf("close target = %q, want gc-1", got)
	}
	if !req.Items[1].Close.Force {
		t.Error("close item is not forced; the native path's tx.CloseIssue applied no close policy, and a molecule root routinely closes over open children")
	}
}

// TestBatchTxCloseCarriesTheReasonTheSameBatchStamped is the reason-preserving
// half. The native Tx route closes through tx.CloseIssue with the reason read
// from the row's metadata; a batch whose update item has not committed yet
// cannot read it back, so the reason must come from what THIS batch staged.
// Losing it would silently strip every close reason on the served backend.
func TestBatchTxCloseCarriesTheReasonTheSameBatchStamped(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-2", map[string]string{"close_reason": "stale reason from an earlier close"}))
	store := newNativeDoltStoreForTest(spy)

	if err := store.Tx("gc: close", func(tx Tx) error {
		if err := tx.SetMetadataBatch("gc-2", map[string]string{"close_reason": "session terminated: orphaned"}); err != nil {
			return err
		}
		return tx.Close("gc-2")
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	req := soleBatchRequest(t, spy.applier)
	item := req.Items[len(req.Items)-1].Close
	if item == nil {
		t.Fatalf("last item is not a close: %+v", req.Items[len(req.Items)-1])
	}
	if item.Reason != "session terminated: orphaned" {
		t.Errorf("close reason = %q, want the reason the same batch staged (not the row's stale one)", item.Reason)
	}
}

// TestBatchTxCloseFallsBackToTheStoredReason covers the site that closes
// without staging a reason first: sourceworkflow's already-marked root, and
// every CloseAll caller. The stored close_reason is what the native path
// reads, so the batch must read it too.
func TestBatchTxCloseFallsBackToTheStoredReason(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-3", map[string]string{"close_reason": "workflow finalized"}))
	store := newNativeDoltStoreForTest(spy)

	if err := store.Tx("gc: close", func(tx Tx) error { return tx.Close("gc-3") }); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	req := soleBatchRequest(t, spy.applier)
	if got := req.Items[0].Close.Reason; got != "workflow finalized" {
		t.Errorf("close reason = %q, want the stored close_reason", got)
	}
}

// TestBatchTxRefusesAClosedRowTheWayTheNativePathDoes pins the no-op: the
// native applyCloseInTx returns nil for an already-closed bead rather than
// writing, and the batch must not emit a close item for one either — the
// served closer refuses an already-closed row rather than answering it.
func TestBatchTxSkipsAnAlreadyClosedRow(t *testing.T) {
	closed := nativeIssueWithMetadata(t, "gc-4", map[string]string{})
	closed.Status = beadslib.StatusClosed
	spy := newBatchTxSpy(closed)
	store := newNativeDoltStoreForTest(spy)

	if err := store.Tx("gc: close", func(tx Tx) error { return tx.Close("gc-4") }); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("an empty batch was dialed anyway: %+v", spy.applier.requests)
	}
}

// TestBatchTxReportsAMissingBeadAsNotFound keeps applyCloseInTx's contract.
func TestBatchTxReportsAMissingBeadAsNotFound(t *testing.T) {
	spy := newBatchTxSpy()
	store := newNativeDoltStoreForTest(spy)

	err := store.Tx("gc: close", func(tx Tx) error { return tx.Close("gc-missing") })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Tx error = %v, want ErrNotFound", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a batch was dialed for a missing bead: %+v", spy.applier.requests)
	}
}

// TestBatchTxCreateNamesItsSeam. A create inside a Tx has to hand the callback
// a minted id BEFORE the batch runs — extmsg's bind reads it to key the
// transcript membership it creates in the same callback — and a batch that has
// not been dialed has no id to give. The refusal must say so by name rather
// than surface as a nil-bead panic three frames later.
func TestBatchTxCreateNamesItsSeam(t *testing.T) {
	spy := newBatchTxSpy()
	store := newNativeDoltStoreForTest(spy)

	err := store.Tx("gc: extmsg bind", func(tx Tx) error {
		_, err := tx.Create(Bead{Title: "binding", Type: "task"})
		return err
	})
	if err == nil {
		t.Fatal("a create inside a batch-applied Tx succeeded; it cannot mint an id before the batch is dialed")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("refusal %q does not name the unsupported verb", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a partial batch was dialed after the refusal: %+v", spy.applier.requests)
	}
}

// TestBatchTxReparentNamesItsSeam. UpdateItem.Patch.ParentID is refused by the
// http client's apply encoder (W-ApplyPatch.ParentID), so a reparent inside a
// Tx cannot cross the wire. gc must name that at the seam rather than let the
// encoder's refusal arrive as an opaque transport error.
func TestBatchTxReparentNamesItsSeam(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-5", map[string]string{}))
	store := newNativeDoltStoreForTest(spy)

	parent := "gc-parent"
	err := store.Tx("gc: reparent", func(tx Tx) error {
		return tx.Update("gc-5", UpdateOpts{ParentID: &parent})
	})
	if err == nil {
		t.Fatal("a reparent inside a batch-applied Tx succeeded; the apply patch publishes no parent_id")
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Errorf("refusal %q does not name the parent member it refused", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a partial batch was dialed after the refusal: %+v", spy.applier.requests)
	}
}

// TestBatchTxDialsNothingWhenTheCallbackFails proves the all-or-nothing edge
// the buffered shape buys: a callback that errors after staging writes must
// leave the server untouched, where the native path relied on a rollback.
func TestBatchTxDialsNothingWhenTheCallbackFails(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-6", map[string]string{}))
	store := newNativeDoltStoreForTest(spy)

	sentinel := errors.New("caller changed its mind")
	if err := store.Tx("gc: abort", func(tx Tx) error {
		if err := tx.SetMetadataBatch("gc-6", map[string]string{"state": "x"}); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want the callback's own error", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a batch was dialed after the callback failed: %+v", spy.applier.requests)
	}
}

// TestTxKeepsTheNativeTransactionWhenItIsSupported is the mutation guard on
// the routing arm: a store whose RunInTransaction WORKS must never be
// converted to the batch path, or every gc city on embedded Dolt silently
// loses the transaction rollback its callers document.
func TestTxKeepsTheNativeTransactionWhenItIsSupported(t *testing.T) {
	var ran bool
	spy := &batchTxSpy{applier: &recordingBatchApplier{}}
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		runInTransaction: func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
			ran = true
			return fn(&nativeDoltTransactionSpy{})
		},
	}
	store := newNativeDoltStoreForTest(spy)

	if err := store.Tx("gc: native", func(Tx) error { return nil }); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !ran {
		t.Fatal("the native RunInTransaction path was skipped")
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a batch was dialed on a store with working transactions: %+v", spy.applier.requests)
	}
}

// nativeDoltTransactionSpy is an inert beadslib.Transaction for the routing
// test above, which never calls through it.
type nativeDoltTransactionSpy struct{ beadslib.Transaction }

// THE ROUTING PREDICATE'S KILL COVERAGE.
//
// `err != nil && !entered && errors.As(err, &unsupported)` is the whole safety
// of the fallback, and each conjunct is load-bearing on its own. The two tests
// below exist because each one dies to a DIFFERENT deleted conjunct, and the
// rest of this file kills neither: every other case refuses before the
// callback runs, which is exactly the shape both mutations survive.

// !entered. A transaction that OPENED and then failed with an unsupported
// operation is a FAILED transaction, not an absent one — the callback already
// ran, and some of its writes may have been attempted inside the transaction
// the store is now being told about. Replaying that callback into a batch
// dials every one of its writes a second time. The refusal must surface.
func TestTxDoesNotReplayACallbackThatAlreadyRan(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-1", map[string]string{}))
	unsupported := &beadslib.ErrUnsupported{Op: "UpdateIssue", Backend: "http"}
	callbackRuns := 0
	spy.runInTransaction = func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
		spy.txRan = true
		// The callback runs — the transaction opened — and only then does an
		// operation inside it turn out to be unsupported.
		_ = fn(nativeDoltTransactionForTest{storage: spy.nativeDoltStorageSpy})
		return unsupported
	}
	store := newNativeDoltStoreForTest(spy)

	err := store.Tx("gc: half-applied", func(tx Tx) error {
		callbackRuns++
		return tx.SetMetadataBatch("gc-1", map[string]string{"state": "gc_swept"})
	})
	if !errors.Is(err, unsupported) {
		t.Fatalf("Tx error = %v, want the transaction's own refusal surfaced", err)
	}
	if callbackRuns != 1 {
		t.Fatalf("the callback ran %d times; a callback that already ran must never be replayed", callbackRuns)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("the callback was replayed into a batch after its transaction had already entered it: %+v", spy.applier.requests)
	}
}

// errors.As(*ErrUnsupported). A transaction that could not BEGIN — a busy
// server, a dropped connection, a lock timeout — refuses without entering the
// callback, exactly like an unsupported one does. Falling back on that would
// convert a transient failure into an unisolated batch: the caller asked for
// one atomic act and would silently get a different isolation level whenever
// the store was under load.
func TestTxDoesNotFallBackOnAFailureThatIsNotAnUnsupportedOperation(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-1", map[string]string{}))
	transient := errors.New("begin transaction: server busy")
	callbackRuns := 0
	spy.runInTransaction = func(_ context.Context, _ string, _ func(beadslib.Transaction) error) error {
		spy.txRan = true
		return transient
	}
	store := newNativeDoltStoreForTest(spy)

	err := store.Tx("gc: transient", func(tx Tx) error {
		callbackRuns++
		return tx.SetMetadataBatch("gc-1", map[string]string{"state": "gc_swept"})
	})
	if !errors.Is(err, transient) {
		t.Fatalf("Tx error = %v, want the transient failure verbatim", err)
	}
	if callbackRuns != 0 {
		t.Fatalf("the callback ran %d times against a transaction that never began", callbackRuns)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a transient BEGIN failure was answered with an unisolated batch: %+v", spy.applier.requests)
	}
}

// batchOnlyMemStorage is the mem double with its transaction taken away, so
// the SAME callback can be run down both routes against the same substrate.
type batchOnlyMemStorage struct{ *nativeDoltMemStorage }

func (s *batchOnlyMemStorage) RunInTransaction(context.Context, string, func(beadslib.Transaction) error) error {
	return &beadslib.ErrUnsupported{Op: "RunInTransaction", Backend: "http"}
}

func (s *batchOnlyMemStorage) BatchApplier() (issueops.BatchApplier, error) {
	return rawBatchApplier{storage: s}, nil
}

// THE DIFFERENTIAL. The two routes are two implementations of one contract, so
// the thing worth asserting is that a callback cannot TELL which one ran it.
// Field-by-field assertions on the item list prove the batch was composed;
// this proves it was composed into the same durable state the transaction
// would have left.
func TestBothTxRoutesLeaveTheSameState(t *testing.T) {
	run := func(t *testing.T, storage beadslib.Storage) Bead {
		t.Helper()
		store := newNativeDoltStoreForTest(storage)
		created, err := store.Create(Bead{Title: "differential", Metadata: map[string]string{"state": "active"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Tx("gc: close session "+created.ID, func(tx Tx) error {
			if err := tx.SetMetadataBatch(created.ID, map[string]string{
				"state":        "gc_swept",
				"close_reason": "session terminated: swept by gc",
			}); err != nil {
				return err
			}
			return tx.Close(created.ID)
		}); err != nil {
			t.Fatalf("Tx: %v", err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got
	}

	mem := newNativeDoltMemStorage()
	native := run(t, mem)
	batched := run(t, &batchOnlyMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()})

	if native.Status != batched.Status {
		t.Errorf("status differs by route: native %q, batched %q", native.Status, batched.Status)
	}
	for _, key := range []string{"state", "close_reason"} {
		if native.Metadata[key] != batched.Metadata[key] {
			t.Errorf("metadata[%q] differs by route: native %q, batched %q", key, native.Metadata[key], batched.Metadata[key])
		}
	}
	if batched.Metadata["close_reason"] != "session terminated: swept by gc" {
		t.Errorf("the batched route lost the staged close reason: %q", batched.Metadata["close_reason"])
	}
}
