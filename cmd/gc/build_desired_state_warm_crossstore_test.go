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

// TestBuildDesiredState_WarmRigPoolSeesCityStoreRoutedDemand: cross-store
// delivery (vp-kvp) routes molecule steps for rig pools into the CITY store,
// so city-store routed demand is legitimate demand for a rig pool at all
// times — not only while the pool sleeps. The city-store probe used to be
// gated on isCold, leaving a WARM rig pool structurally blind to city-store
// routed work: demand pinned at the rig-store count while routed beads sat
// unclaimed in the city store, and pools at the warm/cold boundary oscillated
// pool_desired N↔0 (cold ticks glimpsed city demand, warm ticks went blind)
// and were mass orphan-drained every flip (maintainer-city freeze,
// 2026-07-16: implementation-worker pinned at poolDesired=1 with 1 rig-store
// and 9 city-store routed beads, serializing every live molecule and starving
// all downstream control beads).
func TestBuildDesiredState_WarmRigPoolSeesCityStoreRoutedDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// A live pool-managed session bead makes the pool WARM (isCold=false):
	// runningSessions counts open, awake, pool-managed session beads across
	// the city + rig stores, no process probe involved.
	if _, err := cityStore.Create(beads.Bead{
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name": "gc__worker-1",
			"template":     "gascity/worker",
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	// Routed demand delivered cross-store into the CITY store; the rig store
	// stays empty. Before the fix the warm pool probed only the rig store and
	// read 0 here.
	for i := 0; i < 3; i++ {
		if _, err := cityStore.Create(beads.Bead{
			Title:    "cross-store routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": "gascity/worker"},
		}); err != nil {
			t.Fatalf("create routed city bead %d: %v", i, err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "gascity",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 3 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 3: a WARM rig pool must count "+
			"city-store routed demand (cross-store delivery), not just its own rig store — "+
			"gating the city probe on isCold leaves warm rig pools blind to routed work and "+
			"pins pool demand at the rig-store count (full ScaleCheckCounts=%v, partial=%v)",
			got, dsResult.ScaleCheckCounts, dsResult.PoolScaleCheckPartialTemplates)
	}
}

// TestBuildDesiredState_WarmRigPoolCityProbeDoesNotDoubleCountRigDemand: the
// warm city probe is a UNION with the rig-store probe, not a duplicate — a
// bead routed to the pool in the RIG store must be counted exactly once when
// both probes run.
func TestBuildDesiredState_WarmRigPoolCityProbeDoesNotDoubleCountRigDemand(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	if _, err := cityStore.Create(beads.Bead{
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"session_name": "gc__worker-1",
			"template":     "gascity/worker",
			"state":        "active",
			"pool_managed": "true",
		},
	}); err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	// One routed bead in EACH store: expect a count of exactly 2 (1+1), not 3+.
	if _, err := rigStore.Create(beads.Bead{
		Title:    "rig-store routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "gascity/worker"},
	}); err != nil {
		t.Fatalf("create routed rig bead: %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{
		Title:    "city-store routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "gascity/worker"},
	}); err != nil {
		t.Fatalf("create routed city bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "gascity",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(5),
		}},
	}

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if got := dsResult.ScaleCheckCounts["gascity/worker"]; got != 2 {
		t.Fatalf("ScaleCheckCounts[gascity/worker] = %d, want 2 (1 rig + 1 city, no double count; full=%v)",
			got, dsResult.ScaleCheckCounts)
	}
}
