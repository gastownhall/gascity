package opjournal

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
)

func turnEndedFact() Fact {
	return Fact{
		Kind: FactTurnEnded, Conversation: "gc-1", Generation: 7, Turn: "run-9:2",
		Reason: string(TurnEndCompleted), Basis: FactBasisRuntime, Detail: "end_turn", At: t0,
	}
}

func TestRecordFactCreatesThenFindsTheOccurrence(t *testing.T) {
	h := newHarness(t)
	f := turnEndedFact()
	created, err := h.store.RecordFact(f)
	if err != nil || !created {
		t.Fatalf("first RecordFact = created %v, %v", created, err)
	}
	id, _ := FactID("gc", f)
	b, err := h.mem.Get(id)
	if err != nil {
		t.Fatalf("fact row %s: %v", id, err)
	}
	got, err := DecodeFact(b)
	if err != nil || got.Detail != "end_turn" || got.Basis != FactBasisRuntime || b.Status != "closed" {
		t.Fatalf("stored fact %+v, %v", got, err)
	}
	creates := h.countOps(beadstest.OpCreate)
	// A second writer of the same occurrence, with another body, finds the
	// first one: facts are immutable and the first writer decides the body.
	again := f
	again.Reason, again.Basis, again.Detail = string(TurnEndEnded), FactBasisObserved, ""
	again.At = t0.Add(1)
	created, err = h.store.RecordFact(again)
	if err != nil || created {
		t.Fatalf("second RecordFact = created %v, %v", created, err)
	}
	if n := h.countOps(beadstest.OpCreate); n != creates {
		t.Fatalf("a recorded occurrence was created again (%d creates)", n-creates)
	}
	if b2, _ := h.mem.Get(id); b2.Metadata[keyFactReason] != string(TurnEndCompleted) {
		t.Fatalf("the second writer changed the stored fact: %v", b2.Metadata)
	}
}

func TestRecordFactRefusesAnInvalidFactWithoutWriting(t *testing.T) {
	h := newHarness(t)
	f := turnEndedFact()
	f.Turn = ""
	if _, err := h.store.RecordFact(f); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("RecordFact(no turn) = %v, want ErrInvalidRequest", err)
	}
	if n := h.countOps(beadstest.OpCreate) + h.countOps(beadstest.OpGet); n != 0 {
		t.Fatalf("a refused fact reached the store (%d calls)", n)
	}
}

// refusingCreateStore refuses every create as a duplicate, as a store does
// when a concurrent writer won the pinned id between the read and the create.
type refusingCreateStore struct {
	*beads.MemStore
	stored beads.Bead
}

func (s *refusingCreateStore) Create(beads.Bead) (beads.Bead, error) {
	if s.stored.ID != "" {
		if _, err := s.MemStore.Create(s.stored); err != nil {
			return beads.Bead{}, err
		}
	}
	return beads.Bead{}, errors.New("duplicate primary key")
}

func TestRecordFactResolvesARefusedCreate(t *testing.T) {
	f := turnEndedFact()
	stored, err := EncodeFact("gc", f)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("matching row", func(t *testing.T) {
		mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
		h := newHarnessOver(t, mem, &refusingCreateStore{MemStore: mem, stored: stored})
		if created, err := h.store.RecordFact(f); err != nil || created {
			t.Fatalf("RecordFact after a lost create = created %v, %v; want false, nil", created, err)
		}
	})
	t.Run("no row", func(t *testing.T) {
		mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
		h := newHarnessOver(t, mem, &refusingCreateStore{MemStore: mem})
		if _, err := h.store.RecordFact(f); err == nil || !strings.Contains(err.Error(), "duplicate primary key") {
			t.Fatalf("RecordFact with a refused create and no row = %v, want the create error", err)
		}
	})
	t.Run("another occurrence under the id", func(t *testing.T) {
		other := stored
		other.Metadata = map[string]string{}
		for k, v := range stored.Metadata {
			other.Metadata[k] = v
		}
		other.Metadata[keyFactTurn] = "run-9:3"
		mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
		h := newHarnessOver(t, mem, &refusingCreateStore{MemStore: mem, stored: other})
		if _, err := h.store.RecordFact(f); !errors.Is(err, ErrCorruptOp) {
			t.Fatalf("RecordFact over another occurrence = %v, want ErrCorruptOp", err)
		}
	})
}

func TestRecordFactUnregisteredTypeAndPinnedID(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, typeRefusingStore{mem})
	if _, err := h.store.RecordFact(turnEndedFact()); !errors.Is(err, ErrBeadTypeUnregistered) {
		t.Fatalf("RecordFact on a store without the op type = %v", err)
	}
	minting := &beads.MemStore{IDPrefix: "gc"}
	h = newHarnessOver(t, minting, minting)
	if _, err := h.store.RecordFact(turnEndedFact()); !errors.Is(err, ErrPinnedIDNotHonored) {
		t.Fatalf("RecordFact on a minting store = %v, want ErrPinnedIDNotHonored", err)
	}
	if all, _ := minting.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); len(all) != 0 {
		t.Fatalf("stray row left behind: %+v", all)
	}
}

// A store that overwrites a duplicate pinned id is never asked to: the read
// comes first.
func TestRecordFactRetryNeverRecreates(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, upsertingStore{mem})
	if created, err := h.store.RecordFact(turnEndedFact()); err != nil || !created {
		t.Fatalf("RecordFact = %v, %v", created, err)
	}
	creates := h.countOps(beadstest.OpCreate)
	if created, err := h.store.RecordFact(turnEndedFact()); err != nil || created {
		t.Fatalf("retried RecordFact = %v, %v", created, err)
	}
	if n := h.countOps(beadstest.OpCreate); n != creates {
		t.Fatalf("a retried RecordFact issued %d creates", n-creates)
	}
}

func openSQLiteOpStore(t *testing.T) (*Store, beads.Store) {
	t.Helper()
	sq, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sq.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	s, err := NewStore(beads.SessionStore{Store: sq}, &clock.Fake{Time: t0})
	if err != nil {
		t.Fatalf("NewStore(sqlite): %v", err)
	}
	return s, sq
}

func TestRecordFactConcurrentWriters(t *testing.T) {
	stores := map[string]func(t *testing.T) (*Store, beads.Store){
		"memstore": func(t *testing.T) (*Store, beads.Store) {
			h := newHarness(t)
			return h.store, h.mem
		},
		"sqlite": openSQLiteOpStore,
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			s, backing := open(t)
			const n = 32
			var wg sync.WaitGroup
			var createdCount atomic.Int32
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					f := turnEndedFact()
					// Writers disagree on the body; only the occurrence keys it.
					f.Detail = fmt.Sprintf("writer_%d", i)
					created, err := s.RecordFact(f)
					errs[i] = err
					if created {
						createdCount.Add(1)
					}
				}()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("writer %d: %v", i, err)
				}
			}
			if c := createdCount.Load(); c != 1 {
				t.Fatalf("%d writers created the fact, want exactly 1", c)
			}
			id, _ := FactID(s.Prefix(), turnEndedFact())
			b, err := backing.Get(id)
			if err != nil {
				t.Fatalf("fact row: %v", err)
			}
			if _, err := DecodeFact(b); err != nil || b.Status != "closed" {
				t.Fatalf("stored fact (status %s): %v", b.Status, err)
			}
			all, err := backing.List(beads.ListQuery{Label: FactConversationLabel("gc-1"), IncludeClosed: true})
			if err != nil || len(all) != 1 {
				t.Fatalf("%d fact rows, %v; want 1", len(all), err)
			}
		})
	}
}

// A stored fact whose basis a newer build wrote is still the same
// occurrence: RecordFact reports it recorded rather than failing.
func TestRecordFactFindsAnOccurrenceWithANewerBasis(t *testing.T) {
	h := newHarness(t)
	f := turnEndedFact()
	b, err := EncodeFact("gc", f)
	if err != nil {
		t.Fatal(err)
	}
	b.Metadata[keyFactBasis] = "replayed"
	if _, err := h.mem.Create(b); err != nil {
		t.Fatal(err)
	}
	created, err := h.store.RecordFact(f)
	if err != nil || created {
		t.Fatalf("RecordFact over a newer basis = created %v, %v", created, err)
	}
}

// closeFailingStore creates rows open, as MemStore and BdStore do, and fails
// every conditional update.
type closeFailingStore struct{ *beads.MemStore }

func (s closeFailingStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return errors.New("disk full")
}

// A close that fails after the create still reports the fact created, with
// an error that names the fact.
func TestRecordFactReportsAFailedCloseAfterTheCreate(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, closeFailingStore{mem})
	created, err := h.store.RecordFact(turnEndedFact())
	if !created || err == nil {
		t.Fatalf("RecordFact = created %v, %v; want true and the close error", created, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "closing fact") || strings.Contains(msg, " op ") {
		t.Fatalf("error %q does not name the fact", msg)
	}
}
