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
