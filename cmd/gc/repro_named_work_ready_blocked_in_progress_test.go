package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockedProjectionStore wraps a beads.Store and stamps IsBlocked=true on
// every row List returns whose ID matches blockedID, mirroring the real
// store's denormalized dependency-ready projection (bd's IsBlocked column,
// as exercised by beadsRouteReadinessStore in
// route_equivalence_beads_test.go). A direct in-memory store never carries
// this projection on its own, so tests that need a blocked in_progress bead
// have to add it this way.
type blockedProjectionStore struct {
	beads.Store
	blockedID string
}

func (s blockedProjectionStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	for i := range rows {
		if rows[i].ID == s.blockedID {
			blocked := true
			rows[i].IsBlocked = &blocked
		}
	}
	return rows, err
}

// TestNamedWorkReadyExcludesBlockedInProgressBead is the gascity#8767 repro.
//
// namedWorkReady's loop over in_progress work beads treated every in_progress
// bead as always-actionable named demand, without checking the store's
// IsBlocked projection — unlike workBeadHasAwakeDemand (compute_awake_set.go),
// which correctly returns !bead.Blocked. A named session holding an
// in_progress bead blocked by an open gate/dependency was therefore counted
// as having named demand forever, so it was never idle-eligible even though
// it had nothing actionable to do — causing spurious `gc session reset`
// restarts of a correctly-waiting session.
//
// This test sets up a named session with an in_progress work bead whose
// IsBlocked projection is true, and asserts namedWorkReady (surfaced via
// DesiredStateResult.NamedSessionDemand) no longer counts it as named demand.
func TestNamedWorkReadyExcludesBlockedInProgressBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	const identity = "gascity/patrol"

	b, err := rigStore.Create(beads.Bead{
		Title:    "patrol work blocked on an open gate",
		Type:     "task",
		Status:   "open",
		Assignee: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	blockedRigStore := blockedProjectionStore{Store: rigStore, blockedID: b.ID}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:         "patrol",
			Dir:          "gascity",
			StartCommand: "true",
			WorkQuery:    "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "patrol",
			Dir:      "gascity",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": blockedRigStore}, nil, nil, io.Discard,
	)

	if dsResult.NamedSessionDemand[identity] {
		t.Fatalf("named session %q has a blocked in_progress bead %s but still generated named-session "+
			"demand (NamedSessionDemand=%v); namedWorkReady must skip in_progress beads whose IsBlocked "+
			"projection is true, matching workBeadHasAwakeDemand's !bead.Blocked on the wake side (gascity#8767)",
			identity, b.ID, dsResult.NamedSessionDemand)
	}
}

// TestNamedWorkReadyStillCountsUnblockedInProgressBead is the control for the
// test above: an in_progress bead with no IsBlocked projection set (the
// common case — nil, not blocked) must still count as named demand, so the
// fix does not break the normal always-actionable in_progress case.
func TestNamedWorkReadyStillCountsUnblockedInProgressBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	const identity = "gascity/patrol"

	b, err := rigStore.Create(beads.Bead{
		Title:    "patrol work with nothing blocking it",
		Type:     "task",
		Status:   "open",
		Assignee: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:         "patrol",
			Dir:          "gascity",
			StartCommand: "true",
			WorkQuery:    "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "patrol",
			Dir:      "gascity",
			Mode:     "on_demand",
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("named session %q has an unblocked in_progress bead %s but generated no named-session "+
			"demand (NamedSessionDemand=%v); the normal always-actionable in_progress case must still work",
			identity, b.ID, dsResult.NamedSessionDemand)
	}
}
