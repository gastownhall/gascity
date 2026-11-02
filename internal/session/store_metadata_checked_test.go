package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// checkedAsk is a casOnly predicate that counts its asks and, on its first,
// runs mutate: a write that lands while the (possibly slow) mode query runs.
type checkedAsk struct {
	want   bool
	asks   int
	mutate func()
}

func (a *checkedAsk) ask() bool {
	a.asks++
	if a.asks == 1 && a.mutate != nil {
		a.mutate()
	}
	return a.want
}

func seedCheckedSession(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// TestUpdateMetadataChecked pins the checked write: a store with a conditional
// writer writes by CAS and never asks casOnly; a store with none asks casOnly
// once, before the deciding read, and writes unfenced only on an explicit
// false; nil or true refuse with ErrConditionalWriteUnsupported and write
// nothing; an incapable require-mode store refuses with the require error
// without asking. Kills asking the mode on a capable store, a nil predicate
// read as legacy, a deciding read taken before the ask, and a refusal that
// still writes.
func TestUpdateMetadataChecked(t *testing.T) {
	cases := []struct {
		name      string
		store     func(t *testing.T, mem *beads.MemStore) beads.Store
		incapable bool
		casOnly   *bool // nil passes a nil predicate
		wantErr   error
		require   bool
		wantWrite bool
		wantAsks  int
		wantCAS   bool
	}{
		{name: "unstamped store, legacy", store: func(_ *testing.T, mem *beads.MemStore) beads.Store { return mem }, casOnly: ptrBool(false), wantWrite: true, wantAsks: 1},
		{name: "conditional writes off, legacy", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Off, mem) }, casOnly: ptrBool(false), wantWrite: true, wantAsks: 1},
		{name: "auto over an incapable store, legacy", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Auto, mem) }, incapable: true, casOnly: ptrBool(false), wantWrite: true, wantAsks: 1},
		{name: "auto over an incapable store, CAS only", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Auto, mem) }, incapable: true, casOnly: ptrBool(true), wantErr: beads.ErrConditionalWriteUnsupported, wantAsks: 1},
		{name: "auto over an incapable store, no predicate", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Auto, mem) }, incapable: true, wantErr: beads.ErrConditionalWriteUnsupported},
		{name: "require over an incapable store", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Require, mem) }, incapable: true, casOnly: ptrBool(false), require: true},
		{name: "capable store, CAS only", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Auto, mem) }, casOnly: ptrBool(true), wantWrite: true, wantCAS: true},
		{name: "capable store, no predicate", store: func(t *testing.T, mem *beads.MemStore) beads.Store { return stampedMemStore(t, gate.Auto, mem) }, wantWrite: true, wantCAS: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			store := tc.store(t, mem)
			created := seedCheckedSession(t, store)
			mem.DisableConditionalWrites = tc.incapable
			rec := &rowWriteRecorder{Store: store}
			var casOnly func() bool
			ask := &checkedAsk{mutate: func() {
				if err := mem.SetMetadata(created.ID, "test_seen", "during-ask"); err != nil {
					t.Fatalf("mutate during the ask: %v", err)
				}
			}}
			if tc.casOnly != nil {
				ask.want = *tc.casOnly
				casOnly = ask.ask
			}
			decisions := 0
			var seen string
			wrote, err := NewStore(beads.SessionStore{Store: rec}).UpdateMetadataChecked(created.ID, 3, func(_ Info, resp PersistedResponse) MetadataPatch {
				decisions++
				seen = resp.Metadata["test_seen"]
				return MetadataPatch{"test_checked": "yes"}
			}, casOnly)
			switch {
			case tc.require:
				if !beads.IsConditionalWritesRequired(err) {
					t.Fatalf("err = %v, want the require refusal", err)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("err = %v, want nil", err)
			}
			got, gerr := mem.Get(created.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if wrote != tc.wantWrite || (got.Metadata["test_checked"] == "yes") != tc.wantWrite {
				t.Fatalf("wrote = %v, row %v; want written=%v", wrote, got.Metadata, tc.wantWrite)
			}
			if !tc.wantWrite && decisions != 0 {
				t.Fatalf("decide ran %d times on a refused write, want 0", decisions)
			}
			if ask.asks != tc.wantAsks {
				t.Fatalf("casOnly asked %d times, want %d", ask.asks, tc.wantAsks)
			}
			if tc.wantAsks > 0 && tc.wantWrite && seen != "during-ask" {
				t.Fatalf("decide saw test_seen=%q; want the deciding read after the ask", seen)
			}
			if usedCAS := len(rec.ifMatch) > 0; tc.wantWrite && usedCAS != tc.wantCAS {
				t.Fatalf("writes %v; want CAS=%v", rec.writes, tc.wantCAS)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// TestUpdateMetadataCheckedEmptyPatchWritesNothing: a decide that returns no
// patch writes nothing on either path, and is not an error.
func TestUpdateMetadataCheckedEmptyPatchWritesNothing(t *testing.T) {
	for name, incapable := range map[string]bool{"capable": false, "legacy fallback": true} {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStore()
			store := stampedMemStore(t, gate.Auto, mem)
			created := seedCheckedSession(t, store)
			mem.DisableConditionalWrites = incapable
			rec := &rowWriteRecorder{Store: store}
			wrote, err := NewStore(beads.SessionStore{Store: rec}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch { return nil }, func() bool { return false })
			if wrote || err != nil || len(rec.writes) != 0 {
				t.Fatalf("empty patch: wrote %v, err %v, writes %v; want nothing", wrote, err, rec.writes)
			}
		})
	}
}

// TestUpdateMetadataCheckedRedecidesAndBoundsContention: a capable store's
// lost CAS re-reads and re-decides, up to attempts times, after which nothing
// is written and no error is returned; casOnly is never asked. Kills a blind
// retry after a lost CAS and an unbounded loop.
func TestUpdateMetadataCheckedRedecidesAndBoundsContention(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedCheckedSession(t, stamped)
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	decisions := 0
	wrote, err := NewStore(beads.SessionStore{Store: loser}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
		decisions++
		return MetadataPatch{"test_checked": "yes"}
	}, func() bool { t.Fatal("casOnly asked on a capable store"); return true })
	if wrote || err != nil || decisions != 3 {
		t.Fatalf("always-lost CAS: wrote %v, err %v, decisions %d; want not written, nil, 3 decisions", wrote, err, decisions)
	}
	if loser.writes[len(loser.writes)-1] != "UpdateIfMatch" || len(loser.ifMatch) != 3 {
		t.Fatalf("writes %v; want three CAS attempts and no blind write", loser.writes)
	}
}

// failingRowStore fails reads or writes of the session row.
type failingRowStore struct {
	beads.Store
	getErr, updateErr error
}

func (s failingRowStore) Get(id string) (beads.Bead, error) {
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func (s failingRowStore) Update(id string, opts beads.UpdateOpts) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(id, opts)
}

// TestUpdateMetadataCheckedPropagatesErrors: a failed read and a failed
// fallback write are returned, and a failed write spends the mint of the
// disposition patch it carried. Kills swallowing either error and a leaked
// mint.
func TestUpdateMetadataCheckedPropagatesErrors(t *testing.T) {
	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive), "wait_hold": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := func() bool { return false }
	readErr := errors.New("store down")
	if _, err := NewStore(beads.SessionStore{Store: failingRowStore{Store: mem, getErr: readErr}}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
		t.Fatal("decide ran without a read")
		return nil
	}, legacy); !errors.Is(err, readErr) {
		t.Fatalf("read failure: err = %v, want %v", err, readErr)
	}

	writeErr := errors.New("write refused")
	before := OpenDispositionMints(created.ID)
	wrote, err := NewStore(beads.SessionStore{Store: failingRowStore{Store: mem, updateErr: writeErr}}).UpdateMetadataChecked(created.ID, 3, func(info Info, _ PersistedResponse) MetadataPatch {
		to, err := DispositionOfInfo(info).Apply(ActorController, DispEvent{Kind: EvWaitEnd}, State(info.MetadataState), dispT0)
		if err != nil {
			t.Fatal(err)
		}
		return to.Changed().Into(info.ID, nil)
	}, legacy)
	if wrote || !errors.Is(err, writeErr) {
		t.Fatalf("write failure: wrote %v, err %v; want %v", wrote, err, writeErr)
	}
	if n := OpenDispositionMints(created.ID) - before; n != 0 {
		t.Errorf("the failed write left %d open mints", n)
	}
}
