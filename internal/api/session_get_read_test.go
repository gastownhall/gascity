package api

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These are the load-bearing pins for the behavior the read-model Get cutover
// actually introduced: sessionGetEnriched / bridgeSessionGetError carry the
// ErrSessionNotFound->ErrNotSession (400) vs beads.ErrNotFound (404) bridge and
// the re-issued empty-type heal. The retired Manager.GetWithPersistedResponse
// was oracled by the wire test; the real production path is oracled here.

func newSessionFront(store beads.Store) *session.Store {
	return session.NewStore(beads.SessionStore{Store: store})
}

// TestSessionGetEnrichedAbsentIsNotFound: an absent id stays on the
// beads.ErrNotFound chain (NOT ErrNotSession) and maps to 404.
func TestSessionGetEnrichedAbsentIsNotFound(t *testing.T) {
	store := beads.NewMemStore()
	mgr := session.NewManagerWithOptions(store, runtime.NewFake())

	_, _, err := sessionGetEnriched(newSessionFront(store), mgr, "missing")
	if err == nil {
		t.Fatal("sessionGetEnriched(missing): want error, got nil")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("absent id must stay on the beads.ErrNotFound chain, got %v", err)
	}
	if errors.Is(err, session.ErrNotSession) {
		t.Fatalf("absent id must not be bridged to ErrNotSession, got %v", err)
	}
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != 404 {
		t.Fatalf("absent id status = %d, want 404", rec.Code)
	}
}

// TestSessionGetEnrichedNonSessionIsBadRequest: a present-but-non-session bead
// is bridged from ErrSessionNotFound to ErrNotSession and maps to 400.
func TestSessionGetEnrichedNonSessionIsBadRequest(t *testing.T) {
	nonSession := beads.Bead{ID: "task-1", Type: "task", Status: "open", Labels: []string{"work"}}
	store := beads.NewMemStoreFrom(1, []beads.Bead{nonSession}, nil)
	mgr := session.NewManagerWithOptions(store, runtime.NewFake())

	_, _, err := sessionGetEnriched(newSessionFront(store), mgr, "task-1")
	if err == nil {
		t.Fatal("sessionGetEnriched(non-session): want error, got nil")
	}
	if !errors.Is(err, session.ErrNotSession) {
		t.Fatalf("present non-session bead must bridge to ErrNotSession, got %v", err)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("present non-session bead must not be on the beads.ErrNotFound chain, got %v", err)
	}
	rec := httptest.NewRecorder()
	writeSessionManagerError(rec, err)
	if rec.Code != 400 {
		t.Fatalf("non-session status = %d, want 400", rec.Code)
	}
}

// TestSessionGetEnrichedHealsTypeLostBead: a type-lost (empty Type, session
// label) bead is healed back to the canonical session type on GET, exactly as
// the retired loadSessionBead RepairEmptyType did — the heal fires on a read,
// not just on the reconciler tick.
func TestSessionGetEnrichedHealsTypeLostBead(t *testing.T) {
	typeLost := beads.Bead{
		ID:       "s-typelost",
		Type:     "", // lost after a partial write / schema migration
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"state": "asleep", "session_name": "s-typelost"},
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{typeLost}, nil)
	mgr := session.NewManagerWithOptions(store, runtime.NewFake())

	info, _, err := sessionGetEnriched(newSessionFront(store), mgr, "s-typelost")
	if err != nil {
		t.Fatalf("sessionGetEnriched(type-lost): %v", err)
	}
	if info.Type != session.BeadType {
		t.Fatalf("returned Info.Type = %q, want %q", info.Type, session.BeadType)
	}
	healed, err := store.Get("s-typelost")
	if err != nil {
		t.Fatalf("store.Get after heal: %v", err)
	}
	if healed.Type != session.BeadType {
		t.Fatalf("persisted bead Type = %q, want %q (empty-type heal must persist)", healed.Type, session.BeadType)
	}
}

// TestBridgeSessionGetErrorNil: a nil error passes through as nil.
func TestBridgeSessionGetErrorNil(t *testing.T) {
	if got := bridgeSessionGetError("any", nil); got != nil {
		t.Fatalf("bridgeSessionGetError(nil) = %v, want nil", got)
	}
}
