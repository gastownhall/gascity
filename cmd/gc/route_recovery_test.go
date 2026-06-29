package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRestoreCarriedWorkRoutes covers ga-n2d.4: after a controller restart,
// open+unassigned work that carries a gc.run_target pool route but no
// gc.routed_to is invisible to the pool autoscaler (which keys on gc.routed_to)
// and never spawns a worker. restoreCarriedWorkRoutes must re-stamp gc.routed_to
// from the route the bead already declares, for both carriers of a legacy route
// — a plain (kind-less) standalone work bead and a pre-ga-eld2x workflow root —
// while leaving every bead for which gc.run_target is not a recoverable pool
// route untouched: already-routed, assigned, closed, control-dispatcher, and
// workflow-topology beads.
func TestRestoreCarriedWorkRoutes(t *testing.T) {
	const pool = "gascity/gastown.polecat"
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Recoverable: open workflow root, run_target set, routed_to empty.
		{ID: "WR-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Already routed — left alone (idempotent, no double-write).
		{ID: "WR-2", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool, "gc.routed_to": "gascity/gastown.refinery",
		}},
		// Assigned workflow root — already claimed, no route restored.
		{ID: "WR-3", Title: "root", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Closed workflow root — done, no route restored.
		{ID: "WR-4", Title: "root", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": pool,
		}},
		// Recoverable broadening: a plain (kind-less) standalone work bead — this
		// fork's dominant work shape — carries its pool route in gc.run_target
		// too. The autoscaler is blind to it until gc.routed_to is restored.
		{ID: "T-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Assigned plain work bead — already claimed, no route restored.
		{ID: "T-2", Title: "work", Type: "task", Status: "open", Assignee: pool, Metadata: map[string]string{
			"gc.run_target": pool,
		}},
		// Already-routed plain work bead — idempotent, left untouched.
		{ID: "T-3", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": pool, "gc.routed_to": pool,
		}},
		// Control-dispatcher and workflow-topology beads carry a bare
		// gc.run_target, but there it is a dispatch/structure target an agent
		// never claims from a pool — they must never be pool-routed.
		{ID: "CTRL-1", Title: "retry", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "retry", "gc.run_target": pool,
		}},
		{ID: "TOPO-1", Title: "scope", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "scope", "gc.run_target": pool,
		}},
		{ID: "TOPO-2", Title: "spec", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "spec", "gc.run_target": pool,
		}},
	}, nil)

	restored, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes: %v", err)
	}
	if restored != 2 {
		t.Fatalf("restored = %d, want 2 (WR-1 workflow root + T-1 plain work bead)", restored)
	}

	// Restored from the route each bead already carried.
	for _, id := range []string{"WR-1", "T-1"} {
		if got := mustRoutedTo(t, store, id); got != pool {
			t.Errorf("%s gc.routed_to = %q, want %q (restored from gc.run_target)", id, got, pool)
		}
	}
	// Already-routed beads keep their original route, not their run_target.
	if got := mustRoutedTo(t, store, "WR-2"); got != "gascity/gastown.refinery" {
		t.Errorf("WR-2 gc.routed_to = %q, want gascity/gastown.refinery (untouched)", got)
	}
	if got := mustRoutedTo(t, store, "T-3"); got != pool {
		t.Errorf("T-3 gc.routed_to = %q, want %q (untouched)", got, pool)
	}
	// Assigned, closed, control, and topology beads must stay unrouted.
	for _, id := range []string{"WR-3", "WR-4", "T-2", "CTRL-1", "TOPO-1", "TOPO-2"} {
		if got := mustRoutedTo(t, store, id); got != "" {
			t.Errorf("%s gc.routed_to = %q, want empty (must be left unrouted)", id, got)
		}
	}

	// Idempotent: a second pass restores nothing because WR-1 and T-1 now carry
	// gc.routed_to and yield no recoverable carried route.
	restored2, err := restoreCarriedWorkRoutes(store)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes (second pass): %v", err)
	}
	if restored2 != 0 {
		t.Errorf("second pass restored = %d, want 0 (idempotent)", restored2)
	}
}

// TestRestoreCarriedWorkRoutesNilStore guards the nil-store path the controller
// hits when a scope's bead store is unavailable.
func TestRestoreCarriedWorkRoutesNilStore(t *testing.T) {
	restored, err := restoreCarriedWorkRoutes(nil)
	if err != nil {
		t.Fatalf("restoreCarriedWorkRoutes(nil): %v", err)
	}
	if restored != 0 {
		t.Errorf("restored = %d, want 0 for nil store", restored)
	}
}

// TestCityRuntimeRecoverUnroutedWorkRoutes confirms the controller method
// sweeps both the city store and every rig store, and recovers both carried-route
// shapes (workflow root and plain work bead).
func TestCityRuntimeRecoverUnroutedWorkRoutes(t *testing.T) {
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CW-1", Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "city/gastown.polecat",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Plain work bead — the fork's standalone-issue shape.
		{ID: "RW-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.run_target": "gascity/gastown.polecat",
		}},
	}, nil)
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"gascity": rigStore},
		stderr:              io.Discard,
	}

	cr.recoverUnroutedWorkRoutes()

	if got := mustRoutedTo(t, cityStore, "CW-1"); got != "city/gastown.polecat" {
		t.Errorf("CW-1 gc.routed_to = %q, want city/gastown.polecat", got)
	}
	if got := mustRoutedTo(t, rigStore, "RW-1"); got != "gascity/gastown.polecat" {
		t.Errorf("RW-1 gc.routed_to = %q, want gascity/gastown.polecat", got)
	}
}

func mustRoutedTo(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Metadata["gc.routed_to"]
}
