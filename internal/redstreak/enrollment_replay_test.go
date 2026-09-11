package redstreak

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureRun mirrors the JSON schema shared by the historical run fixtures
// in testdata/ (mac_regression_27_red_runs.json, nightly_3_red_runs.json).
type fixtureRun struct {
	ID                  int64  `json:"id"`
	StartedAt           string `json:"started_at"`
	Event               string `json:"event"`
	WorkflowConclusion  string `json:"workflow_conclusion"`
	AggregateConclusion string `json:"aggregate_conclusion"`
}

// loadFixtureRuns decodes a testdata run-history fixture, oldest run first.
func loadFixtureRuns(t *testing.T, name string) []fixtureRun {
	t.Helper()
	var fixture struct {
		Runs []fixtureRun `json:"runs"`
	}
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return fixture.Runs
}

// observedRunsUpTo converts the oldest n fixture runs into the newest-first
// []ObservedRun shape EvaluateHistory expects, letting a test replay history
// day by day exactly as a live monitor would have observed it.
func observedRunsUpTo(runs []fixtureRun, n int, aggregateJob string) []ObservedRun {
	observed := make([]ObservedRun, 0, n)
	for i := n - 1; i >= 0; i-- {
		run := runs[i]
		observed = append(observed, ObservedRun{
			Run: Run{
				ID:         run.ID,
				Event:      run.Event,
				Conclusion: run.WorkflowConclusion,
				StartedAt:  mustTimeValue(run.StartedAt),
				URL:        fmt.Sprintf("https://example.test/runs/%d", run.ID),
			},
			Jobs: []Job{{Name: aggregateJob, Conclusion: run.AggregateConclusion}},
		})
	}
	return observed
}

// TestReplayMacRegressionEnrollmentSurfacesByJuly5 replays the captured
// 27-run Mac Regression backfill one run at a time, as the monitor would
// have seen it live, and proves the threshold=3 enrollment would have
// created an episode no later than 2026-07-05 -- not after all 27 runs, as
// the historical backfill in TestEvaluateHistoryReconstructsTwentySevenRunBackfill
// demonstrates happened without this enrollment in place.
func TestReplayMacRegressionEnrollmentSurfacesByJuly5(t *testing.T) {
	runs := loadFixtureRuns(t, "mac_regression_27_red_runs.json")
	monitor := testRunMonitor("mac-regression-red-streak", "Mac regression summary")
	monitor.ActivatedAfter = "2026-07-02T00:00:00Z"

	var surfacedAt string
	for day := 1; day <= len(runs); day++ {
		observed := observedRunsUpTo(runs, day, monitor.AggregateJob)
		eval, err := EvaluateHistory(monitor, observed)
		if err != nil {
			t.Fatalf("EvaluateHistory (day %d): %v", day, err)
		}
		if eval.ConsecutiveFailures >= monitor.ThresholdOrDefault() {
			surfacedAt = runs[day-1].StartedAt
			break
		}
	}

	if surfacedAt == "" {
		t.Fatal("Mac Regression enrollment never crossed threshold across the 27-run backfill")
	}
	surfaced, err := time.Parse(time.RFC3339, surfacedAt)
	if err != nil {
		t.Fatalf("parse surfaced date %q: %v", surfacedAt, err)
	}
	deadline := time.Date(2026, 7, 5, 23, 59, 59, 0, time.UTC)
	if surfaced.After(deadline) {
		t.Fatalf("Mac Regression enrollment first surfaced %s, want no later than 2026-07-05", surfaced.Format(time.RFC3339))
	}
}

// TestReplayNightlyEnrollmentSurfacesGaP658sc replays the 3-run Nightly
// sample captured from ga-p658sc's real GitHub Actions run IDs and proves
// the threshold=3 enrollment would have crossed on exactly that sample,
// surfacing the incident ga-p658sc was filed to track.
func TestReplayNightlyEnrollmentSurfacesGaP658sc(t *testing.T) {
	runs := loadFixtureRuns(t, "nightly_3_red_runs.json")
	monitor := testRunMonitor("nightly-red-streak", "Nightly summary")
	monitor.ActivatedAfter = "2026-07-26T00:00:00Z"

	observed := observedRunsUpTo(runs, len(runs), monitor.AggregateJob)
	eval, err := EvaluateHistory(monitor, observed)
	if err != nil {
		t.Fatalf("EvaluateHistory: %v", err)
	}
	if eval.ConsecutiveFailures < monitor.ThresholdOrDefault() {
		t.Fatalf("ConsecutiveFailures = %d, want >= threshold %d across the ga-p658sc 3-run sample",
			eval.ConsecutiveFailures, monitor.ThresholdOrDefault())
	}
	if eval.LatestRunID != runs[len(runs)-1].ID {
		t.Fatalf("LatestRunID = %d, want the newest ga-p658sc run %d", eval.LatestRunID, runs[len(runs)-1].ID)
	}
}
