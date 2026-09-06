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

// mockBeadEvent builds a bead.created/bead.closed event carrying a bead
// snapshot payload, matching beads.DecodeBeadEventPayload's expected shape.
func mockBeadEvent(t *testing.T, seq uint64, eventType string, ts time.Time, b beads.Bead) events.Event {
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

func TestRunAnalyzeBeads_TableOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"urgent"}}),
		mockBeadEvent(t, 2, events.BeadClosed, now, beads.Bead{ID: "gcg-1", Type: "task", Labels: []string{"urgent"}}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeBeads: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"gcg", "task", "urgent", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestRunAnalyzeBeads_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task"}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
		cityPath: dir,
		since:    "30d",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeBeads: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	groups, _ := parsed["groups"].([]any)
	if len(groups) != 1 {
		t.Errorf("expected 1 group in JSON, got %d", len(groups))
	}
	if ok, _ := parsed["ok"].(bool); !ok {
		t.Errorf("expected ok=true envelope field, got %+v", parsed["ok"])
	}
}

func TestRunAnalyzeBeads_StoreFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockBeadEvent(t, 1, events.BeadCreated, now, beads.Bead{ID: "gcg-1", Type: "task"}),
		mockBeadEvent(t, 2, events.BeadCreated, now, beads.Bead{ID: "gcm-1", Type: "task"}),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
		cityPath: dir,
		since:    "30d",
		store:    "gcg",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeBeads: %v", err)
	}

	var parsed struct {
		Groups []map[string]any `json:"groups"`
		Total  map[string]any   `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if len(parsed.Groups) != 1 {
		t.Fatalf("--store filter: expected 1 group, got %d", len(parsed.Groups))
	}
	if opened, _ := parsed.Total["opened"].(float64); opened != 1 {
		t.Errorf("--store filter: total opened = %v, want 1", opened)
	}
}

func TestRunAnalyzeBeads_ExplicitEventsPath(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-events.jsonl")
	if err := os.WriteFile(custom, []byte(""), 0o600); err != nil {
		t.Fatalf("write custom events: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
		eventPath: custom,
		since:     "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeBeads: %v", err)
	}
	if !strings.Contains(stdout.String(), "Store") {
		t.Errorf("empty events file should still emit header row, got:\n%s", stdout.String())
	}
}

func TestRunAnalyzeBeads_MissingEventsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("missing events.jsonl should be benign empty input, got: %v", err)
	}
}

func TestRunAnalyzeBeads_MissingExplicitEventsFile(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(dir, "missing-events.jsonl")
	err := runAnalyzeBeads(beadsCmdOptions{
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

func TestRunAnalyzeBeads_BadSinceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
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

func TestRunAnalyzeBeads_BadUntilFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeBeads(beadsCmdOptions{
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

func TestNewAnalyzeBeadsCmd_RegistersUnderAnalyze(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAnalyzeCmd(&stdout, &stderr)
	found := false
	for _, c := range cmd.Commands() {
		if c.Use == "beads" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'beads' subcommand registered under 'gc analyze'")
	}
}
