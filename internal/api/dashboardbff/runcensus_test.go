package dashboardbff

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

func TestRunCensusSourceServesOnlyWarmAggregateCounts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	active := runMoleculeEvent(1, "run-active", "test-formula", "worker-1")
	step := runCensusBeadEvent(2, beads.Bead{
		ID: "run-active.step", Title: "private step title", Type: "task", Status: "in_progress",
		Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "run-active"},
	})
	writeEventLog(t, logPath, active, step)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	census, ok := p.RunCensus(context.Background(), "alpha")
	if !ok {
		t.Fatal("RunCensus reported a registered city as unknown")
	}
	if census.StatusCounts.Active != 1 {
		t.Fatalf("status_counts = %+v, want active=1", census.StatusCounts)
	}
	if !census.Ready || census.Partial {
		t.Fatalf("warm census = %+v, want ready and complete", census)
	}
}

func TestRunCensusSourceRejectsUnknownCity(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	if _, ok := p.RunCensus(context.Background(), "ghost"); ok {
		t.Fatal("RunCensus accepted an unknown city")
	}
}

func TestRunCensusSourceAcceptsRegistryCityNames(t *testing.T) {
	for _, cityName := range []string{"alpha_beta", "alpha.beta"} {
		t.Run(cityName, func(t *testing.T) {
			dir := t.TempDir()
			writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"),
				runMoleculeEvent(1, "run-one", "test-formula", ""),
			)
			p := New(Deps{Resolver: fakeResolver{paths: map[string]string{cityName: dir}}})
			p.Start(t.Context())
			t.Cleanup(p.Stop)

			census, ok := p.RunCensus(context.Background(), cityName)
			if !ok {
				t.Fatalf("RunCensus rejected registered city name %q", cityName)
			}
			if !census.Ready || census.StatusCounts.Pending != 1 {
				t.Fatalf("census = %+v, want warm pending=1", census)
			}
		})
	}
}

func TestRunCensusSourceMarksDecodeMissesPartial(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runMoleculeEvent(1, "run-active", "test-formula", ""),
		events.Event{Seq: 2, Type: events.BeadCreated, Payload: json.RawMessage(`{"status":"open"}`)},
	)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	census, ok := p.RunCensus(context.Background(), "alpha")
	if !ok {
		t.Fatal("RunCensus reported a registered city as unknown")
	}
	if !census.Ready || !census.Partial {
		t.Fatalf("census = %+v, want ready partial snapshot after a decode miss", census)
	}
	if len(census.PartialReasons) != 1 || census.PartialReasons[0] != "run projection is incomplete" {
		t.Fatalf("partial reasons = %q, want one sanitized incomplete reason", census.PartialReasons)
	}
}

func TestRunCensusSourceUsesIncrementalTailAfterColdLoad(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run-one", "test-formula", ""))

	state := captureTailCursor(logPath)
	projector := runproj.NewProjector()
	if err := projector.ColdLoad(logPath); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	tailer := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	tailer.build(projector, nil, nil)
	close(tailer.readyCh)

	first := tailer.runCensus(context.Background())
	second := tailer.runCensus(context.Background())
	if first.StatusCounts.Pending != 1 || second.StatusCounts != first.StatusCounts || second.Ready != first.Ready || second.Partial != first.Partial {
		t.Fatalf("repeated warm census = %+v / %+v, want stable pending=1", first, second)
	}

	appendEvents(t, logPath, runCensusBeadEvent(2, beads.Bead{
		ID: "run-one.step", Title: "step", Type: "task", Status: "in_progress",
		Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: "run-one"},
	}))
	tailer.foldNext(projector, state)
	if projector.LastSeq() != 2 {
		t.Fatalf("incremental tail cursor = %d, want 2", projector.LastSeq())
	}

	updated := tailer.runCensus(context.Background())
	if updated.StatusCounts.Pending != 0 || updated.StatusCounts.Active != 1 {
		t.Fatalf("incremental census = %+v, want pending=0 active=1", updated.StatusCounts)
	}
}

func TestRunCensusSourceMarksIncrementalDecodeMissPartial(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run-one", "test-formula", ""))

	state := captureTailCursor(logPath)
	projector := runproj.NewProjector()
	if err := projector.ColdLoad(logPath); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	tailer := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	tailer.build(projector, nil, nil)
	close(tailer.readyCh)

	appendEvents(t, logPath, events.Event{
		Seq: 2, Type: events.BeadUpdated, Payload: json.RawMessage(`{"status":"open"}`),
	})
	tailer.foldNext(projector, state)

	got := tailer.runCensus(context.Background())
	if !got.Ready || !got.Partial {
		t.Fatalf("incremental census = %+v, want ready partial snapshot after decode miss", got)
	}
}

func runCensusBeadEvent(seq uint64, bead beads.Bead) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{Bead: bead})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}
