package session

import (
	"errors"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// checkedModeQuery is a casOnly that answers casOnly and counts its asks.
type checkedModeQuery struct {
	casOnly bool
	asks    int
	before  func() // runs inside the ask, before it answers
}

func (q *checkedModeQuery) ask() bool {
	q.asks++
	if q.before != nil {
		q.before()
	}
	return q.casOnly
}

// noWriterStores are session stores that resolve no conditional writer and do
// not refuse for it: unstamped, stamped off, and auto degraded on an
// incapable store.
func noWriterStores() []struct {
	name string
	open func(t *testing.T) beads.Store
} {
	stamped := func(mode gate.Mode) func(t *testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			mem := beads.NewMemStore()
			store := stampedMemStore(t, mode, mem)
			mem.DisableConditionalWrites = true
			return store
		}
	}
	return []struct {
		name string
		open func(t *testing.T) beads.Store
	}{
		{"unstamped", func(*testing.T) beads.Store { return beads.NewMemStore() }},
		{"off", stamped(gate.Off)},
		{"auto-degraded", stamped(gate.Auto)},
	}
}

// TestUpdateMetadataCheckedNoWriterWritesOnlyWhenCASOnlyAllows proves the
// checked write without a conditional writer: casOnly false writes the patch
// decided from a fresh read, once; casOnly true and a nil casOnly refuse with
// beads.ErrConditionalWriteUnsupported, run no decide and write nothing.
// Kills a fallback that ignores casOnly, a nil casOnly read as legacy, and a
// refusal that decides (or writes) first.
func TestUpdateMetadataCheckedNoWriterWritesOnlyWhenCASOnlyAllows(t *testing.T) {
	for _, backend := range noWriterStores() {
		t.Run(backend.name, func(t *testing.T) {
			t.Run("legacy", func(t *testing.T) {
				store := backend.open(t)
				created := seedRowFenceSession(t, store, "s-legacy")
				q := &checkedModeQuery{casOnly: false}
				decides := 0
				written, err := NewStore(beads.SessionStore{Store: store}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
					decides++
					return MetadataPatch{"alias": "checked"}
				}, q.ask)
				if err != nil || !written || decides != 1 || q.asks != 1 {
					t.Fatalf("UpdateMetadataChecked = (%v, %v), decides %d, asks %d; want one decided write after one ask", written, err, decides, q.asks)
				}
				got, err := store.Get(created.ID)
				if err != nil || got.Metadata["alias"] != "checked" {
					t.Fatalf("row after the checked write: %v, %v; want alias checked", got.Metadata, err)
				}
			})
			for name, casOnly := range map[string]func() bool{
				"cas-only": (&checkedModeQuery{casOnly: true}).ask,
				"nil":      nil,
			} {
				t.Run(name, func(t *testing.T) {
					store := backend.open(t)
					created := seedRowFenceSession(t, store, "s-refused")
					written, err := NewStore(beads.SessionStore{Store: store}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
						t.Fatal("decide ran for a refused checked write")
						return nil
					}, casOnly)
					if written || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
						t.Fatalf("UpdateMetadataChecked = (%v, %v), want the unsupported refusal", written, err)
					}
					assertRowUntouched(t, store, created.ID)
				})
			}
		})
	}
}

// TestUpdateMetadataCheckedRequireRefuses proves a require-mode store that
// cannot fence returns its refusal without asking casOnly or deciding: a
// legacy casOnly does not open a blind write there.
func TestUpdateMetadataCheckedRequireRefuses(t *testing.T) {
	mem := beads.NewMemStore()
	store := stampedMemStore(t, gate.Require, mem)
	mem.DisableConditionalWrites = true
	created := seedRowFenceSession(t, store, "s-require")
	q := &checkedModeQuery{casOnly: false}
	written, err := NewStore(beads.SessionStore{Store: store}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
		t.Fatal("decide ran on a require-mode store that cannot fence")
		return nil
	}, q.ask)
	if written || !beads.IsConditionalWritesRequired(err) || q.asks != 0 {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), asks %d; want the require refusal and no ask", written, err, q.asks)
	}
	assertRowUntouched(t, store, created.ID)
}

// TestUpdateMetadataCheckedCASNeverAsksTheMode proves a store with a
// conditional writer writes by UpdateIfMatch alone, never asks casOnly, and
// re-decides on the fresh row after a writer lands between its read and its
// CAS. Kills a checked write that queries the mode (or writes blind) on a
// capable store, and one that replays a stale patch.
func TestUpdateMetadataCheckedCASNeverAsksTheMode(t *testing.T) {
	mem := beads.NewMemStore()
	store := stampedMemStore(t, gate.Auto, mem)
	created := seedRowFenceSession(t, store, "s-cas")
	rec := &rowWriteRecorder{Store: store}
	q := &checkedModeQuery{casOnly: false}
	var seen []string
	written, err := NewStore(beads.SessionStore{Store: rec}).UpdateMetadataChecked(created.ID, 3, func(current Info, _ PersistedResponse) MetadataPatch {
		seen = append(seen, current.Alias)
		if len(seen) == 1 {
			if err := mem.SetMetadataBatch(created.ID, map[string]string{"alias": "operator-pick"}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
		}
		return MetadataPatch{"pool_slot": current.Alias}
	}, q.ask)
	if err != nil || !written || q.asks != 0 {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), asks %d; want a CAS write and no mode query", written, err, q.asks)
	}
	if !slices.Equal(seen, []string{"", "operator-pick"}) {
		t.Fatalf("decide saw aliases %q, want a second decision on the fresh row", seen)
	}
	if !slices.Equal(rec.writes, []string{"UpdateIfMatch", "UpdateIfMatch"}) {
		t.Fatalf("writes = %v, want two UpdateIfMatch and nothing blind", rec.writes)
	}
	got, err := mem.Get(created.ID)
	if err != nil || got.Metadata["pool_slot"] != "operator-pick" {
		t.Fatalf("row after the re-decide: %v, %v; want pool_slot decided from the fresh read", got.Metadata, err)
	}

	// Contention past every attempt writes nothing and is no error.
	attempts := 0
	written, err = NewStore(beads.SessionStore{Store: store}).UpdateMetadataChecked(created.ID, 2, func(Info, PersistedResponse) MetadataPatch {
		attempts++
		if err := mem.SetMetadataBatch(created.ID, map[string]string{"alias": "moved"}); err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
		return MetadataPatch{"pool_slot": "never"}
	}, q.ask)
	if err != nil || written || attempts != 2 || q.asks != 0 {
		t.Fatalf("contended UpdateMetadataChecked = (%v, %v), attempts %d, asks %d; want nothing written after 2 attempts", written, err, attempts, q.asks)
	}
}

// TestUpdateMetadataCheckedDecidesAfterTheModeQuery proves casOnly is asked
// before the deciding read, never between it and the write: a row write that
// lands while the mode is asked is what decide reads. Kills deciding first
// and asking the (slow) mode inside the read-to-write window.
func TestUpdateMetadataCheckedDecidesAfterTheModeQuery(t *testing.T) {
	mem := beads.NewMemStore()
	created := seedRowFenceSession(t, mem, "s-slow-mode")
	q := &checkedModeQuery{casOnly: false, before: func() {
		if err := mem.SetMetadataBatch(created.ID, map[string]string{"alias": "landed-during-the-query"}); err != nil {
			t.Fatalf("write during the mode query: %v", err)
		}
	}}
	var seen string
	written, err := NewStore(beads.SessionStore{Store: mem}).UpdateMetadataChecked(created.ID, 3, func(current Info, _ PersistedResponse) MetadataPatch {
		seen = current.Alias
		return MetadataPatch{"pool_slot": "3"}
	}, q.ask)
	if err != nil || !written || seen != "landed-during-the-query" {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), decide saw alias %q; want the decision on the read after the query", written, err, seen)
	}
}

// TestUpdateMetadataCheckedEmptyPatchAndErrors proves an empty patch writes
// nothing on either path, and a read or write failure is returned.
func TestUpdateMetadataCheckedEmptyPatchAndErrors(t *testing.T) {
	legacy := func() bool { return false }
	empty := func(Info, PersistedResponse) MetadataPatch { return nil }

	capable := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	unfenced := beads.NewMemStore()
	for name, store := range map[string]beads.Store{"cas": capable, "checked": unfenced} {
		created := seedRowFenceSession(t, store, "s-empty-"+name)
		rec := &rowWriteRecorder{Store: store}
		written, err := NewStore(beads.SessionStore{Store: rec}).UpdateMetadataChecked(created.ID, 3, empty, legacy)
		if err != nil || written || len(rec.writes) != 0 {
			t.Fatalf("%s: empty patch = (%v, %v), writes %v; want nothing written", name, written, err, rec.writes)
		}
	}

	if _, err := NewStore(beads.SessionStore{Store: unfenced}).UpdateMetadataChecked("s-missing", 3, empty, legacy); err == nil {
		t.Fatal("UpdateMetadataChecked on a missing row: want the read error")
	}
	failing := failingUpdateStore{Store: beads.NewMemStore()}
	created := seedRowFenceSession(t, failing, "s-write-fails")
	written, err := NewStore(beads.SessionStore{Store: failing}).UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"alias": "x"}
	}, legacy)
	if written || !errors.Is(err, errCheckedWriteFailed) {
		t.Fatalf("UpdateMetadataChecked with a failing write = (%v, %v), want the write's error", written, err)
	}
}

var errCheckedWriteFailed = errors.New("update failed")

// failingUpdateStore fails every Update.
type failingUpdateStore struct{ beads.Store }

func (failingUpdateStore) Update(string, beads.UpdateOpts) error { return errCheckedWriteFailed }
