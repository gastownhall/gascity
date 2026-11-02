package session

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// The checked metadata write (UpdateMetadataChecked, #8313): the started
// cells' writers' verb on a store that resolves no conditional writer — the
// default fresh-init topology's session store, whose pinned bd lacks
// --if-revision — where only a casOnly that explicitly reports legacy admits
// the unfenced write.

// seedCheckedSession opens a session front door over store with one active
// session row, for the checked-write tests.
func seedCheckedSession(t *testing.T, store beads.Store) (*Store, beads.Bead) {
	t.Helper()
	created, err := store.Create(sessionBeadFixture("s-checked", "open", map[string]string{
		"state":        string(StateActive),
		"session_name": "s-checked",
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return NewStore(beads.SessionStore{Store: store}), created
}

// checkedNoWriterBackends are the no-writer shapes the default topology and a
// degraded rollout produce: a store conditional writes are off for, an
// auto-mode one that cannot fence, and a require-mode one that cannot fence
// (the last for the fail-closed case).
func checkedNoWriterBackends() []struct {
	name  string
	open  func(*testing.T) beads.Store
	fails bool // the refusal is require mode's, not the unsupported sentinel
} {
	stamped := func(mode gate.Mode) func(*testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			mem := beads.NewMemStore()
			store := stampedMemStore(t, mode, mem)
			mem.DisableConditionalWrites = true
			return store
		}
	}
	return []struct {
		name  string
		open  func(*testing.T) beads.Store
		fails bool
	}{
		{"off", func(*testing.T) beads.Store { return beads.NewMemStore() }, false},
		{"auto-degraded", stamped(gate.Auto), false},
		{"require-incapable", stamped(gate.Require), true},
	}
}

// legacyCheckedMode is the explicit legacy predicate: the city records started
// claims on a store that cannot fence, too.
func legacyCheckedMode() func() bool { return func() bool { return false } }

// TestUpdateMetadataCheckedRefusesWithoutExplicitLegacy proves a store that
// resolves no conditional writer refuses the write before the deciding read
// unless casOnly explicitly reports legacy: a nil predicate (missing wiring)
// and one reporting a records-only-by-CAS city both return
// beads.ErrConditionalWriteUnsupported, and require mode fails closed on its
// own error without asking the predicate at all. Nothing is written either way.
func TestUpdateMetadataCheckedRefusesWithoutExplicitLegacy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		casOnly func() bool
	}{
		{"nil", nil},
		{"records only by CAS", func() bool { return true }},
	} {
		for _, backend := range checkedNoWriterBackends() {
			t.Run(backend.name+"/"+tc.name, func(t *testing.T) {
				store := backend.open(t)
				front, created := seedCheckedSession(t, store)
				asked := 0
				casOnly := tc.casOnly
				if casOnly != nil {
					casOnly = func() bool { asked++; return tc.casOnly() }
				}

				written, err := front.UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
					t.Fatal("decide ran although the write was refused before the deciding read")
					return nil
				}, casOnly)
				unsupported := errors.Is(err, beads.ErrConditionalWriteUnsupported)
				if backend.fails {
					unsupported = beads.IsConditionalWritesRequired(err)
					if asked != 0 {
						t.Fatalf("the mode predicate was asked %d times under require mode", asked)
					}
				} else if tc.casOnly == nil && asked != 0 {
					t.Fatalf("a nil predicate was called %d times", asked)
				}
				if written || !unsupported {
					t.Fatalf("UpdateMetadataChecked = (%v, %v), want the refusal", written, err)
				}
				if got, err := store.Get(created.ID); err != nil || got.Metadata["alias"] != "" {
					t.Fatalf("row after the refusal: alias=%q (%v); want nothing written", got.Metadata["alias"], err)
				}
			})
		}
	}
}

// TestUpdateMetadataCheckedWritesUnfencedOnlyOnExplicitLegacy proves the
// explicit-legacy fallback on every no-writer shape that does not fail closed:
// one unfenced Update right after the deciding read. Kills a fallback that
// also serves require mode, and one that writes before decide runs.
func TestUpdateMetadataCheckedWritesUnfencedOnlyOnExplicitLegacy(t *testing.T) {
	for _, backend := range checkedNoWriterBackends() {
		if backend.fails {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			rec := &rowWriteRecorder{Store: store}
			front, created := seedCheckedSession(t, rec)

			written, err := front.UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
				return MetadataPatch{"alias": "checked"}
			}, legacyCheckedMode())
			if err != nil || !written {
				t.Fatalf("UpdateMetadataChecked = (%v, %v), want written", written, err)
			}
			if !slices.Equal(rec.writes, []string{"Update"}) {
				t.Fatalf("writes = %v, want one unfenced Update", rec.writes)
			}
			if got, err := store.Get(created.ID); err != nil || got.Metadata["alias"] != "checked" {
				t.Fatalf("row after the write: alias=%q (%v); want the patch landed", got.Metadata["alias"], err)
			}
		})
	}
}

// TestUpdateMetadataCheckedAsksTheModeBeforeTheDecidingRead proves the
// predicate is asked before the row is read for the decision: a row that
// moves under the (potentially slow) mode query is the one decide sees. Kills
// a deciding read taken before the query.
func TestUpdateMetadataCheckedAsksTheModeBeforeTheDecidingRead(t *testing.T) {
	store := beads.NewMemStore()
	front, created := seedCheckedSession(t, store)
	casOnly := func() bool {
		if err := store.SetMetadata(created.ID, "alias", "written-under-the-mode-query"); err != nil {
			t.Fatal(err)
		}
		return false
	}

	var seen string
	written, err := front.UpdateMetadataChecked(created.ID, 3, func(fresh Info, _ PersistedResponse) MetadataPatch {
		seen = fresh.Alias
		return MetadataPatch{"alias": "decided"}
	}, casOnly)
	if err != nil || !written {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), want written", written, err)
	}
	if seen != "written-under-the-mode-query" {
		t.Fatalf("decide saw alias %q, want the row as it stands after the mode query", seen)
	}
}

// TestUpdateMetadataCheckedUsesTheWriterAndNeverAsksTheMode proves a store
// that resolves a conditional writer takes the revision CAS and never asks
// the mode predicate — a nil one included. Kills a capability+mode conjunct
// that consults the city on every write.
func TestUpdateMetadataCheckedUsesTheWriterAndNeverAsksTheMode(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	rec := &rowWriteRecorder{Store: store}
	front, created := seedCheckedSession(t, rec)

	written, err := front.UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"alias": "checked"}
	}, nil)
	if err != nil || !written {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), want written by CAS", written, err)
	}
	if !slices.Equal(rec.writes, []string{"UpdateIfMatch"}) {
		t.Fatalf("writes = %v, want one UpdateIfMatch and no blind write", rec.writes)
	}
}

// TestUpdateMetadataCheckedRedecidesOnContention proves the CAS loop's
// bounds: a write whose fence loses to a concurrent write re-reads and
// re-decides on the fresh row and lands within attempts, and a call with no
// attempts left writes nothing and reports no error.
func TestUpdateMetadataCheckedRedecidesOnContention(t *testing.T) {
	t.Run("re-decides and lands", func(t *testing.T) {
		store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
		front, created := seedCheckedSession(t, store)
		decisions := 0

		written, err := front.UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
			decisions++
			if decisions == 1 {
				if err := store.SetMetadataBatch(created.ID, map[string]string{"session_name": "moved"}); err != nil {
					t.Fatalf("concurrent write: %v", err)
				}
			}
			return MetadataPatch{"alias": "checked"}
		}, nil)
		if err != nil || !written || decisions != 2 {
			t.Fatalf("UpdateMetadataChecked = (%v, %v) after %d decisions, want written on the second", written, err, decisions)
		}
		got, err := store.Get(created.ID)
		if err != nil || got.Metadata["alias"] != "checked" || got.Metadata["session_name"] != "moved" {
			t.Fatalf("row after the write: %v; want both the concurrent write and the patch", got.Metadata)
		}
	})
	t.Run("out of attempts writes nothing", func(t *testing.T) {
		store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
		front, created := seedCheckedSession(t, store)

		written, err := front.UpdateMetadataChecked(created.ID, 1, func(Info, PersistedResponse) MetadataPatch {
			if err := store.SetMetadataBatch(created.ID, map[string]string{"session_name": "moved"}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
			return MetadataPatch{"alias": "checked"}
		}, nil)
		if err != nil || written {
			t.Fatalf("UpdateMetadataChecked = (%v, %v), want nothing written and no error", written, err)
		}
		if got, err := store.Get(created.ID); err != nil || got.Metadata["alias"] != "" {
			t.Fatalf("row after the lost write: alias=%q (%v); want the concurrent write alone", got.Metadata["alias"], err)
		}
	})
}

// TestUpdateMetadataCheckedEmptyPatchWritesNothing proves decide returning an
// empty patch writes nothing, on the CAS arm and the legacy fallback arm both.
func TestUpdateMetadataCheckedEmptyPatchWritesNothing(t *testing.T) {
	for _, backend := range []struct {
		name    string
		open    func(*testing.T) beads.Store
		casOnly func() bool
	}{
		{"CAS", func(t *testing.T) beads.Store { return stampedMemStore(t, gate.Auto, beads.NewMemStore()) }, nil},
		{"legacy fallback", func(*testing.T) beads.Store { return beads.NewMemStore() }, legacyCheckedMode()},
	} {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			rec := &rowWriteRecorder{Store: store}
			front, created := seedCheckedSession(t, rec)

			written, err := front.UpdateMetadataChecked(created.ID, 3, func(Info, PersistedResponse) MetadataPatch {
				return nil
			}, backend.casOnly)
			if written || err != nil || len(rec.writes) != 0 {
				t.Fatalf("UpdateMetadataChecked = (%v, %v), writes %v; want nothing written", written, err, rec.writes)
			}
		})
	}
}

// TestUpdateMetadataCheckedPropagatesReadErrors proves a row the deciding
// read cannot load is an error, never a quiet success.
func TestUpdateMetadataCheckedPropagatesReadErrors(t *testing.T) {
	front := NewStore(beads.SessionStore{Store: beads.NewMemStore()})
	for _, casOnly := range []func() bool{nil, legacyCheckedMode()} {
		written, err := front.UpdateMetadataChecked("no-such-row", 3, func(Info, PersistedResponse) MetadataPatch {
			t.Fatal("decide ran for a row that cannot be read")
			return nil
		}, casOnly)
		if written || err == nil || !strings.Contains(err.Error(), "no-such-row") {
			t.Fatalf("UpdateMetadataChecked = (%v, %v), want the read error naming the row", written, err)
		}
	}
}

// failingBlindUpdateStore is a no-writer store whose blind Update always
// fails, for the fallback's error path.
type failingBlindUpdateStore struct{ beads.Store }

func (s failingBlindUpdateStore) Update(string, beads.UpdateOpts) error {
	return errors.New("sessions store unavailable")
}

// TestUpdateMetadataCheckedUnmintsAFailedBlindWrite proves the legacy
// fallback's failed write leaves no open disposition mint, as the CAS arm's
// lost attempts do.
func TestUpdateMetadataCheckedUnmintsAFailedBlindWrite(t *testing.T) {
	mem := beads.NewMemStore() // unstamped: no conditional writer, so the blind arm runs
	b, err := mem.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive), "wait_hold": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	before := OpenDispositionMints(b.ID)
	front := NewStore(beads.SessionStore{Store: failingBlindUpdateStore{Store: mem}})

	written, err := front.UpdateMetadataChecked(b.ID, 3, func(info Info, _ PersistedResponse) MetadataPatch {
		to, err := DispositionOfInfo(info).Apply(ActorController, DispEvent{Kind: EvWaitEnd}, State(info.MetadataState), dispT0)
		if err != nil {
			t.Fatal(err)
		}
		return to.Changed().Into(info.ID, nil)
	}, legacyCheckedMode())
	if written || err == nil {
		t.Fatalf("UpdateMetadataChecked = (%v, %v), want the failed blind write", written, err)
	}
	if n := OpenDispositionMints(b.ID) - before; n != 0 {
		t.Errorf("the failed blind write left %d open mints", n)
	}
}
