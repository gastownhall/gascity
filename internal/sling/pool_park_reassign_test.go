package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReassignReleasesPoolPark is the pool start-failure park's unpark verb
// (cmd/gc/pool_start_backoff.go C6): `gc sling --reassign` clears every park
// and start-failure key on the bead before routing, in the same update that
// clears the assignee, so the target pool starts it with a clean count.
func TestReassignReleasesPoolPark(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store
	parked := map[string]string{
		beadmeta.ParkedAtMetadataKey:          "2026-09-14T12:00:00Z",
		beadmeta.ParkReasonMetadataKey:        "fatal: two branches match",
		beadmeta.ParkFailuresMetadataKey:      "5",
		beadmeta.ParkMailFailedMetadataKey:    "2026-09-14T12:00:00Z",
		beadmeta.StartFailuresMetadataKey:     "2",
		beadmeta.StartFailedAtMetadataKey:     "2026-09-14T11:59:00Z",
		beadmeta.StartFailureMetadataKey:      "fatal: two branches match",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-14T12:05:00Z",
		"gc.keep":                             "yes",
	}
	bead, err := store.Create(beads.Bead{Title: "task", Type: "task", Status: "open", Metadata: parked})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	if _, err := s.ExpandConvoy(context.Background(), bead.ID, a, RouteOpts{Reassign: true}, store); err != nil {
		t.Fatalf("ExpandConvoy: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range ParkReleaseMetadataKeys {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("%s = %q after --reassign, want cleared", key, v)
		}
	}
	if got.Metadata["gc.keep"] != "yes" {
		t.Errorf("unrelated metadata was touched: %v", got.Metadata)
	}
	if len(ParkReleaseMetadataKeys) != 8 {
		t.Fatalf("ParkReleaseMetadataKeys = %v, want the four park keys and the four counter keys", ParkReleaseMetadataKeys)
	}

	// The re-dispatch of a parked bead to the pool it is ALREADY routed to is
	// the normal case: the idempotent short-circuit must not skip the release.
	routed, err := store.Create(beads.Bead{Title: "routed", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpandConvoy(context.Background(), routed.ID, a, RouteOpts{}, store); err != nil {
		t.Fatalf("first route: %v", err)
	}
	if err := store.Update(routed.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.ParkedAtMetadataKey:      "2026-09-14T12:00:00Z",
		beadmeta.ParkFailuresMetadataKey:  "5",
		beadmeta.StartFailuresMetadataKey: "1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpandConvoy(context.Background(), routed.ID, a, RouteOpts{Reassign: true}, store); err != nil {
		t.Fatalf("reassign of an already-routed bead: %v", err)
	}
	got, err = store.Get(routed.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{beadmeta.ParkedAtMetadataKey, beadmeta.ParkFailuresMetadataKey, beadmeta.StartFailuresMetadataKey} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("already-routed bead: %s = %q after --reassign, want cleared (the idempotent short-circuit skipped the release)", key, v)
		}
	}

	// A CONVOY re-dispatch takes DoSlingBatch: each parked child, already
	// routed to the target, is released too, not skipped as idempotent.
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "child", Type: "task", Status: "open", ParentID: convoy.ID, Metadata: map[string]string{
		"gc.routed_to":                        "mayor",
		beadmeta.ParkedAtMetadataKey:          "2026-09-14T12:00:00Z",
		beadmeta.ParkFailuresMetadataKey:      "5",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-14T12:05:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpandConvoy(context.Background(), convoy.ID, a, RouteOpts{Reassign: true}, store); err != nil {
		t.Fatalf("convoy reassign: %v", err)
	}
	got, err = store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{beadmeta.ParkedAtMetadataKey, beadmeta.ParkFailuresMetadataKey, beadmeta.StartBackoffUntilMetadataKey} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("convoy child: %s = %q after --reassign, want cleared (DoSlingBatch skipped the release)", key, v)
		}
	}

	// Without --reassign the park stands: routing alone never unparks.
	again, err := store.Create(beads.Bead{Title: "task2", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.ParkedAtMetadataKey: "2026-09-14T12:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpandConvoy(context.Background(), again.ID, a, RouteOpts{}, store); err != nil {
		t.Fatalf("ExpandConvoy: %v", err)
	}
	if got, _ := store.Get(again.ID); got.Metadata[beadmeta.ParkedAtMetadataKey] == "" {
		t.Fatal("a plain sling cleared the park; only --reassign may")
	}
}
