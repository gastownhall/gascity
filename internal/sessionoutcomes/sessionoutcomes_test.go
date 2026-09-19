package sessionoutcomes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func startEvent(t *testing.T, seq uint64, ts time.Time, p workerOperationPayload) events.Event {
	t.Helper()
	if p.Operation == "" {
		p.Operation = "start"
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Event{Seq: seq, Type: events.WorkerOperation, Ts: ts, Payload: data}
}

func TestAnalyze_GroupsByTemplateProviderBucket(t *testing.T) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	es := []events.Event{
		startEvent(t, 1, base, workerOperationPayload{Result: "succeeded", Provider: "anthropic", Template: "polecat", DurationMs: 100}),
		startEvent(t, 2, base.Add(5*time.Minute), workerOperationPayload{Result: "failed", Provider: "anthropic", Template: "polecat", DurationMs: 200}),
		startEvent(t, 3, base.Add(2*time.Hour), workerOperationPayload{Result: "succeeded", Provider: "openai", Template: "manager", DurationMs: 300}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})

	if len(report.Groups) != 2 {
		t.Fatalf("expected 2 groups (2 distinct hour buckets), got %d: %+v", len(report.Groups), report.Groups)
	}
	first := report.Groups[0]
	if first.Key.Template != "polecat" || first.Key.Provider != "anthropic" {
		t.Errorf("first group key wrong: %+v", first.Key)
	}
	if first.Started != 2 || first.Succeeded != 1 || first.Failed != 1 {
		t.Errorf("first group counts wrong: %+v", first)
	}
	if report.Total.Started != 3 || report.Total.Succeeded != 2 || report.Total.Failed != 1 {
		t.Errorf("total wrong: %+v", report.Total)
	}
}

func TestAnalyze_NonStartOperationsIgnored(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Operation: "attach", Result: "succeeded", Provider: "anthropic", Template: "polecat"}),
		startEvent(t, 2, now, workerOperationPayload{Operation: "start_resolved", Result: "succeeded", Provider: "anthropic", Template: "polecat"}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})
	if report.Total.Started != 1 {
		t.Errorf("Total.Started = %d, want 1 (only start_resolved counts)", report.Total.Started)
	}
}

func TestAnalyze_MissingTemplateProviderGroupUnknown(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Result: "succeeded"}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})
	if len(report.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(report.Groups))
	}
	g := report.Groups[0]
	if g.Key.Template != unknownDim || g.Key.Provider != unknownDim {
		t.Errorf("expected unknown/unknown key, got %+v", g.Key)
	}
	if report.Instrumentation.MissingTemplate != 1 || report.Instrumentation.MissingProvider != 1 {
		t.Errorf("instrumentation wrong: %+v", report.Instrumentation)
	}
}

func TestAnalyze_OtherResultCounted(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Result: "timeout", Provider: "anthropic", Template: "polecat"}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})
	if report.Total.Other != 1 || report.Total.Succeeded != 0 || report.Total.Failed != 0 {
		t.Errorf("expected 1 Other, got %+v", report.Total)
	}
}

func TestAnalyze_WindowFiltersEvents(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now.Add(-2*time.Hour), workerOperationPayload{Result: "succeeded", Provider: "a", Template: "t"}),
		startEvent(t, 2, now, workerOperationPayload{Result: "succeeded", Provider: "a", Template: "t"}),
	}
	report := Analyze(es, Window{Since: now.Add(-time.Hour)}, time.Hour, Filter{})
	if report.Total.Started != 1 {
		t.Errorf("Total.Started = %d, want 1 (older event outside window)", report.Total.Started)
	}
}

func TestAnalyze_FilterByTemplateAndProvider(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Result: "succeeded", Provider: "anthropic", Template: "polecat"}),
		startEvent(t, 2, now, workerOperationPayload{Result: "succeeded", Provider: "openai", Template: "manager"}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{Template: "polecat"})
	if len(report.Groups) != 1 || report.Groups[0].Key.Template != "polecat" {
		t.Fatalf("expected filter to keep only polecat group, got %+v", report.Groups)
	}
	// Filter gates both per-group and Total counters: the excluded
	// openai/manager attempt contributes to neither.
	if report.Total.Started != 1 {
		t.Errorf("Total.Started = %d, want 1 (filter applies to total too)", report.Total.Started)
	}
}

func TestAnalyze_SkipsUndecodablePayload(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		{Seq: 1, Type: events.WorkerOperation, Ts: now, Payload: json.RawMessage(`{"operation":`)},
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
}

func TestAnalyze_BucketDefaultsToHourWhenNonPositive(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Result: "succeeded", Provider: "a", Template: "t"}),
	}
	report := Analyze(es, Window{}, 0, Filter{})
	if report.Bucket != time.Hour {
		t.Errorf("Bucket = %v, want 1h default", report.Bucket)
	}
}

func TestGroup_PossibleOutage(t *testing.T) {
	cases := []struct {
		name     string
		g        Group
		expected bool
	}{
		{"all failed tight cluster enough attempts", Group{Started: 3, Failed: 3, MinDurationMs: 100, MaxDurationMs: 150}, true},
		{"all failed but spread out", Group{Started: 3, Failed: 3, MinDurationMs: 50, MaxDurationMs: 5000}, false},
		{"not enough attempts", Group{Started: 2, Failed: 2, MinDurationMs: 100, MaxDurationMs: 100}, false},
		{"mixed results", Group{Started: 3, Failed: 2, Succeeded: 1, MinDurationMs: 100, MaxDurationMs: 100}, false},
		{"all succeeded", Group{Started: 3, Succeeded: 3, MinDurationMs: 100, MaxDurationMs: 100}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.PossibleOutage(); got != tc.expected {
				t.Errorf("PossibleOutage() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestGroup_AvgDurationMs(t *testing.T) {
	now := time.Now().UTC()
	es := []events.Event{
		startEvent(t, 1, now, workerOperationPayload{Result: "succeeded", Provider: "a", Template: "t", DurationMs: 100}),
		startEvent(t, 2, now, workerOperationPayload{Result: "succeeded", Provider: "a", Template: "t", DurationMs: 300}),
	}
	report := Analyze(es, Window{}, time.Hour, Filter{})
	if got := report.Total.AvgDurationMs(); got != 200 {
		t.Errorf("AvgDurationMs() = %v, want 200", got)
	}
}

func TestGroup_MarshalJSON_IncludesComputedFields(t *testing.T) {
	g := Group{Started: 3, Failed: 3, MinDurationMs: 10, MaxDurationMs: 20}
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["possible_outage"] != true {
		t.Errorf("possible_outage = %v, want true", decoded["possible_outage"])
	}
	if _, ok := decoded["avg_duration_ms"]; !ok {
		t.Error("avg_duration_ms missing from JSON output")
	}
}
