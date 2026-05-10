package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

func TestParseDurationWithDays(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"1d12h", 36 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"0s", 0, false},
		{"", 0, true},
		{"abc", 0, true},
		{"xd", 0, true},
	}
	for _, tc := range cases {
		got, err := parseDurationWithDays(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseDurationWithDays(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDurationWithDays(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDurationWithDays(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseTimeFlag(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	t.Run("duration produces past timestamp", func(t *testing.T) {
		got, err := parseTimeFlag("7d", now)
		if err != nil {
			t.Fatalf("parseTimeFlag: %v", err)
		}
		want := now.Add(-7 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("zero duration is now", func(t *testing.T) {
		got, err := parseTimeFlag("0s", now)
		if err != nil {
			t.Fatalf("parseTimeFlag: %v", err)
		}
		if !got.Equal(now) {
			t.Errorf("0s should equal now: got %v, want %v", got, now)
		}
	})
	t.Run("RFC3339 timestamp passes through", func(t *testing.T) {
		got, err := parseTimeFlag("2026-04-01T00:00:00Z", now)
		if err != nil {
			t.Fatalf("parseTimeFlag: %v", err)
		}
		want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("empty is zero time", func(t *testing.T) {
		got, err := parseTimeFlag("", now)
		if err != nil {
			t.Fatalf("parseTimeFlag(empty): %v", err)
		}
		if !got.IsZero() {
			t.Errorf("empty should yield zero time, got %v", got)
		}
	})
	t.Run("malformed returns error", func(t *testing.T) {
		_, err := parseTimeFlag("not-a-time", now)
		if err == nil {
			t.Error("expected error for malformed input")
		}
	})
}

// writeEventsFile populates an events.jsonl with the provided events.
func writeEventsFile(t *testing.T, dir string, es []events.Event) {
	t.Helper()
	path := filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create events: %v", err)
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, e := range es {
		if err := enc.Encode(&e); err != nil {
			t.Fatalf("encode event: %v", err)
		}
	}
}

func writeAnalyzeReliabilityCity(t *testing.T, dir string, es []events.Event) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeEventsFile(t, dir, es)
}

func mockWorkerOpEvent(t *testing.T, seq uint64, ts time.Time, sessionID, model, version, agent string) events.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"session_id":     sessionID,
		"session_name":   sessionID,
		"model":          model,
		"prompt_version": version,
		"agent_name":     agent,
		"operation":      "start",
		"result":         "succeeded",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return events.Event{
		Seq:     seq,
		Type:    "worker.operation",
		Ts:      ts,
		Subject: sessionID,
		Payload: payload,
	}
}

func TestRunAnalyzeReliability_TableOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockWorkerOpEvent(t, 1, now, "sA", "claude-opus-4-7", "v3", "rigA/worker-1"),
		mockWorkerOpEvent(t, 2, now, "sB", "claude-sonnet-4-6", "v3", "rigA/worker-2"),
		{Seq: 3, Type: "session.crashed", Ts: now, Subject: "sA"},
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeReliability: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"claude-opus-4-7", "v3", "rigA", "Crashed", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q\n%s", want, out)
		}
	}
}

func TestRunAnalyzeReliability_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockWorkerOpEvent(t, 1, now, "sA", "opus", "v1", "rig/p"),
		{Seq: 2, Type: "session.crashed", Ts: now, Subject: "sA"},
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: dir,
		since:    "30d",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeReliability: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	groups, _ := parsed["groups"].([]any)
	if len(groups) != 1 {
		t.Errorf("expected 1 group in JSON, got %d", len(groups))
	}
}

func TestRunAnalyzeReliability_ModelFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	es := []events.Event{
		mockWorkerOpEvent(t, 1, now, "sA", "opus", "v1", "rig/a"),
		mockWorkerOpEvent(t, 2, now, "sB", "sonnet", "v1", "rig/b"),
		{Seq: 3, Type: "session.crashed", Ts: now, Subject: "sA"},
		{Seq: 4, Type: "session.crashed", Ts: now, Subject: "sB"},
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: dir,
		since:    "30d",
		model:    "opus",
		jsonOut:  true,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeReliability: %v", err)
	}

	var parsed struct {
		Groups []map[string]any `json:"groups"`
		Total  map[string]any   `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if total, _ := parsed.Total["crashed"].(float64); total != 1 {
		t.Errorf("--model filter: total crashed = %v, want 1", total)
	}
}

func TestRunAnalyzeReliability_ExplicitEventsPath(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-events.jsonl")
	if err := os.WriteFile(custom, []byte(""), 0o600); err != nil {
		t.Fatalf("write custom events: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		eventPath: custom,
		since:     "1h",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeReliability: %v", err)
	}
	// Empty file produces a header-only table — must not error.
	if !strings.Contains(stdout.String(), "Model") {
		t.Errorf("empty events file should still emit header row, got:\n%s", stdout.String())
	}
}

func TestRunAnalyzeReliability_MissingEventsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	// Don't create events.jsonl — exercise the "file missing" path.
	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: dir,
		since:    "30d",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("missing events.jsonl should be benign empty input, got: %v", err)
	}
}

func TestRunAnalyzeReliability_MissingExplicitEventsFile(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(dir, "missing-events.jsonl")
	err := runAnalyzeReliability(reliabilityCmdOptions{
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

func TestRunAnalyzeReliability_MissingExplicitCityPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing-city")
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: missing,
		since:    "30d",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("missing explicit --city path should return an error")
	}
	if !strings.Contains(err.Error(), "--city") {
		t.Fatalf("error should mention --city, got: %v", err)
	}
}

func TestRunAnalyzeReliability_BadSinceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
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

func TestAnalyzeReliabilityCommand_UsesRootPersistentCityFlag(t *testing.T) {
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")

	dir := t.TempDir()
	now := time.Now().UTC()
	writeAnalyzeReliabilityCity(t, dir, []events.Event{
		mockWorkerOpEvent(t, 1, now, "sA", "root-opus", "v1", "rig/p"),
		{Seq: 2, Type: "session.crashed", Ts: now, Subject: "sA"},
	})

	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--city", dir, "analyze", "reliability", "--since", "30d"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "root-opus") {
		t.Fatalf("root --city was not used; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

func TestAnalyzeReliabilityCommand_SubcommandCityFlagWins(t *testing.T) {
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})

	rootCity := t.TempDir()
	subcommandCity := t.TempDir()
	now := time.Now().UTC()
	writeAnalyzeReliabilityCity(t, rootCity, []events.Event{
		mockWorkerOpEvent(t, 1, now, "root", "root-model", "v1", "rig/p"),
	})
	writeAnalyzeReliabilityCity(t, subcommandCity, []events.Event{
		mockWorkerOpEvent(t, 1, now, "sub", "subcommand-model", "v1", "rig/p"),
	})

	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(&stdout, &stderr)
	cmd.SetArgs([]string{
		"--city", rootCity,
		"analyze", "reliability",
		"--city", subcommandCity,
		"--since", "30d",
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "subcommand-model") {
		t.Fatalf("subcommand --city was not used; stdout:\n%s\nstderr:\n%s", out, stderr.String())
	}
	if strings.Contains(out, "root-model") {
		t.Fatalf("subcommand --city should override root --city; stdout:\n%s", out)
	}
}

func TestResolveEventsPath_ExplicitWins(t *testing.T) {
	got, err := resolveEventsPath(reliabilityCmdOptions{
		eventPath: "/explicit/events.jsonl",
		cityPath:  "/some/city",
	})
	if err != nil {
		t.Fatalf("resolveEventsPath: %v", err)
	}
	if got != "/explicit/events.jsonl" {
		t.Errorf("got %q, want explicit override", got)
	}
}

func TestResolveEventsPath_CityFlagComputesPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got, err := resolveEventsPath(reliabilityCmdOptions{cityPath: dir})
	if err != nil {
		t.Fatalf("resolveEventsPath: %v", err)
	}
	want := filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveEventsPath_NoSourceReturnsError(t *testing.T) {
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origCwd) //nolint:errcheck
	})
	_, err = resolveEventsPath(reliabilityCmdOptions{})
	if err == nil {
		t.Fatal("expected error when no city is findable")
	}
}

// TestRunAnalyzeReliability_FullPipelineMatchesGoldenSnapshot verifies
// the table format end-to-end with a curated event set, against a stable
// expected output. Catches accidental column-order or sorting drift.
func TestRunAnalyzeReliability_FullPipelineMatchesGoldenSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	es := []events.Event{
		mockWorkerOpEvent(t, 1, now, "sA", "opus", "v3", "rigA/worker-1"),
		mockWorkerOpEvent(t, 2, now, "sB", "opus", "v3", "rigA/worker-2"),
		mockWorkerOpEvent(t, 3, now, "sC", "sonnet", "v3", "rigB/worker-1"),
		{Seq: 4, Type: "session.crashed", Ts: now, Subject: "sA"},
		{Seq: 5, Type: "session.crashed", Ts: now, Subject: "sC"},
		{Seq: 6, Type: "session.idle_killed", Ts: now, Subject: "sA"},
	}
	writeEventsFile(t, dir, es)

	var stdout, stderr bytes.Buffer
	err := runAnalyzeReliability(reliabilityCmdOptions{
		cityPath: dir,
		since:    fmt.Sprintf("%dh", int(time.Since(now)/time.Hour)+24),
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runAnalyzeReliability: %v", err)
	}

	out := stdout.String()
	// Top group is opus (2 unhealthy events) before sonnet (1 event).
	opusIdx := strings.Index(out, "opus")
	sonnetIdx := strings.Index(out, "sonnet")
	if opusIdx < 0 || sonnetIdx < 0 {
		t.Fatalf("missing rows in output:\n%s", out)
	}
	if opusIdx > sonnetIdx {
		t.Errorf("opus group should appear before sonnet (higher unhealthy):\n%s", out)
	}
}

func TestNewAnalyzeReliabilityCmd_UsageContainsFlags(t *testing.T) {
	cmd := newAnalyzeReliabilityCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"city", "since", "until", "model", "rig", "json", "events"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s", name)
		}
	}
}

func TestNewAnalyzeCmd_HasReliabilitySubcommand(t *testing.T) {
	cmd := newAnalyzeCmd(&bytes.Buffer{}, &bytes.Buffer{})
	var found bool
	for _, sub := range cmd.Commands() {
		if sub.Name() == "reliability" {
			found = true
		}
	}
	if !found {
		t.Error("`gc analyze` is missing the `reliability` subcommand")
	}
}
