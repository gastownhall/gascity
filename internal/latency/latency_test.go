package latency

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func mustEncodeBead(t *testing.T, b beads.Bead) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	return data
}

func beadEvent(t *testing.T, seq uint64, eventType string, ts time.Time, b beads.Bead) events.Event {
	t.Helper()
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: b.ID,
		Payload: mustEncodeBead(t, b),
	}
}

func stepEvent(seq uint64, eventType string, ts time.Time, subject, runID, stepID string) events.Event {
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: subject,
		RunID:   runID,
		StepID:  stepID,
	}
}

// --- Metric 1: claim wait per pool ---

func TestAnalyze_ClaimWait_RoutedThenClaimed(t *testing.T) {
	now := time.Now().UTC()
	routedAt := now
	claimedAt := now.Add(90 * time.Second)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, routedAt, beads.Bead{
			ID:     "wf-1",
			Status: "open",
			Metadata: beads.StringMap{
				beadmeta.RoutedToMetadataKey: "worker-pool",
			},
		}),
		beadEvent(t, 2, events.BeadUpdated, claimedAt, beads.Bead{
			ID:       "wf-1",
			Status:   "in_progress",
			Assignee: "worker-pool-slot-1",
			Metadata: beads.StringMap{
				beadmeta.RoutedToMetadataKey: "worker-pool",
			},
		}),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.ClaimWait) != 1 {
		t.Fatalf("expected 1 pool group, got %d: %+v", len(report.ClaimWait), report.ClaimWait)
	}
	g := report.ClaimWait[0]
	if g.Pool != "worker-pool" {
		t.Errorf("Pool = %q, want worker-pool", g.Pool)
	}
	if g.Stats.Count != 1 {
		t.Fatalf("Count = %d, want 1", g.Stats.Count)
	}
	if g.Stats.MinMs != 90000 || g.Stats.MaxMs != 90000 {
		t.Errorf("expected 90000ms sample, got min=%d max=%d", g.Stats.MinMs, g.Stats.MaxMs)
	}
}

func TestAnalyze_ClaimWait_UnroutedBeadProducesNoSample(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "b-1", Status: "open"}),
		beadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "direct-session",
		}),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.ClaimWait) != 0 {
		t.Errorf("expected no claim-wait groups for a non-pool-routed bead, got %+v", report.ClaimWait)
	}
}

func TestAnalyze_ClaimWait_ReopenedBeforeClaimAbandonsWait(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		// Dead-assignee reopen clears routing without ever claiming.
		beadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "open",
		}),
		beadEvent(t, 3, events.BeadUpdated, now.Add(2*time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "someone",
		}),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.ClaimWait) != 0 {
		t.Errorf("expected the abandoned wait to produce no sample, got %+v", report.ClaimWait)
	}
}

func TestAnalyze_ClaimWait_ReroutedToDifferentPoolAttributesToNewPool(t *testing.T) {
	now := time.Now().UTC()
	reroutedAt := now.Add(time.Minute)
	claimedAt := now.Add(2 * time.Minute)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		// Rerouted to a different pool before ever being claimed.
		beadEvent(t, 2, events.BeadUpdated, reroutedAt, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
		beadEvent(t, 3, events.BeadUpdated, claimedAt, beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.ClaimWait) != 1 {
		t.Fatalf("expected 1 pool group, got %d: %+v", len(report.ClaimWait), report.ClaimWait)
	}
	g := report.ClaimWait[0]
	if g.Pool != "pool-b" {
		t.Errorf("Pool = %q, want pool-b (the pool that actually claimed the bead)", g.Pool)
	}
	wantMs := claimedAt.Sub(reroutedAt).Milliseconds()
	if g.Stats.MinMs != wantMs {
		t.Errorf("MinMs = %d, want %d (wait measured from reroute, not original routing)", g.Stats.MinMs, wantMs)
	}
}

func TestAnalyze_ClaimWait_PoolFilter(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		beadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		beadEvent(t, 3, events.BeadCreated, now, beads.Bead{
			ID: "b-2", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
		beadEvent(t, 4, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-2", Status: "in_progress", Assignee: "slot-2",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
	}
	report := Analyze(es, Window{}, Filter{Pool: "pool-a"})
	if len(report.ClaimWait) != 1 || report.ClaimWait[0].Pool != "pool-a" {
		t.Errorf("pool filter failed: %+v", report.ClaimWait)
	}
}

func TestAnalyze_ClaimWait_UndecodablePayloadCountsAsSkipped(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		{Seq: 1, Type: events.BeadCreated, Ts: now, Subject: "b-1", Payload: json.RawMessage(`not json`)},
	}
	report := Analyze(es, Window{}, Filter{})
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
}

func TestAnalyze_ClaimWait_WindowFiltersOnClaimTimestamp(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now.Add(-48*time.Hour), beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		beadEvent(t, 2, events.BeadUpdated, now.Add(-47*time.Hour), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
	}
	report := Analyze(es, Window{Since: now.Add(-time.Hour)}, Filter{})
	if len(report.ClaimWait) != 0 {
		t.Errorf("expected claim outside window to be excluded, got %+v", report.ClaimWait)
	}
}

// --- Metric 2: gate queue wait ---

func TestAnalyze_GateQueueWait_PairsDefinedAndStarted(t *testing.T) {
	now := time.Now().UTC()
	definedAt := now
	startedAt := now.Add(5 * time.Second)
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "root-1",
			Metadata: beads.StringMap{
				beadmeta.FormulaNameMetadataKey: "mol-verify",
			},
		}),
		stepEvent(2, events.ExecutionStepDefined, definedAt, "step-bead-1", "root-1", "check"),
		stepEvent(3, events.ExecutionStepStarted, startedAt, "step-bead-1", "root-1", "check"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateQueueWait) != 1 {
		t.Fatalf("expected 1 gate-queue-wait group, got %d: %+v", len(report.GateQueueWait), report.GateQueueWait)
	}
	g := report.GateQueueWait[0]
	if g.Formula != "mol-verify" || g.StepID != "check" {
		t.Errorf("group key wrong: %+v", g)
	}
	if g.Stats.Count != 1 || g.Stats.MinMs != 5000 {
		t.Errorf("stats wrong: %+v", g.Stats)
	}
}

func TestAnalyze_GateQueueWait_UnknownFormulaWhenRootNotObserved(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		stepEvent(1, events.ExecutionStepDefined, now, "step-bead-1", "root-missing", "check"),
		stepEvent(2, events.ExecutionStepStarted, now.Add(time.Second), "step-bead-1", "root-missing", "check"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateQueueWait) != 1 || report.GateQueueWait[0].Formula != unknownFormula {
		t.Errorf("expected unknown-formula bucket, got %+v", report.GateQueueWait)
	}
}

func TestAnalyze_GateQueueWait_NoStartedEventProducesNoSample(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		stepEvent(1, events.ExecutionStepDefined, now, "step-bead-1", "root-1", "check"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateQueueWait) != 0 {
		t.Errorf("expected no groups for a defined-but-never-started step, got %+v", report.GateQueueWait)
	}
}

func TestAnalyze_GateQueueWait_FormulaFilter(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-a"},
		}),
		beadEvent(t, 2, events.BeadCreated, now, beads.Bead{
			ID:       "root-2",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-b"},
		}),
		stepEvent(3, events.ExecutionStepDefined, now, "step-1", "root-1", "check"),
		stepEvent(4, events.ExecutionStepStarted, now.Add(time.Second), "step-1", "root-1", "check"),
		stepEvent(5, events.ExecutionStepDefined, now, "step-2", "root-2", "check"),
		stepEvent(6, events.ExecutionStepStarted, now.Add(time.Second), "step-2", "root-2", "check"),
	}
	report := Analyze(es, Window{}, Filter{Formula: "mol-a"})
	if len(report.GateQueueWait) != 1 || report.GateQueueWait[0].Formula != "mol-a" {
		t.Errorf("formula filter failed: %+v", report.GateQueueWait)
	}
}

// --- Metric 3: gate bounce rate ---

func TestAnalyze_GateBounce_RedefinitionCountsAsBounce(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-verify"},
		}),
		stepEvent(2, events.ExecutionStepDefined, now, "step-bead-1", "root-1", "check"),
		stepEvent(3, events.ExecutionStepDefined, now.Add(time.Minute), "step-bead-2", "root-1", "check"),
		stepEvent(4, events.ExecutionStepDefined, now.Add(2*time.Minute), "step-bead-3", "root-1", "check"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateBounce) != 1 {
		t.Fatalf("expected 1 formula group, got %d: %+v", len(report.GateBounce), report.GateBounce)
	}
	g := report.GateBounce[0]
	if g.Formula != "mol-verify" {
		t.Errorf("Formula = %q, want mol-verify", g.Formula)
	}
	if g.Definitions != 3 {
		t.Errorf("Definitions = %d, want 3", g.Definitions)
	}
	if g.Bounces != 2 {
		t.Errorf("Bounces = %d, want 2", g.Bounces)
	}
	if want := 2.0 / 3.0; g.BounceRate != want {
		t.Errorf("BounceRate = %v, want %v", g.BounceRate, want)
	}
}

func TestAnalyze_GateBounce_SingleDefinitionHasZeroBounces(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		stepEvent(1, events.ExecutionStepDefined, now, "step-bead-1", "root-1", "check"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateBounce) != 1 {
		t.Fatalf("expected 1 group, got %+v", report.GateBounce)
	}
	if report.GateBounce[0].Bounces != 0 || report.GateBounce[0].BounceRate != 0 {
		t.Errorf("expected zero bounces, got %+v", report.GateBounce[0])
	}
}

func TestAnalyze_GateBounce_WindowExcludesDefinitionsOutsideRange(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now.Add(-48*time.Hour), beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-verify"},
		}),
		// Outside the window below: must not count toward Definitions/Bounces.
		stepEvent(2, events.ExecutionStepDefined, now.Add(-47*time.Hour), "step-bead-1", "root-1", "check"),
		stepEvent(3, events.ExecutionStepDefined, now.Add(-46*time.Hour), "step-bead-2", "root-1", "check"),
		// Inside the window.
		stepEvent(4, events.ExecutionStepDefined, now, "step-bead-3", "root-1", "check"),
	}
	report := Analyze(es, Window{Since: now.Add(-time.Hour)}, Filter{})
	if len(report.GateBounce) != 1 {
		t.Fatalf("expected 1 formula group, got %d: %+v", len(report.GateBounce), report.GateBounce)
	}
	g := report.GateBounce[0]
	if g.Definitions != 1 || g.Bounces != 0 {
		t.Errorf("expected window to exclude the two out-of-range definitions, got %+v", g)
	}
}

func TestAnalyze_GateBounce_MultipleStepsWithinFormulaAggregate(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-verify"},
		}),
		stepEvent(2, events.ExecutionStepDefined, now, "step-bead-1", "root-1", "check"),
		stepEvent(3, events.ExecutionStepDefined, now.Add(time.Minute), "step-bead-2", "root-1", "check"),
		stepEvent(4, events.ExecutionStepDefined, now, "step-bead-3", "root-1", "build"),
	}
	report := Analyze(es, Window{}, Filter{})
	if len(report.GateBounce) != 1 {
		t.Fatalf("expected 1 formula group (both steps aggregate), got %+v", report.GateBounce)
	}
	g := report.GateBounce[0]
	if g.Definitions != 3 || g.Bounces != 1 {
		t.Errorf("aggregate wrong: %+v", g)
	}
}

func TestAnalyze_GateBounce_FormulaFilter(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		beadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-a"},
		}),
		beadEvent(t, 2, events.BeadCreated, now, beads.Bead{
			ID:       "root-2",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-b"},
		}),
		stepEvent(3, events.ExecutionStepDefined, now, "step-1", "root-1", "check"),
		stepEvent(4, events.ExecutionStepDefined, now, "step-2", "root-2", "check"),
	}
	report := Analyze(es, Window{}, Filter{Formula: "mol-a"})
	if len(report.GateBounce) != 1 || report.GateBounce[0].Formula != "mol-a" {
		t.Errorf("formula filter failed: %+v", report.GateBounce)
	}
}

// --- Shared helpers ---

func TestComputeDurationStats_Empty(t *testing.T) {
	stats := computeDurationStats(nil)
	if stats.Count != 0 {
		t.Errorf("expected zero-value stats for empty input, got %+v", stats)
	}
}

func TestComputeDurationStats_Basic(t *testing.T) {
	stats := computeDurationStats([]int64{100, 200, 300, 400, 500})
	if stats.Count != 5 {
		t.Errorf("Count = %d, want 5", stats.Count)
	}
	if stats.MinMs != 100 || stats.MaxMs != 500 {
		t.Errorf("min/max wrong: %+v", stats)
	}
	if stats.AvgMs != 300 {
		t.Errorf("AvgMs = %d, want 300", stats.AvgMs)
	}
	if stats.P50Ms != 300 {
		t.Errorf("P50Ms = %d, want 300", stats.P50Ms)
	}
}

func TestWindow_Contains(t *testing.T) {
	now := time.Now().UTC()
	w := Window{Since: now.Add(-time.Hour), Until: now}
	if !w.Contains(now.Add(-30 * time.Minute)) {
		t.Error("expected timestamp within window to be contained")
	}
	if w.Contains(now.Add(-2 * time.Hour)) {
		t.Error("expected timestamp before Since to be excluded")
	}
	if w.Contains(now.Add(time.Hour)) {
		t.Error("expected timestamp after Until to be excluded")
	}
}
