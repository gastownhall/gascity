package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// mockStallBeadEvent builds a bead.created/bead.updated/bead.closed event
// carrying a bead snapshot payload, matching beads.DecodeBeadEventPayload's
// expected shape.
func mockStallBeadEvent(t *testing.T, seq uint64, eventType string, ts time.Time, b beads.Bead) events.Event {
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

func TestRunAnalyzeStall_TableOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockStallBeadEvent(t, 1, events.BeadCreated, now.Add(-30*time.Minute), beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-2"}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		cityPath:  dir,
		since:     "30d",
		threshold: "15m",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeStall: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"gcg-1", "polecat", "yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestRunAnalyzeStall_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockStallBeadEvent(t, 1, events.BeadCreated, now.Add(-30*time.Minute), beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-2"}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		cityPath:  dir,
		since:     "30d",
		threshold: "15m",
		jsonOut:   true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeStall: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	entries, _ := parsed["entries"].([]any)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry in JSON, got %d", len(entries))
	}
	if ok, _ := parsed["ok"].(bool); !ok {
		t.Errorf("expected ok=true envelope field, got %+v", parsed["ok"])
	}
	if stalled, _ := parsed["total_stalled"].(float64); stalled != 1 {
		t.Errorf("total_stalled = %v, want 1", stalled)
	}
}

func TestRunAnalyzeStall_PoolFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockStallBeadEvent(t, 1, events.BeadCreated, now.Add(-30*time.Minute), beads.Bead{ID: "gcg-1", Status: "in_progress", Assignee: "polecat-1"}),
		mockStallBeadEvent(t, 2, events.BeadCreated, now.Add(-30*time.Minute), beads.Bead{ID: "gcg-2", Status: "in_progress", Assignee: "mechanic-1"}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		cityPath: dir,
		since:    "30d",
		pool:     "polecat",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeStall: %v", err)
	}

	var parsed struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if len(parsed.Entries) != 1 {
		t.Fatalf("--pool filter: expected 1 entry, got %d", len(parsed.Entries))
	}
}

func TestRunAnalyzeStall_ExplicitEventsPath(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-events.jsonl")
	if err := os.WriteFile(custom, []byte(""), 0o600); err != nil {
		t.Fatalf("write custom events: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		eventPath: custom,
		since:     "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeStall: %v", err)
	}
	if !strings.Contains(stdout.String(), "Bead") {
		t.Errorf("empty events file should still emit header row, got:\n%s", stdout.String())
	}
}

func TestRunAnalyzeStall_MissingEventsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("missing events.jsonl should be benign empty input, got: %v", err)
	}
}

func TestRunAnalyzeStall_MissingExplicitEventsFile(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(dir, "missing-events.jsonl")
	err := runAnalyzeStall(stallCmdOptions{
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

func TestRunAnalyzeStall_BadSinceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
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

func TestRunAnalyzeStall_BadUntilFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
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

func TestRunAnalyzeStall_BadThresholdFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		eventPath: "/dev/null",
		since:     "30d",
		threshold: "not-a-duration",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for malformed --threshold")
	}
	if !strings.Contains(err.Error(), "--threshold") {
		t.Errorf("error should mention --threshold: %v", err)
	}
}

func TestRunAnalyzeStall_NegativeThresholdRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeStall(stallCmdOptions{
		eventPath: "/dev/null",
		since:     "30d",
		threshold: "-5m",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for negative --threshold")
	}
	if !strings.Contains(err.Error(), "--threshold") {
		t.Errorf("error should mention --threshold: %v", err)
	}
}

func TestNewAnalyzeStallCmd_RegistersUnderAnalyze(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAnalyzeCmd(&stdout, &stderr)
	found := false
	for _, c := range cmd.Commands() {
		if c.Use == "stalls" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'stalls' subcommand registered under 'gc analyze'")
	}
}
