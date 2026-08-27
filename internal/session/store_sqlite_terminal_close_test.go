package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file drives Store.Close over a REAL beads.SQLiteStore — the store a
// routed sqlite city runs. Until the atomic terminal-close capability reached
// that store, Close took the historical ClosePatch-then-Close fallback there,
// which is the exact ga-f7v2ft.78.6 strand class: a stale controller write can
// land between the two commands and leave a closed row carrying a nonterminal
// lifecycle state.
//
// staleAwakeBeforeCloseBacking is the deterministic stand-in for that writer.
// It stamps state=awake exactly once, immediately before an unfenced Close
// reaches the backing store, and forwards the backing store's atomic
// terminal-close capability verbatim so the arm Close takes is decided by what
// SQLiteStore can really do rather than by the wrapper.
type staleAwakeBeforeCloseBacking struct {
	beads.Store
	fired       bool
	plainCloses int
}

func (s *staleAwakeBeforeCloseBacking) Close(id string) error {
	s.plainCloses++
	if !s.fired {
		s.fired = true
		if err := s.SetMetadata(id, "state", string(StateAwake)); err != nil {
			return err
		}
	}
	return s.Store.Close(id)
}

func (s *staleAwakeBeforeCloseBacking) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	return beads.AtomicConditionalCloserFor(s.Store)
}

func openSQLiteSessionRow(t *testing.T, metadata map[string]string) (beads.Store, string) {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	bead, err := store.Create(beads.Bead{
		Title:    "probe",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return store, bead.ID
}

// TestSQLiteStoreAdvertisesAtomicTerminalClose pins the capability discovery
// the fix turns on: session.Store.Close only leaves the split fallback when
// AtomicConditionalCloserFor resolves a closer through the typed class wrapper.
func TestSQLiteStoreAdvertisesAtomicTerminalClose(t *testing.T) {
	backing, _ := openSQLiteSessionRow(t, map[string]string{"state": string(StateAwake)})
	if _, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: backing}); !ok {
		t.Fatal("AtomicConditionalCloserFor(SessionStore{SQLiteStore}) = unavailable; the split close sequence stays live on a sqlite city")
	}
}

// TestCloseOverSQLiteStoreCannotStrandAwakeStateOnAClosedRow is the
// ga-f7v2ft.78.6 regression at the sqlite store. A stale controller write lands
// in the window between ClosePatch and Close; the durable row must never come
// to rest closed with a nonterminal lifecycle state, and the terminal write
// must be one fenced command rather than a metadata write followed by a
// separate close.
func TestCloseOverSQLiteStoreCannotStrandAwakeStateOnAClosedRow(t *testing.T) {
	backing, id := openSQLiteSessionRow(t, map[string]string{"state": string(StateSuspended)})
	stale := &staleAwakeBeforeCloseBacking{Store: backing}
	front := NewStore(beads.SessionStore{Store: stale})

	closed, err := front.Close(id, "drained", time.Date(2026, 8, 8, 10, 17, 35, 0, time.UTC))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close reported not-closed for an open suspended session")
	}

	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read durable row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("durable row = status=%q state=%q; want closed/%s (closed+awake is the incident signature)",
			row.Status, row.Metadata["state"], StateDrained)
	}
	if stale.plainCloses != 0 {
		t.Fatalf("terminal close fell back to %d unfenced Close calls, want 0", stale.plainCloses)
	}
}

// TestCloseOverSQLiteStoreYieldsToAWinningCloser proves the fused write is
// fenced: when another actor closes the row first, the terminal close reports
// already-closed instead of blindly re-closing.
func TestCloseOverSQLiteStoreYieldsToAWinningCloser(t *testing.T) {
	backing, id := openSQLiteSessionRow(t, map[string]string{"state": string(StateSuspended)})
	stale := &staleAwakeBeforeCloseBacking{Store: backing}
	front := NewStore(beads.SessionStore{Store: stale})

	if err := backing.Close(id); err != nil {
		t.Fatalf("pre-close the row: %v", err)
	}

	closed, err := front.Close(id, "drained", time.Now())
	if err != nil {
		t.Fatalf("Close on an already-closed row: %v", err)
	}
	if closed {
		t.Fatal("Close reported closed for a row another actor had already closed")
	}
	if stale.plainCloses != 0 {
		t.Fatalf("already-closed row took %d Close calls, want 0", stale.plainCloses)
	}
}

// TestSQLiteCloseWithMetadataIfMatchRefusesAStaleFence pins the store-level
// contract Close's retry arm depends on: a losing fence changes nothing at all,
// so a caller can reread and retry without inspecting a half-written row.
func TestSQLiteCloseWithMetadataIfMatchRefusesAStaleFence(t *testing.T) {
	backing, id := openSQLiteSessionRow(t, map[string]string{"state": string(StateSuspended)})
	closer, ok := beads.AtomicConditionalCloserFor(backing)
	if !ok {
		t.Fatal("SQLiteStore does not expose atomic terminal close")
	}
	observed, err := backing.Get(id)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if err := backing.SetMetadata(id, "state", string(StateAwake)); err != nil {
		t.Fatalf("advance the row under the observed revision: %v", err)
	}

	if _, err := closer.CloseWithMetadataIfMatch(id, observed.Revision, map[string]string{"state": string(StateDrained)}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch at a stale revision = %v, want a precondition failure", err)
	}
	row, err := backing.Get(id)
	if err != nil {
		t.Fatalf("reread row: %v", err)
	}
	if row.Status != "open" || row.Metadata["state"] != string(StateAwake) {
		t.Fatalf("losing fence mutated the row: status=%q state=%q, want open/%s", row.Status, row.Metadata["state"], StateAwake)
	}

	if _, err := closer.CloseWithMetadataIfMatch(id, row.Revision, map[string]string{"state": string(StateDrained)}); err != nil {
		t.Fatalf("CloseWithMetadataIfMatch at the current revision: %v", err)
	}
	row, err = backing.Get(id)
	if err != nil {
		t.Fatalf("reread closed row: %v", err)
	}
	if row.Status != "closed" || row.Metadata["state"] != string(StateDrained) {
		t.Fatalf("fused terminal write = status=%q state=%q, want closed/%s", row.Status, row.Metadata["state"], StateDrained)
	}
}
