package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func fallbackSessionBead(id, qualified, state, sleepReason string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   sessionpkg.BeadType,
		Status: "open",
		Labels: []string{"agent:" + qualified, sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name": id,
			"state":        state,
			"sleep_reason": sleepReason,
		},
	}
}

func TestSessionRuntimeMissingInStore(t *testing.T) {
	tests := []struct {
		name      string
		bead      beads.Bead
		qualified string
		want      bool
	}{
		{
			name:      "asleep runtime-missing",
			bead:      fallbackSessionBead("gc-fopl", "gc-contrib/control-dispatcher", "asleep", "runtime-missing"),
			qualified: "gc-contrib/control-dispatcher",
			want:      true,
		},
		{
			name:      "awake",
			bead:      fallbackSessionBead("gc-0grpb", "gc-contrib/control-dispatcher", "awake", ""),
			qualified: "gc-contrib/control-dispatcher",
			want:      false,
		},
		{
			name:      "asleep deliberate reason",
			bead:      fallbackSessionBead("gc-x", "gc-contrib/control-dispatcher", "asleep", "idle"),
			qualified: "gc-contrib/control-dispatcher",
			want:      false,
		},
		{
			name:      "no session bead for agent",
			bead:      fallbackSessionBead("gc-other", "gc-contrib/coder", "asleep", "runtime-missing"),
			qualified: "gc-contrib/control-dispatcher",
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, []beads.Bead{tt.bead}, nil)
			if got := sessionRuntimeMissingInStore(beads.SessionStore{Store: store}, tt.qualified); got != tt.want {
				t.Fatalf("sessionRuntimeMissingInStore = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionRuntimeMissingInStore_NilAndEmpty(t *testing.T) {
	if sessionRuntimeMissingInStore(beads.SessionStore{}, "gc-contrib/control-dispatcher") {
		t.Fatal("nil store should report not-missing")
	}
	store := beads.NewMemStoreFrom(1, nil, nil)
	if sessionRuntimeMissingInStore(beads.SessionStore{Store: store}, "") {
		t.Fatal("empty qualified name should report not-missing")
	}
}

func TestControlDispatcherSessionRuntimeMissing_NoCityTomlSkips(t *testing.T) {
	// A bare working dir (no city.toml) must short-circuit before opening any
	// store, so routing never spins up a managed Dolt backend on the hot path.
	if controlDispatcherSessionRuntimeMissing(t.TempDir(), "gc-contrib/control-dispatcher") {
		t.Fatal("expected false for dir without city.toml")
	}
	if controlDispatcherSessionRuntimeMissing("", "gc-contrib/control-dispatcher") {
		t.Fatal("expected false for empty cityPath")
	}
}

// TestPopulateSlingDepsCallbacksWiresControlDispatcherRuntimeMissing guards the
// CLI sling path's wiring of the rig→city control-dispatcher fallback (#3454).
// CityPath points at a dir without city.toml so the wired closure short-circuits
// before opening any store — the package-wide Dolt leak-guard would otherwise
// trip when the sling hot path spins up a managed backend.
func TestPopulateSlingDepsCallbacksWiresControlDispatcherRuntimeMissing(t *testing.T) {
	deps := slingDeps{CityPath: t.TempDir()}
	populateSlingDepsCallbacks(&deps)
	if deps.ControlDispatcherRuntimeMissing == nil {
		t.Fatal("populateSlingDepsCallbacks did not wire ControlDispatcherRuntimeMissing")
	}
	if deps.ControlDispatcherRuntimeMissing("gc-contrib/control-dispatcher") {
		t.Fatal("expected false for a city dir without city.toml (no store opened)")
	}
}
