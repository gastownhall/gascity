package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// TestClosedSessionIntervalSettledByUsagePass pins AC-1 of issue #6672: a
// session a close ended straight from awake carries the close's settle
// markers, and the next non-boot usage pass — whose open-snapshot feed no
// longer holds the closed row — still accounts exactly one compute fact for
// the closed interval, anchored at the close time, and clears the markers.
func TestClosedSessionIntervalSettledByUsagePass(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex pool worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-D",
			// No session_key: a terminal codex session without a key settles
			// its model sweep as a definitive miss, so the interval commits
			// on this pass.
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	// The close's settle markers, exactly as ClosePatch stamps them.
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	// The next reconcile tick's usage pass: the open snapshot no longer holds
	// the closed row, exactly as the tick feeds it.
	cr.emitDueComputeFacts(context.Background(), nil, false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	var computes []usage.Fact
	for _, f := range facts {
		if f.Kind == usage.KindCompute {
			computes = append(computes, f)
		}
	}
	if len(computes) != 1 {
		t.Fatalf("compute facts = %d, want 1", len(computes))
	}
	if got := computes[0].SessionID; got != b.ID {
		t.Errorf("compute fact session = %q, want %q", got, b.ID)
	}
	if got := computes[0].WallSeconds; got != 600 {
		t.Errorf("compute fact wall seconds = %v, want 600 (closed_at − awake_started_at)", got)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[usageComputeEmittedAtKey] != startRaw {
		t.Errorf("usage_compute_emitted_at = %q, want %q", got.Metadata[usageComputeEmittedAtKey], startRaw)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
		t.Errorf("usage_close_pending = %q, want cleared", v)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosedAt]; v != "" {
		t.Errorf("usage_closed_at = %q, want cleared", v)
	}
}

// TestClosedSessionSettleSweepsTrailingTurnsAndAdvancesCursor pins AC-2 of
// issue #6672: a closed codex row's settle runs the end-of-interval model
// sweep, so the turns its transcript gained after the last live sweep (the
// drain-ack turn among them) each yield exactly one model fact at the raw
// sink, and the invocation cursor advances on the closed bead so a later
// sweep of the same conversation re-reads nothing.
func TestClosedSessionSettleSweepsTrailingTurnsAndAdvancesCursor(t *testing.T) {
	cityPath := t.TempDir()
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()
	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := start.Add(10 * time.Minute)
	startRaw := start.Format(time.RFC3339Nano)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex pool worker",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "worker-1",
			"awake_started_at": startRaw,
			"session_key":      codexSweepSessionKey,
			"work_dir":         workDir,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-E",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(b.ID, session.UsageClosePatch(closedAt)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: nil, cityName: "demo", cityPath: cityPath, stderr: io.Discard}
	cr.emitDueComputeFacts(context.Background(), nil, false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindCount(facts, usage.KindModel); got != 2 {
		t.Fatalf("model facts = %d, want 2 (one per trailing turn): %+v", got, facts)
	}
	if got := kindCount(facts, usage.KindCompute); got != 1 {
		t.Fatalf("compute facts = %d, want 1", got)
	}
	if got := rawSinkModelFactCount(t, sinkPath); got != 2 {
		t.Fatalf("raw model facts = %d, want 2 (each trailing turn recorded exactly once)", got)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Metadata[session.MetadataKeyInvocationUsageCursor]; v == "" {
		t.Error("invocation_usage_cursor on the closed bead is empty, want the last turn's identity")
	}
	if got.Metadata[usageModelSweptAtKey] != startRaw {
		t.Errorf("usage_model_swept_at = %q, want %q", got.Metadata[usageModelSweptAtKey], startRaw)
	}
	if v := got.Metadata[session.MetadataKeyUsageClosePending]; v != "" {
		t.Errorf("usage_close_pending = %q, want cleared", v)
	}
}
