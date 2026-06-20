package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/usage"
)

type captureSink struct{ facts []usage.Fact }

func (c *captureSink) Record(_ context.Context, f usage.Fact) error {
	c.facts = append(c.facts, f)
	return nil
}

type erroringSink struct{ calls int }

func (e *erroringSink) Record(context.Context, usage.Fact) error {
	e.calls++
	return errors.New("disk full")
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

	if !emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", now, nil) {
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
	if emitComputeFactForBead(context.Background(), sink, store, refreshed, "fake", "demo", now, nil) {
		t.Fatal("second emit on same interval must no-op (marker set)")
	}
	if len(sink.facts) != 1 {
		t.Fatalf("no new fact expected, got %d", len(sink.facts))
	}
}

// TestEmitComputeFactForBeadMultiInterval proves the create -> sleep -> wake ->
// sleep path bills two distinct compute facts. A reused session bead gets a
// fresh awake_started_at epoch on the second wake, so the interval-1 emit marker
// (keyed on the first epoch) does not suppress interval 2.
func TestEmitComputeFactForBeadMultiInterval(t *testing.T) {
	store := beads.NewMemStore()
	t1 := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	s1 := t1.Add(60 * time.Second)
	b, err := store.Create(beads.Bead{
		Title: "session",
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "pool-1",
			"awake_started_at": t1.Format(time.RFC3339Nano),
			"slept_at":         s1.Format(time.RFC3339Nano),
			"molecule_id":      "run-A",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}

	if !emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", s1.Add(time.Second), nil) {
		t.Fatal("interval 1 should emit")
	}

	// Second awake interval: the controller stamps a fresh epoch on wake.
	t2 := t1.Add(2 * time.Hour)
	s2 := t2.Add(30 * time.Second)
	if err := store.SetMetadata(b.ID, "awake_started_at", t2.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(b.ID, "slept_at", s2.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !emitComputeFactForBead(context.Background(), sink, store, refreshed, "fake", "demo", s2.Add(time.Second), nil) {
		t.Fatal("interval 2 should emit a second compute fact")
	}
	if len(sink.facts) != 2 {
		t.Fatalf("want 2 compute facts across two awake intervals, got %d", len(sink.facts))
	}
	if sink.facts[0].IdempotencyKey == sink.facts[1].IdempotencyKey {
		t.Fatal("two intervals must have distinct idempotency keys")
	}
	if sink.facts[0].WallSeconds != 60 || sink.facts[1].WallSeconds != 30 {
		t.Fatalf("interval wall seconds wrong: %v, %v", sink.facts[0].WallSeconds, sink.facts[1].WallSeconds)
	}
}

func TestEmitComputeFactForBeadSinkErrorIsLogged(t *testing.T) {
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{Title: "s", Metadata: map[string]string{
		"state":            "asleep",
		"awake_started_at": start.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	sink := &erroringSink{}

	if emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", start.Add(time.Minute), logf) {
		t.Fatal("a failing sink must not report success")
	}
	if len(logged) == 0 {
		t.Fatal("sink failure must be surfaced via logf, not dropped silently")
	}
	// Marker must stay unset so a later tick retries.
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("marker must not be set when the sink write failed (so the fact retries)")
	}
}

func TestEmitComputeFactForBeadNoOps(t *testing.T) {
	store := beads.NewMemStore()
	ctx := context.Background()
	now := time.Now().UTC()
	sink := &captureSink{}

	// No awake_started_at → nothing to bill.
	b1, _ := store.Create(beads.Bead{Title: "s1", Metadata: map[string]string{"state": "asleep"}})
	if emitComputeFactForBead(ctx, sink, store, b1, "fake", "demo", now, nil) {
		t.Fatal("no awake_started_at must no-op")
	}
	// Discard sink → no-op even with a valid interval.
	b2, _ := store.Create(beads.Bead{Title: "s2", Metadata: map[string]string{"state": "asleep", "awake_started_at": now.Format(time.RFC3339)}})
	if emitComputeFactForBead(ctx, usage.Discard, store, b2, "fake", "demo", now, nil) {
		t.Fatal("discard sink must no-op")
	}
	if len(sink.facts) != 0 {
		t.Fatalf("expected no facts, got %d", len(sink.facts))
	}
}

// TestEmitComputeFactForBeadHungSinkReturnsPromptly is the reconcile-path half
// of the hung-exec-sink regression: a compute fact whose sink write hangs must
// not stall the reconcile tick, and must leave the emit marker unset so the
// fact retries on a later tick.
func TestEmitComputeFactForBeadHungSinkReturnsPromptly(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{Title: "s", Metadata: map[string]string{
		"state":            "asleep",
		"awake_started_at": start.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	sink := usage.NewExecSinkWithTimeout(script, 100*time.Millisecond)

	done := make(chan bool, 1)
	began := time.Now()
	go func() {
		done <- emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", start.Add(time.Minute), nil)
	}()
	select {
	case ok := <-done:
		if elapsed := time.Since(began); elapsed > 5*time.Second {
			t.Fatalf("reconcile compute path blocked on a hung sink: took %s", elapsed)
		}
		if ok {
			t.Fatal("a timed-out sink write must report failure, not success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile compute path did not return under a hung sink")
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("marker must stay unset when the sink timed out (so the fact retries)")
	}
}

func TestIsComputeTerminalState(t *testing.T) {
	// Every non-running endpoint the open-bead scan can observe.
	for _, s := range []string{"asleep", "drained", "archived", "suspended", "quarantined"} {
		if !isComputeTerminalState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	// Running states, transient states, and closed (which leaves the open set the
	// scan reads) are not emitted by the scan.
	for _, s := range []string{"active", "awake", "creating", "draining", "closed", ""} {
		if isComputeTerminalState(s) {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
