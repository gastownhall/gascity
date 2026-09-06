package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func sessionStartEvent(t *testing.T, seq uint64, ts time.Time, operation, result, template, provider string, durationMs int64) events.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"operation":   operation,
		"result":      result,
		"template":    template,
		"provider":    provider,
		"duration_ms": durationMs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return events.Event{Seq: seq, Type: "worker.operation", Ts: ts, Payload: payload}
}

func TestRunAnalyzeSessions_TableOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		sessionStartEvent(t, 1, now, "start", "succeeded", "polecat", "anthropic", 100),
		sessionStartEvent(t, 2, now, "start", "failed", "polecat", "anthropic", 200),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeSessions(sessionsCmdOptions{
		cityPath: dir,
		since:    "30d",
		bucket:   "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeSessions: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"polecat", "anthropic", "Started", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestRunAnalyzeSessions_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		sessionStartEvent(t, 1, now, "start", "succeeded", "polecat", "anthropic", 100),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeSessions(sessionsCmdOptions{
		cityPath: dir,
		since:    "30d",
		bucket:   "1h",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeSessions: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	groups, _ := parsed["groups"].([]any)
	if len(groups) != 1 {
		t.Errorf("expected 1 group in JSON, got %d", len(groups))
	}
	if parsed["ok"] != true {
		t.Errorf("expected ok=true wrapper, got %v", parsed["ok"])
	}
}

func TestRunAnalyzeSessions_TemplateFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		sessionStartEvent(t, 1, now, "start", "succeeded", "polecat", "anthropic", 100),
		sessionStartEvent(t, 2, now, "start", "succeeded", "manager", "openai", 100),
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeSessions(sessionsCmdOptions{
		cityPath: dir,
		since:    "30d",
		bucket:   "1h",
		template: "polecat",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeSessions: %v", err)
	}

	var parsed struct {
		Total map[string]any `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if started, _ := parsed.Total["started"].(float64); started != 1 {
		t.Errorf("--template filter: total started = %v, want 1", started)
	}
}

func TestRunAnalyzeSessions_InvalidBucketRejected(t *testing.T) {
	dir := t.TempDir()
	writeEventsFile(t, dir, nil)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeSessions(sessionsCmdOptions{
		cityPath: dir,
		since:    "30d",
		bucket:   "not-a-duration",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid --bucket")
	}
}

func TestRunAnalyzeSessions_ExplicitEventsPath(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-events.jsonl")
	if err := os.WriteFile(custom, []byte(""), 0o600); err != nil {
		t.Fatalf("write custom events: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runAnalyzeSessions(sessionsCmdOptions{
		eventPath: custom,
		since:     "1h",
		bucket:    "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeSessions: %v", err)
	}
	// Empty file produces a header-only table — must not error.
	if !strings.Contains(stdout.String(), "Bucket") {
		t.Errorf("expected header row, got:\n%s", stdout.String())
	}
}
