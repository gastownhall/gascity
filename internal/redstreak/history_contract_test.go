package redstreak

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestEvaluateHistoryUsesAggregateJobVerdictAndOnlyScheduledRuns(t *testing.T) {
	monitor := testRunMonitor("nightly", "Nightly summary")
	runs := []ObservedRun{
		testObservedRun(104, "schedule", "success", "failure", "2026-07-30T04:00:00Z", monitor.AggregateJob),
		testObservedRun(103, "workflow_dispatch", "failure", "success", "2026-07-30T03:00:00Z", monitor.AggregateJob),
		testObservedRun(102, "pull_request", "failure", "success", "2026-07-30T02:00:00Z", monitor.AggregateJob),
		testObservedRun(101, "schedule", "success", "failure", "2026-07-30T01:00:00Z", monitor.AggregateJob),
	}

	got, err := EvaluateHistory(monitor, runs)
	if err != nil {
		t.Fatalf("EvaluateHistory: %v", err)
	}
	if got.EvaluatedRuns != 2 {
		t.Fatalf("EvaluatedRuns = %d, want 2 schedule-triggered runs", got.EvaluatedRuns)
	}
	if got.LatestRunID != 104 {
		t.Fatalf("LatestRunID = %d, want latest scheduled run 104", got.LatestRunID)
	}
	if got.AggregateConclusion != "failure" {
		t.Fatalf("AggregateConclusion = %q, want aggregate job failure", got.AggregateConclusion)
	}
	if got.ConsecutiveFailures != 2 {
		t.Fatalf("ConsecutiveFailures = %d, want 2; workflow-level success must not mask aggregate failure", got.ConsecutiveFailures)
	}
	if got.ConsecutiveSuccesses != 0 {
		t.Fatalf("ConsecutiveSuccesses = %d, want 0", got.ConsecutiveSuccesses)
	}
}

func TestEvaluateHistoryCountsConsecutiveAggregateConclusions(t *testing.T) {
	monitor := testRunMonitor("mac", "Mac regression summary")
	for _, tc := range []struct {
		name          string
		conclusions   []string
		wantFailures  int
		wantSuccesses int
	}{
		{
			name:         "failure failure failure crosses threshold",
			conclusions:  []string{"failure", "failure", "failure"},
			wantFailures: 3,
		},
		{
			name:         "success resets a prior red streak",
			conclusions:  []string{"failure", "success", "failure"},
			wantFailures: 1,
		},
		{
			name:         "cancelled and skipped are non-success",
			conclusions:  []string{"cancelled", "skipped"},
			wantFailures: 2,
		},
		{
			name:          "three greens are recovery evidence",
			conclusions:   []string{"success", "success", "success"},
			wantSuccesses: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := make([]ObservedRun, 0, len(tc.conclusions))
			for i := len(tc.conclusions) - 1; i >= 0; i-- {
				conclusion := tc.conclusions[i]
				runs = append(runs, testObservedRun(
					int64(200+i),
					"schedule",
					"failure",
					conclusion,
					time.Date(2026, 7, 30, i+1, 0, 0, 0, time.UTC).Format(time.RFC3339),
					monitor.AggregateJob,
				))
			}
			got, err := EvaluateHistory(monitor, runs)
			if err != nil {
				t.Fatalf("EvaluateHistory: %v", err)
			}
			if got.ConsecutiveFailures != tc.wantFailures {
				t.Fatalf("ConsecutiveFailures = %d, want %d", got.ConsecutiveFailures, tc.wantFailures)
			}
			if got.ConsecutiveSuccesses != tc.wantSuccesses {
				t.Fatalf("ConsecutiveSuccesses = %d, want %d", got.ConsecutiveSuccesses, tc.wantSuccesses)
			}
		})
	}
}

func TestEvaluateHistoryReconstructsTwentySevenRunBackfill(t *testing.T) {
	var fixture struct {
		Runs []struct {
			ID                  int64  `json:"id"`
			StartedAt           string `json:"started_at"`
			Event               string `json:"event"`
			WorkflowConclusion  string `json:"workflow_conclusion"`
			AggregateConclusion string `json:"aggregate_conclusion"`
		} `json:"runs"`
	}
	path := filepath.Join("testdata", "mac_regression_27_red_runs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(fixture.Runs) != 27 {
		t.Fatalf("fixture runs = %d, want the observed 27-run historical backfill", len(fixture.Runs))
	}

	monitor := testRunMonitor("mac", "Mac regression summary")
	monitor.ActivatedAfter = "2026-07-03T00:00:00Z"
	runs := make([]ObservedRun, 0, len(fixture.Runs))
	for i := len(fixture.Runs) - 1; i >= 0; i-- {
		run := fixture.Runs[i]
		runs = append(runs, testObservedRun(
			run.ID,
			run.Event,
			run.WorkflowConclusion,
			run.AggregateConclusion,
			run.StartedAt,
			monitor.AggregateJob,
		))
	}
	got, err := EvaluateHistory(monitor, runs)
	if err != nil {
		t.Fatalf("EvaluateHistory: %v", err)
	}
	if got.EvaluatedRuns != 27 || got.ConsecutiveFailures != 27 {
		t.Fatalf("backfill = evaluated %d consecutive failures %d, want 27/27",
			got.EvaluatedRuns, got.ConsecutiveFailures)
	}
	if got.LatestRunID != 27 {
		t.Fatalf("LatestRunID = %d, want synthetic run 27", got.LatestRunID)
	}
}

func TestEvaluateHistoryAppliesActivationBoundaryBeforeAggregateContract(t *testing.T) {
	monitor := testRunMonitor("nightly", "Nightly summary")
	runs := []ObservedRun{
		testObservedRun(302, "schedule", "failure", "success", "2026-07-30T01:00:00Z", monitor.AggregateJob),
		{
			Run: Run{
				ID:        301,
				Event:     "schedule",
				StartedAt: mustTime(t, "2026-07-29T23:59:59Z"),
			},
		},
	}

	got, err := EvaluateHistory(monitor, runs)
	if err != nil {
		t.Fatalf("EvaluateHistory: %v; a pre-activation missing job must be ignored", err)
	}
	if got.EvaluatedRuns != 1 || got.LatestRunID != 302 || got.ConsecutiveSuccesses != 1 {
		t.Fatalf("evaluation = %#v, want only post-activation run 302", got)
	}
}

func TestEvaluateHistoryReportsMissingOrDuplicatePostActivationAggregateJobAsRed(t *testing.T) {
	monitor := testRunMonitor("nightly", "Nightly summary")
	for _, tc := range []struct {
		name    string
		jobs    []Job
		matches int
	}{
		{name: "missing", jobs: nil, matches: 0},
		{
			name: "duplicate",
			jobs: []Job{
				{Name: monitor.AggregateJob, Conclusion: "success"},
				{Name: monitor.AggregateJob, Conclusion: "failure"},
			},
			matches: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := ObservedRun{
				Run: Run{
					ID:         401,
					Event:      "schedule",
					Conclusion: "success",
					StartedAt:  mustTime(t, "2026-07-30T01:00:00Z"),
					URL:        "https://example.test/runs/401",
				},
				Jobs: tc.jobs,
			}
			got, err := EvaluateHistory(monitor, []ObservedRun{run})
			if err == nil {
				t.Fatal("EvaluateHistory() error = nil, want aggregate data-contract error")
			}
			var contractErr *DataContractError
			if !errors.As(err, &contractErr) {
				t.Fatalf("EvaluateHistory() error = %T %v, want *DataContractError", err, err)
			}
			if contractErr.RunID != 401 || contractErr.Matches != tc.matches {
				t.Fatalf("DataContractError = %#v, want run 401 and %d matches", contractErr, tc.matches)
			}
			if !got.DataContractError {
				t.Fatalf("DataContractError = false for %s aggregate job", tc.name)
			}
			if got.ConsecutiveFailures != 1 || got.ConsecutiveSuccesses != 0 {
				t.Fatalf("streak = failures %d successes %d, want 1/0; contract break must never become green",
					got.ConsecutiveFailures, got.ConsecutiveSuccesses)
			}
		})
	}
}

func testRunMonitor(name, aggregate string) config.GitHubRunMonitor {
	return config.GitHubRunMonitor{
		Name:           name,
		Owner:          "gastownhall",
		Repo:           "gascity",
		WorkflowFile:   name + ".yml",
		AggregateJob:   aggregate,
		ActivatedAfter: "2026-07-30T00:00:00Z",
		Rig:            "gascity",
		Route:          "gascity/ci-triage",
	}
}

func testObservedRun(id int64, event, workflowConclusion, aggregateConclusion, startedAt, aggregateName string) ObservedRun {
	return ObservedRun{
		Run: Run{
			ID:         id,
			Event:      event,
			Conclusion: workflowConclusion,
			StartedAt:  mustTimeValue(startedAt),
			URL:        "https://example.test/runs/" + time.Unix(id, 0).UTC().Format("150405"),
		},
		Jobs: []Job{{Name: aggregateName, Conclusion: aggregateConclusion}},
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func mustTimeValue(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
