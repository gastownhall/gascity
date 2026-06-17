package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/usage"
)

type captureSink struct{ facts []usage.Fact }

func (c *captureSink) Record(_ context.Context, f usage.Fact) error {
	c.facts = append(c.facts, f)
	return nil
}

func TestEmitComputeFactForBead(t *testing.T) {
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	b, err := store.Create(beads.Bead{
		Title: "session",
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "s-x",
			"awake_started_at": start.Format(time.RFC3339),
			"slept_at":         slept.Format(time.RFC3339),
			"molecule_id":      "mol-7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	now := slept.Add(5 * time.Second)

	if !emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", now) {
		t.Fatal("expected first emit to record a fact")
	}
	if len(sink.facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(sink.facts))
	}
	f := sink.facts[0]
	if f.Kind != usage.KindCompute {
		t.Fatalf("kind = %q", f.Kind)
	}
	if f.WallSeconds != 90 {
		t.Fatalf("wall = %v, want 90 (slept_at - awake_started_at)", f.WallSeconds)
	}
	if f.RunID != "mol-7" {
		t.Fatalf("runID = %q, want mol-7", f.RunID)
	}
	if f.Runtime != "fake" || f.City != "demo" || f.Worker != "s-x" {
		t.Fatalf("unexpected fact fields: %+v", f)
	}
	if f.IdempotencyKey == "" {
		t.Fatal("missing idempotency key")
	}

	// Marker should now suppress re-emit. Re-fetch the bead (marker persisted).
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if emitComputeFactForBead(context.Background(), sink, store, refreshed, "fake", "demo", now) {
		t.Fatal("second emit on same interval must no-op (marker set)")
	}
	if len(sink.facts) != 1 {
		t.Fatalf("no new fact expected, got %d", len(sink.facts))
	}
}

func TestEmitComputeFactForBeadNoOps(t *testing.T) {
	store := beads.NewMemStore()
	ctx := context.Background()
	now := time.Now().UTC()
	sink := &captureSink{}

	// No awake_started_at → nothing to bill.
	b1, _ := store.Create(beads.Bead{Title: "s1", Metadata: map[string]string{"state": "asleep"}})
	if emitComputeFactForBead(ctx, sink, store, b1, "fake", "demo", now) {
		t.Fatal("no awake_started_at must no-op")
	}
	// Discard sink → no-op even with a valid interval.
	b2, _ := store.Create(beads.Bead{Title: "s2", Metadata: map[string]string{"state": "asleep", "awake_started_at": now.Format(time.RFC3339)}})
	if emitComputeFactForBead(ctx, usage.Discard, store, b2, "fake", "demo", now) {
		t.Fatal("discard sink must no-op")
	}
	if len(sink.facts) != 0 {
		t.Fatalf("expected no facts, got %d", len(sink.facts))
	}
}

func TestIsComputeTerminalState(t *testing.T) {
	for _, s := range []string{"asleep", "drained", "archived"} {
		if !isComputeTerminalState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []string{"active", "creating", ""} {
		if isComputeTerminalState(s) {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
