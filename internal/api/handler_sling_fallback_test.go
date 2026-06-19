package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func runtimeMissingSessionBead(id, qualified, state, sleepReason string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{"agent:" + qualified, session.LabelSession},
		Metadata: map[string]string{
			"session_name": id,
			"state":        state,
			"sleep_reason": sleepReason,
		},
	}
}

// TestServerControlDispatcherRuntimeMissing covers the API sling path's wiring
// of the rig→city control-dispatcher fallback (#3454). The checker reads the
// server's already-open city bead store (never openCityStoreAt), so it stays
// off the managed-Dolt spawn path on the sling hot path.
func TestServerControlDispatcherRuntimeMissing(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		runtimeMissingSessionBead("gc-cd", "gc-contrib/control-dispatcher", "asleep", "runtime-missing"),
	}, nil)
	s := &Server{state: &fakeState{cityBeadStore: store}}
	if !s.controlDispatcherRuntimeMissing("gc-contrib/control-dispatcher") {
		t.Fatal("expected runtime-missing rig dispatcher to report true")
	}
	if s.controlDispatcherRuntimeMissing("gc-contrib/coder") {
		t.Fatal("non-dispatcher agent should report false")
	}
}

func TestServerControlDispatcherRuntimeMissing_HealthyAndNilStore(t *testing.T) {
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		runtimeMissingSessionBead("gc-cd", "gc-contrib/control-dispatcher", "awake", ""),
	}, nil)
	s := &Server{state: &fakeState{cityBeadStore: store}}
	if s.controlDispatcherRuntimeMissing("gc-contrib/control-dispatcher") {
		t.Fatal("awake dispatcher should report false")
	}
	// A nil city store must report not-missing rather than panic, and never
	// spins up a managed Dolt backend.
	if (&Server{state: &fakeState{}}).controlDispatcherRuntimeMissing("gc-contrib/control-dispatcher") {
		t.Fatal("nil city store should report false")
	}
}
