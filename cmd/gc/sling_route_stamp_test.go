package main

import (
	"context"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// routeWriteRecorder records the metadata writes a router makes.
type routeWriteRecorder struct {
	beads.Store
	singles []string            // keys written one at a time by SetMetadata
	batches []map[string]string // each SetMetadataBatch call's pairs
}

func (r *routeWriteRecorder) SetMetadata(id, key, value string) error {
	r.singles = append(r.singles, key)
	return r.Store.SetMetadata(id, key, value)
}

func (r *routeWriteRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	r.batches = append(r.batches, maps.Clone(kvs))
	return r.Store.SetMetadataBatch(id, kvs)
}

// TestCliBeadRouterStampsRoutedAtOnlyOnARouteChange pins the CLI router's
// gc.routed_at stamp: a route change writes gc.routed_to and a parseable
// gc.routed_at in one SetMetadataBatch call, and re-routing a bead to the
// route it already has writes no gc.routed_at.
func TestCliBeadRouterStampsRoutedAtOnlyOnARouteChange(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "alpha", MaxActiveSessions: intPtr(2)},
			{Name: "beta", MaxActiveSessions: intPtr(2)},
		},
	}
	route := func(t *testing.T, prior string) (*routeWriteRecorder, beads.Bead) {
		t.Helper()
		rec := &routeWriteRecorder{Store: beads.NewMemStore()}
		bead, err := rec.Create(beads.Bead{Title: "work", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: prior}})
		if err != nil {
			t.Fatalf("create bead: %v", err)
		}
		deps := ptr(slingDepsWithStore(slingDeps{CityName: "test-city", CityPath: t.TempDir(), Cfg: cfg, StoreRef: "city:test-city"}, rec))
		if err := (cliBeadRouter{deps: deps}).Route(context.Background(), sling.RouteRequest{BeadID: bead.ID, Target: "beta", PriorRoutedTo: prior}); err != nil {
			t.Fatalf("route: %v", err)
		}
		bead, err = rec.Get(bead.ID)
		if err != nil {
			t.Fatalf("reload bead: %v", err)
		}
		return rec, bead
	}

	t.Run("a route change stamps gc.routed_at in the same write", func(t *testing.T) {
		rec, bead := route(t, "alpha")
		if len(rec.batches) != 1 || len(rec.singles) != 0 {
			t.Fatalf("writes = %d batches %v + singles %v, want exactly one SetMetadataBatch", len(rec.batches), rec.batches, rec.singles)
		}
		batch := rec.batches[0]
		if batch[beadmeta.RoutedToMetadataKey] != "beta" {
			t.Fatalf("batch %v: gc.routed_to = %q, want beta", batch, batch[beadmeta.RoutedToMetadataKey])
		}
		if _, ok := beadmeta.ParseRoutedAt(batch[beadmeta.RoutedAtMetadataKey]); !ok {
			t.Fatalf("batch %v: gc.routed_at does not parse", batch)
		}
		if bead.Metadata[beadmeta.RoutedToMetadataKey] != "beta" {
			t.Fatalf("bead gc.routed_to = %q, want beta", bead.Metadata[beadmeta.RoutedToMetadataKey])
		}
	})
	t.Run("an unchanged route writes no gc.routed_at", func(t *testing.T) {
		rec, bead := route(t, "beta")
		for _, batch := range rec.batches {
			if _, ok := batch[beadmeta.RoutedAtMetadataKey]; ok {
				t.Fatalf("batch %v stamps gc.routed_at on an unchanged route", batch)
			}
		}
		if _, ok := bead.Metadata[beadmeta.RoutedAtMetadataKey]; ok {
			t.Fatalf("bead metadata %v carries gc.routed_at after an unchanged route", bead.Metadata)
		}
		if bead.Metadata[beadmeta.RoutedToMetadataKey] != "beta" {
			t.Fatalf("bead gc.routed_to = %q, want beta", bead.Metadata[beadmeta.RoutedToMetadataKey])
		}
	})
}
