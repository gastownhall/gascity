package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComputePerfStats verifies the statistics calculation over a known set
// of wall-clock samples.
func TestComputePerfStats(t *testing.T) {
	t.Parallel()
	walls := []int64{100, 200, 300, 400, 500}
	s := computePerfStats(walls)

	if s.MinMs != 100 {
		t.Errorf("MinMs = %d, want 100", s.MinMs)
	}
	if s.MaxMs != 500 {
		t.Errorf("MaxMs = %d, want 500", s.MaxMs)
	}
	if s.MeanMs != 300 {
		t.Errorf("MeanMs = %d, want 300", s.MeanMs)
	}
	if s.P50Ms != 300 {
		t.Errorf("P50Ms = %d, want 300", s.P50Ms)
	}
	if s.P95Ms != 500 {
		t.Errorf("P95Ms = %d, want 500", s.P95Ms)
	}
}

// TestComputePerfStats_Empty ensures no panic on empty input.
func TestComputePerfStats_Empty(t *testing.T) {
	t.Parallel()
	s := computePerfStats(nil)
	if s.MinMs != 0 || s.MaxMs != 0 || s.MeanMs != 0 {
		t.Errorf("empty stats should be zero, got %+v", s)
	}
}

// TestComputePerfStats_Single verifies a single-sample edge case.
func TestComputePerfStats_Single(t *testing.T) {
	t.Parallel()
	s := computePerfStats([]int64{42})
	if s.MinMs != 42 || s.MaxMs != 42 || s.MeanMs != 42 || s.P50Ms != 42 || s.P95Ms != 42 {
		t.Errorf("single-sample stats wrong: %+v", s)
	}
}

// TestParseLifecycleSteps_Present verifies extraction of phases from a
// realistic lifecycle log line.
func TestParseLifecycleSteps_Present(t *testing.T) {
	t.Parallel()
	stderr := "session lifecycle: op=start wave=1 session=gc-x template=worker outcome=started duration=234ms phases=[start_call=50ms post_start_observe=70ms]"
	steps := parseLifecycleSteps(stderr)
	if steps == nil {
		t.Fatal("parseLifecycleSteps returned nil, want map")
	}
	if steps["start_call"] != 50 {
		t.Errorf("start_call = %d, want 50", steps["start_call"])
	}
	if steps["post_start_observe"] != 70 {
		t.Errorf("post_start_observe = %d, want 70", steps["post_start_observe"])
	}
}

// TestParseLifecycleSteps_Absent confirms nil is returned when no phases block
// is present (e.g. controller-managed path that defers to reconciler).
func TestParseLifecycleSteps_Absent(t *testing.T) {
	t.Parallel()
	stderr := "session lifecycle: op=start wave=1 session=gc-x template=worker outcome=started duration=5ms"
	if got := parseLifecycleSteps(stderr); got != nil {
		t.Errorf("parseLifecycleSteps = %v, want nil when no phases block", got)
	}
}

// TestParseLifecycleSteps_Empty confirms nil is returned for empty input.
func TestParseLifecycleSteps_Empty(t *testing.T) {
	t.Parallel()
	if got := parseLifecycleSteps(""); got != nil {
		t.Errorf("parseLifecycleSteps('') = %v, want nil", got)
	}
}

// TestSetupPerfCity_Layout verifies that setupPerfCity creates the expected
// directory structure and file contents.
func TestSetupPerfCity_Layout(t *testing.T) {
	t.Parallel()
	cityPath, cleanup, err := setupPerfCity("perf-worker")
	if err != nil {
		t.Fatalf("setupPerfCity: %v", err)
	}
	defer cleanup()

	// city.toml must exist and reference the file beads provider.
	cityTOML, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	if !strings.Contains(string(cityTOML), `provider = "file"`) {
		t.Errorf("city.toml missing file provider:\n%s", cityTOML)
	}

	// pack.toml must declare the named session.
	packTOML, err := os.ReadFile(filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	if !strings.Contains(string(packTOML), "perf-worker") {
		t.Errorf("pack.toml does not mention perf-worker:\n%s", packTOML)
	}

	// Agent config must exist.
	agentTOML, err := os.ReadFile(filepath.Join(cityPath, "agents", "perf-worker", "agent.toml"))
	if err != nil {
		t.Fatalf("reading agent.toml: %v", err)
	}
	if !strings.Contains(string(agentTOML), "start_command") {
		t.Errorf("agent.toml missing start_command:\n%s", agentTOML)
	}

	// .gc/site.toml must exist.
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "site.toml")); err != nil {
		t.Errorf(".gc/site.toml missing: %v", err)
	}
}

// TestSetupPerfCity_Cleanup verifies that the cleanup function removes the city.
func TestSetupPerfCity_Cleanup(t *testing.T) {
	t.Parallel()
	cityPath, cleanup, err := setupPerfCity("perf-worker")
	if err != nil {
		t.Fatalf("setupPerfCity: %v", err)
	}
	cleanup()
	if _, err := os.Stat(cityPath); !os.IsNotExist(err) {
		t.Errorf("city dir still exists after cleanup: %s", cityPath)
	}
}

// TestPrintPerfReport_NoSteps verifies the table output shape when no step
// timing data is present.
func TestPrintPerfReport_NoSteps(t *testing.T) {
	t.Parallel()
	r := perfReport{
		Scenario:   "session-new",
		Iterations: 3,
		Results: []perfIterResult{
			{Iter: 1, WallMs: 100},
			{Iter: 2, WallMs: 200},
			{Iter: 3, WallMs: 300},
		},
		Stats: computePerfStats([]int64{100, 200, 300}),
	}
	var buf bytes.Buffer
	printPerfReport(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "session-new") {
		t.Errorf("report missing scenario name:\n%s", out)
	}
	if !strings.Contains(out, "3 iterations") {
		t.Errorf("report missing iteration count:\n%s", out)
	}
	if !strings.Contains(out, "Wall(ms)") {
		t.Errorf("report missing Wall(ms) header:\n%s", out)
	}
	if !strings.Contains(out, "min=100ms") {
		t.Errorf("report missing min stat:\n%s", out)
	}
}

// TestPrintPerfReport_WithSteps verifies that step column headers appear when
// at least one iteration has step timing data.
func TestPrintPerfReport_WithSteps(t *testing.T) {
	t.Parallel()
	r := perfReport{
		Scenario:   "session-new",
		Iterations: 2,
		Results: []perfIterResult{
			{Iter: 1, WallMs: 100, Steps: map[string]int64{"start_call": 50, "post_start_observe": 30}},
			{Iter: 2, WallMs: 120, Steps: map[string]int64{"start_call": 55, "post_start_observe": 35}},
		},
		Stats: computePerfStats([]int64{100, 120}),
	}
	var buf bytes.Buffer
	printPerfReport(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "post_start_observe") {
		t.Errorf("report missing step column:\n%s", out)
	}
	if !strings.Contains(out, "start_call") {
		t.Errorf("report missing step column:\n%s", out)
	}
}

// TestPrintPerfReport_JSONShape verifies that JSON output contains the
// expected top-level fields.
func TestPrintPerfReport_JSONShape(t *testing.T) {
	t.Parallel()
	r := perfReport{
		Scenario:   "run[status]",
		Iterations: 2,
		Results: []perfIterResult{
			{Iter: 1, WallMs: 80},
			{Iter: 2, WallMs: 90},
		},
		Stats: computePerfStats([]int64{80, 90}),
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{"scenario", "iterations", "results", "stats"} {
		if _, ok := m[field]; !ok {
			t.Errorf("JSON missing field %q", field)
		}
	}
	stats, _ := m["stats"].(map[string]any)
	for _, field := range []string{"min_ms", "mean_ms", "p50_ms", "p95_ms", "max_ms"} {
		if _, ok := stats[field]; !ok {
			t.Errorf("stats JSON missing field %q", field)
		}
	}
}

// TestPerfRunParsesFlags_Defaults exercises the cobra binding for gc perf run
// by checking that the default options are in place.
func TestPerfRunParsesFlags_Defaults(t *testing.T) {
	t.Parallel()
	var captured perfCmdOptions
	cmd := newPerfRunCmd(nil, nil)
	// Reach the underlying flags without executing.
	iter, _ := cmd.Flags().GetInt("iter")
	warmup, _ := cmd.Flags().GetInt("warmup")
	jsonOut, _ := cmd.Flags().GetBool("json")
	captured = perfCmdOptions{iter: iter, warmup: warmup, jsonOut: jsonOut}

	if captured.iter != 5 {
		t.Errorf("default iter = %d, want 5", captured.iter)
	}
	if captured.warmup != 1 {
		t.Errorf("default warmup = %d, want 1", captured.warmup)
	}
	if captured.jsonOut {
		t.Error("default jsonOut = true, want false")
	}
}

// TestPerfSessionNewParsesFlags_Defaults exercises the cobra binding for
// gc perf session-new.
func TestPerfSessionNewParsesFlags_Defaults(t *testing.T) {
	t.Parallel()
	cmd := newPerfSessionNewCmd(nil, nil)
	iter, _ := cmd.Flags().GetInt("iter")
	warmup, _ := cmd.Flags().GetInt("warmup")
	template, _ := cmd.Flags().GetString("template")

	if iter != 5 {
		t.Errorf("default iter = %d, want 5", iter)
	}
	if warmup != 1 {
		t.Errorf("default warmup = %d, want 1", warmup)
	}
	if template != "perf-worker" {
		t.Errorf("default template = %q, want perf-worker", template)
	}
}

// TestPerfCmd_IsHidden confirms that the gc perf command is hidden from --help.
func TestPerfCmd_IsHidden(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	cmd := newPerfCmd(&stdout, &stderr)
	if !cmd.Hidden {
		t.Error("gc perf command should be Hidden = true")
	}
}
