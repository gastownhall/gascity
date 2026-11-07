package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// fencedGraphStepBead is a graph-first step the way molecule.Instantiate
// creates it under the instantiation fence: the route is withheld into
// gc.deferred_routed_to, the type is deferred to a gate, and gc.run_target is
// the formula's bare pool name. Activation restores all three; until then an
// empty gc.routed_to is deliberate, not lost.
func fencedGraphStepBead(id string, fence map[string]string) beads.Bead {
	meta := map[string]string{beadmeta.RunTargetMetadataKey: routeRecoveryTestPool}
	for k, v := range fence {
		meta[k] = v
	}
	return beads.Bead{ID: id, Title: "Resolve PR identity", Type: "gate", Status: "open", Metadata: meta}
}

// TestRouteRecoveryLeavesAFencedGraphStepAlone pins that route recovery never
// reads a graph-first mint's instantiation fence as a lost route.
//
// A fenced step has no gc.kind, an empty gc.routed_to and a bare gc.run_target,
// which is exactly the carried-route shape the lane repairs — so it restored
// the BARE pool name onto the step. Two things go wrong at once: the bare name
// matches no seat, and the restore is a metadata read-merge-write that can
// land over the activation write and put the fence back (westeros qcore,
// 2026-09-09 07:45:43Z: 15 review roots lost in 10 days, one entry step each).
func TestRouteRecoveryLeavesAFencedGraphStepAlone(t *testing.T) {
	cases := map[string]map[string]string{
		"fenced: instantiating, deferred route and type": {
			beadmeta.InstantiatingMetadataKey:    "true",
			beadmeta.DeferredRoutedToMetadataKey: "rig/" + routeRecoveryTestPool,
			beadmeta.DeferredTypeMetadataKey:     "task",
		},
		"deferred route only (a failed mint blanks instantiating first)": {
			beadmeta.DeferredRoutedToMetadataKey: "rig/" + routeRecoveryTestPool,
		},
		"instantiating only": {
			beadmeta.InstantiatingMetadataKey: "true",
		},
	}
	for name, fence := range cases {
		t.Run(name, func(t *testing.T) {
			cr, store := routeRecoveryRuntime(t, fencedGraphStepBead("T-fenced", fence))
			writesBefore := store.writes

			report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
			if report.restored != 0 {
				t.Fatalf("backstop restored %d route(s) onto a fenced step, want 0", report.restored)
			}
			if store.writes != writesBefore {
				t.Fatalf("backstop issued %d write(s) to a fenced step, want 0", store.writes-writesBefore)
			}
			b, err := store.Get("T-fenced")
			if err != nil {
				t.Fatalf("Get(T-fenced): %v", err)
			}
			if got := b.Metadata[beadmeta.RoutedToMetadataKey]; got != "" {
				t.Fatalf("gc.routed_to = %q on a fenced step, want empty: the route is withheld in %s, not lost", got, beadmeta.DeferredRoutedToMetadataKey)
			}

			// The event feed must not name it as a candidate either.
			lane := cr.routeRecoveryLaneOf()
			lane.observe(beadCreatedEvent(t, fencedGraphStepBead("T-fenced", fence)))
			if pending := lane.takePending(); len(pending) != 0 {
				t.Fatalf("event feed named %v as route candidate(s), want none", pending)
			}
		})
	}
}
