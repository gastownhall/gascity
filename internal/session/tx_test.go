package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionTxSpy separates the ways a write can reach the store: inside the one
// transaction the front door opened, or directly around it. It counts
// in-transaction writes through its own beads.Tx wrapper rather than relying on
// the store's read-back, because an empty metadata batch is observationally
// identical to no batch at all once it lands — and "issued no write" is exactly
// what one of these tests is about.
type sessionTxSpy struct {
	*beads.MemStore
	txCalls      int
	txMessages   []string
	txBatch      int
	txClose      int
	directBatch  int
	directClose  int
	directUpdate int
}

func newSessionTxSpy() *sessionTxSpy {
	return &sessionTxSpy{MemStore: beads.NewMemStore()}
}

func (s *sessionTxSpy) Tx(message string, fn func(beads.Tx) error) error {
	s.txCalls++
	s.txMessages = append(s.txMessages, message)
	if fn == nil {
		return s.MemStore.Tx(message, nil)
	}
	return s.MemStore.Tx(message, func(tx beads.Tx) error {
		return fn(&countingBeadsTx{Tx: tx, spy: s})
	})
}

// countingBeadsTx records every write the front door issues inside the
// transaction, then delegates to the real handle.
type countingBeadsTx struct {
	beads.Tx
	spy *sessionTxSpy
}

func (t *countingBeadsTx) SetMetadataBatch(id string, kvs map[string]string) error {
	t.spy.txBatch++
	return t.Tx.SetMetadataBatch(id, kvs)
}

func (t *countingBeadsTx) Close(id string) error {
	t.spy.txClose++
	return t.Tx.Close(id)
}

func (s *sessionTxSpy) SetMetadataBatch(id string, kvs map[string]string) error {
	s.directBatch++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *sessionTxSpy) Close(id string) error {
	s.directClose++
	return s.MemStore.Close(id)
}

func (s *sessionTxSpy) Update(id string, opts beads.UpdateOpts) error {
	s.directUpdate++
	return s.MemStore.Update(id, opts)
}

func newSpiedSessionStore(t *testing.T) (*Store, *sessionTxSpy) {
	t.Helper()
	spy := newSessionTxSpy()
	return NewStore(beads.SessionStore{Store: spy}), spy
}

func seedSessionBead(t *testing.T, spy *sessionTxSpy, metadata map[string]string) beads.Bead {
	t.Helper()
	created, err := spy.Create(beads.Bead{
		Title:    "spied",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Status:   "open",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("seeding the session bead: %v", err)
	}
	return created
}

// TestStoreTxRunsEveryWriteInsideOneStoreTransaction pins the whole reason the
// sessions contract grew a transaction: a caller that needs several session
// writes to commit together must be able to express that through the front
// door. A Tx that ran its callback's writes as ordinary front-door calls would
// still pass every read-back assertion, so this counts HOW the writes arrived.
func TestStoreTxRunsEveryWriteInsideOneStoreTransaction(t *testing.T) {
	front, spy := newSpiedSessionStore(t)
	bead := seedSessionBead(t, spy, map[string]string{"state": "creating", "last_woke_at": "then"})

	if err := front.Tx("gc: spied session write", func(tx Tx) error {
		if err := tx.ApplyPatch(bead.ID, MetadataPatch{"last_woke_at": ""}); err != nil {
			return err
		}
		return tx.CloseWithoutReason(bead.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if spy.txCalls != 1 {
		t.Fatalf("the front door opened %d store transactions, want exactly 1", spy.txCalls)
	}
	if len(spy.txMessages) != 1 || spy.txMessages[0] != "gc: spied session write" {
		t.Fatalf("store transaction messages = %v, want the caller's message verbatim", spy.txMessages)
	}
	if spy.directBatch != 0 || spy.directClose != 0 || spy.directUpdate != 0 {
		t.Fatalf("writes escaped the transaction (batch=%d close=%d update=%d); a Tx whose writes land directly is a pass-through wearing a transaction's name",
			spy.directBatch, spy.directClose, spy.directUpdate)
	}
	if spy.txBatch != 1 || spy.txClose != 1 {
		t.Fatalf("the transaction carried batch=%d close=%d writes, want exactly one of each", spy.txBatch, spy.txClose)
	}

	got, err := spy.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading the bead back: %v", err)
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at = %q after a committed transaction, want cleared", got.Metadata["last_woke_at"])
	}
	if got.Status != "closed" {
		t.Errorf("Status = %q after a committed transaction, want closed", got.Status)
	}
}

// TestStoreTxReturnsTheCallbackFailureVerbatim pins the error identity callers
// branch on. rollbackPendingCreateClears distinguishes "the transaction failed"
// from "the bead was already closed" by the error it gets back, so a front door
// that wrapped or replaced the callback's error would silently reclassify every
// such decision.
func TestStoreTxReturnsTheCallbackFailureVerbatim(t *testing.T) {
	front, spy := newSpiedSessionStore(t)
	bead := seedSessionBead(t, spy, map[string]string{"state": "creating"})
	boom := errors.New("session tx callback failed")

	err := front.Tx("gc: failing session write", func(tx Tx) error {
		if patchErr := tx.ApplyPatch(bead.ID, MetadataPatch{"state": "failed-create"}); patchErr != nil {
			return patchErr
		}
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want the callback's own failure", err)
	}
	if spy.txCalls != 1 {
		t.Fatalf("the front door opened %d store transactions, want exactly 1", spy.txCalls)
	}
}

// TestStoreTxAppliesAnEmptyPatchWithoutWriting pins that the transactional
// ApplyPatch means what the non-transactional one means. Store.ApplyPatch
// treats an empty patch as a no-op; a transactional peer that issued the write
// anyway would give the same NAME two behaviors, and on a backend that
// decomposes SetMetadataBatch per key an empty write is not free.
func TestStoreTxAppliesAnEmptyPatchWithoutWriting(t *testing.T) {
	front, spy := newSpiedSessionStore(t)
	bead := seedSessionBead(t, spy, map[string]string{"state": "creating"})

	if err := front.Tx("gc: empty session patch", func(tx Tx) error {
		return tx.ApplyPatch(bead.ID, MetadataPatch{})
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if spy.txBatch != 0 {
		t.Fatalf("an empty patch issued %d metadata batch write(s) inside the transaction, want 0; Tx.ApplyPatch must be the no-op Store.ApplyPatch is", spy.txBatch)
	}
}

// TestStoreTxRejectsANilCallback pins that a nil callback reaches the store's
// own nil-callback contract rather than being silently treated as an empty
// transaction that "succeeded".
func TestStoreTxRejectsANilCallback(t *testing.T) {
	front, spy := newSpiedSessionStore(t)
	if err := front.Tx("gc: nil callback", nil); err == nil {
		t.Fatal("Tx(nil) reported success; a transaction with no callback committed nothing and must say so")
	}
	if spy.txCalls != 1 {
		t.Fatalf("the front door opened %d store transactions for a nil callback, want exactly 1 (the store owns the contract)", spy.txCalls)
	}
}
