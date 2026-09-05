package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// mockLatencyBeadEvent builds a bead.created/bead.updated event carrying a
// bead snapshot payload, matching beads.DecodeBeadEventPayload's expected
// shape.
func mockLatencyBeadEvent(t *testing.T, seq uint64, eventType string, ts time.Time, b beads.Bead) events.Event {
	t.Helper()
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: b.ID,
		Payload: payload,
	}
}

func mockStepEvent(seq uint64, eventType string, ts time.Time, subject, runID, stepID string) events.Event {
	return events.Event{
		Seq:     seq,
		Type:    eventType,
		Ts:      ts,
		Subject: subject,
		RunID:   runID,
		StepID:  stepID,
	}
}

func TestRunAnalyzeLatency_TableOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockLatencyBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker-pool"},
		}),
		mockLatencyBeadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker-pool"},
		}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeLatency: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"Claim wait per pool", "worker-pool", "Gate queue wait", "Gate bounce rate"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestRunAnalyzeLatency_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockLatencyBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker-pool"},
		}),
		mockLatencyBeadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker-pool"},
		}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		cityPath: dir,
		since:    "30d",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeLatency: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	claimWait, _ := parsed["claim_wait"].([]any)
	if len(claimWait) != 1 {
		t.Errorf("expected 1 claim_wait group in JSON, got %d", len(claimWait))
	}
	if ok, _ := parsed["ok"].(bool); !ok {
		t.Errorf("expected ok=true envelope field, got %+v", parsed["ok"])
	}
}

func TestRunAnalyzeLatency_PoolFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockLatencyBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID: "b-1", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		mockLatencyBeadEvent(t, 2, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-1", Status: "in_progress", Assignee: "slot-1",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-a"},
		}),
		mockLatencyBeadEvent(t, 3, events.BeadCreated, now, beads.Bead{
			ID: "b-2", Status: "open",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
		mockLatencyBeadEvent(t, 4, events.BeadUpdated, now.Add(time.Minute), beads.Bead{
			ID: "b-2", Status: "in_progress", Assignee: "slot-2",
			Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "pool-b"},
		}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		cityPath: dir,
		since:    "30d",
		pool:     "pool-a",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeLatency: %v", err)
	}

	var parsed struct {
		ClaimWait []map[string]any `json:"claim_wait"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if len(parsed.ClaimWait) != 1 {
		t.Fatalf("--pool filter: expected 1 group, got %d", len(parsed.ClaimWait))
	}
	if pool, _ := parsed.ClaimWait[0]["pool"].(string); pool != "pool-a" {
		t.Errorf("--pool filter: got pool %q, want pool-a", pool)
	}
}

func TestRunAnalyzeLatency_FormulaFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockLatencyBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{
			ID:       "root-1",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-a"},
		}),
		mockLatencyBeadEvent(t, 2, events.BeadCreated, now, beads.Bead{
			ID:       "root-2",
			Metadata: beads.StringMap{beadmeta.FormulaNameMetadataKey: "mol-b"},
		}),
		mockStepEvent(3, events.ExecutionStepDefined, now, "step-1", "root-1", "check"),
		mockStepEvent(4, events.ExecutionStepDefined, now, "step-2", "root-2", "check"),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		cityPath: dir,
		since:    "30d",
		formula:  "mol-a",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeLatency: %v", err)
	}

	var parsed struct {
		GateBounce []map[string]any `json:"gate_bounce"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if len(parsed.GateBounce) != 1 {
		t.Fatalf("--formula filter: expected 1 group, got %d", len(parsed.GateBounce))
	}
	if formula, _ := parsed.GateBounce[0]["formula"].(string); formula != "mol-a" {
		t.Errorf("--formula filter: got formula %q, want mol-a", formula)
	}
}

func TestRunAnalyzeLatency_ExplicitEventsPath(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-events.jsonl")
	if err := os.WriteFile(custom, []byte(""), 0o600); err != nil {
		t.Fatalf("write custom events: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		eventPath: custom,
		since:     "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeLatency: %v", err)
	}
	if !strings.Contains(stdout.String(), "Claim wait per pool") {
		t.Errorf("empty events file should still emit section headers, got:\n%s", stdout.String())
	}
}

func TestRunAnalyzeLatency_MissingEventsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("missing events.jsonl should be benign empty input, got: %v", err)
	}
}

func TestRunAnalyzeLatency_MissingExplicitEventsFile(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(dir, "missing-events.jsonl")
	err := runAnalyzeLatency(latencyCmdOptions{
		eventPath: missing,
		since:     "30d",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("missing explicit --events path should return an error")
	}
	if !strings.Contains(err.Error(), "--events") {
		t.Fatalf("error should mention --events, got: %v", err)
	}
}

func TestRunAnalyzeLatency_BadSinceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		eventPath: "/dev/null",
		since:     "yesterday",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for malformed --since")
	}
	if !strings.Contains(err.Error(), "--since") {
		t.Errorf("error should mention --since: %v", err)
	}
}

func TestRunAnalyzeLatency_BadUntilFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeLatency(latencyCmdOptions{
		eventPath: "/dev/null",
		since:     "30d",
		until:     "not-a-time",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for malformed --until")
	}
	if !strings.Contains(err.Error(), "--until") {
		t.Errorf("error should mention --until: %v", err)
	}
}

func TestNewAnalyzeLatencyCmd_RegistersUnderAnalyze(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAnalyzeCmd(&stdout, &stderr)
	found := false
	for _, c := range cmd.Commands() {
		if c.Use == "latency" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'latency' subcommand registered under 'gc analyze'")
	}
}
