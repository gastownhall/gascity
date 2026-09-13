package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFakeStateNudgesBeadStoreFallsBackToCityStore documents the default-backend
// equivalence: with no relocated nudges store configured, NudgesBeadStore returns
// the work store, so the API path is byte-identical at the default backend.
func TestFakeStateNudgesBeadStoreFallsBackToCityStore(t *testing.T) {
	f := newFakeState(t)
	// NudgesBeadStore returns the strongly-typed beads.NudgesStore; compare its
	// embedded .Store to the work store for the default-backend identity check.
	if got := f.NudgesBeadStore(); got.Store != f.CityBeadStore() {
		t.Fatalf("default backend: NudgesBeadStore() must equal CityBeadStore(); got distinct stores")
	}
	relocated := beads.NewMemStore()
	f.nudgesBeadStore = relocated
	if got := f.NudgesBeadStore(); got.Store != relocated {
		t.Fatalf("relocated backend: NudgesBeadStore() must return the configured nudges store")
	}
}

// TestFakeStateSessionsBeadStoreFallsBackToCityStore documents the default-backend
// equivalence for the sessions seam: with no relocated sessions store configured,
// SessionsBeadStore returns the work store, so the API path is byte-identical at
// the default backend.
func TestFakeStateSessionsBeadStoreFallsBackToCityStore(t *testing.T) {
	f := newFakeState(t)
	// SessionsBeadStore returns the strongly-typed beads.SessionStore; compare its
	// embedded .Store to the work store for the default-backend identity check.
	if got := f.SessionsBeadStore(); got.Store != f.CityBeadStore() {
		t.Fatalf("default backend: SessionsBeadStore() must equal CityBeadStore(); got distinct stores")
	}
	relocated := beads.NewMemStore()
	f.sessionsBeadStore = relocated
	if got := f.SessionsBeadStore(); got.Store != relocated {
		t.Fatalf("relocated backend: SessionsBeadStore() must return the configured sessions store")
	}
}

// TestFakeStateGraphBeadStoreFallsBackToCityStore documents the default-backend
// equivalence for the graph seam: with no relocated graph store configured,
// GraphBeadStore returns the work store, so the API path is byte-identical at the
// default backend.
func TestFakeStateGraphBeadStoreFallsBackToCityStore(t *testing.T) {
	f := newFakeState(t)
	// GraphBeadStore returns the strongly-typed beads.GraphStore; compare its
	// embedded .Store to the work store for the default-backend identity check.
	if got := f.GraphBeadStore(); got.Store != f.CityBeadStore() {
		t.Fatalf("default backend: GraphBeadStore() must equal CityBeadStore(); got distinct stores")
	}
	relocated := beads.NewMemStore()
	f.graphBeadStore = relocated
	if got := f.GraphBeadStore(); got.Store != relocated {
		t.Fatalf("relocated backend: GraphBeadStore() must return the configured graph store")
	}
}
