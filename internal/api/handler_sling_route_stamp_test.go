package api

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// routeStampRecorder records the metadata writes a router makes.
type routeStampRecorder struct {
	beads.Store
	singles []string            // keys written one at a time by SetMetadata
	batches []map[string]string // each SetMetadataBatch call's pairs
}

func (r *routeStampRecorder) SetMetadata(id, key, value string) error {
	r.singles = append(r.singles, key)
	return r.Store.SetMetadata(id, key, value)
}

func (r *routeStampRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	r.batches = append(r.batches, maps.Clone(kvs))
	return r.Store.SetMetadataBatch(id, kvs)
}

// TestAPIBeadRouterStampsRoutedAtOnlyOnARouteChange pins the API router's
// gc.routed_at stamp: a route change writes gc.routed_to and a parseable
// gc.routed_at in one SetMetadataBatch call, and re-routing a bead to the
// route it already has writes no gc.routed_at.
func TestAPIBeadRouterStampsRoutedAtOnlyOnARouteChange(t *testing.T) {
	route := func(t *testing.T, prior string) (*routeStampRecorder, beads.Bead) {
		t.Helper()
		state := newFakeState(t)
		state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "smiths", Dir: "myrig", MaxActiveSessions: intPtr(4)})
		rec := &routeStampRecorder{Store: beads.NewMemStore()}
		bead, err := rec.Create(beads.Bead{Title: "work", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: prior}})
		if err != nil {
			t.Fatalf("create bead: %v", err)
		}
		r := apiBeadRouter{server: New(state), store: beadstest.Work(rec)}
		if err := r.Route(t.Context(), sling.RouteRequest{BeadID: bead.ID, Target: "myrig/smiths", PriorRoutedTo: prior}); err != nil {
			t.Fatalf("route: %v", err)
		}
		bead, err = rec.Get(bead.ID)
		if err != nil {
			t.Fatalf("reload bead: %v", err)
		}
		return rec, bead
	}

	t.Run("a route change stamps gc.routed_at in the same write", func(t *testing.T) {
		rec, bead := route(t, "myrig/worker")
		if len(rec.batches) != 1 || len(rec.singles) != 0 {
			t.Fatalf("writes = %d batches %v + singles %v, want exactly one SetMetadataBatch", len(rec.batches), rec.batches, rec.singles)
		}
		batch := rec.batches[0]
		if batch[beadmeta.RoutedToMetadataKey] != "myrig/smiths" {
			t.Fatalf("batch %v: gc.routed_to = %q, want myrig/smiths", batch, batch[beadmeta.RoutedToMetadataKey])
		}
		if _, ok := beadmeta.ParseRoutedAt(batch[beadmeta.RoutedAtMetadataKey]); !ok {
			t.Fatalf("batch %v: gc.routed_at does not parse", batch)
		}
		if bead.Metadata[beadmeta.RoutedToMetadataKey] != "myrig/smiths" {
			t.Fatalf("bead gc.routed_to = %q, want myrig/smiths", bead.Metadata[beadmeta.RoutedToMetadataKey])
		}
	})
	t.Run("an unchanged route writes no gc.routed_at", func(t *testing.T) {
		rec, bead := route(t, "myrig/smiths")
		for _, batch := range rec.batches {
			if _, ok := batch[beadmeta.RoutedAtMetadataKey]; ok {
				t.Fatalf("batch %v stamps gc.routed_at on an unchanged route", batch)
			}
		}
		if _, ok := bead.Metadata[beadmeta.RoutedAtMetadataKey]; ok {
			t.Fatalf("bead metadata %v carries gc.routed_at after an unchanged route", bead.Metadata)
		}
		if bead.Metadata[beadmeta.RoutedToMetadataKey] != "myrig/smiths" {
			t.Fatalf("bead gc.routed_to = %q, want myrig/smiths", bead.Metadata[beadmeta.RoutedToMetadataKey])
		}
	})
}
