package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/redstreak"
)

type fakeGitHubRunEvaluator struct {
	evaluations map[string]redstreak.Evaluation
	errs        map[string]error
	calls       []string
}

func (f *fakeGitHubRunEvaluator) Evaluate(_ context.Context, monitor config.GitHubRunMonitor) (redstreak.Evaluation, error) {
	f.calls = append(f.calls, monitor.Name)
	return f.evaluations[monitor.Name], f.errs[monitor.Name]
}

type githubRunsContractHarness struct {
	cityPath string
	mem      *beads.MemStore
	store    *beadstest.RecordingStore
	client   *fakeGitHubRunEvaluator
}

func TestGitHubRunsEvaluateKeepsWorkflowEpisodesIsolated(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["mac-regression-red-streak"] = testRedStreakEvaluation(
		"mac-regression-red-streak", 601, "failure", 3, 0,
	)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 701, "failure", 3, 0,
	)

	runGitHubRunsEvaluate(t, h, 0)

	if got, want := h.client.calls, []string{"mac-regression-red-streak", "nightly-red-streak"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("evaluated monitors = %v, want %v", got, want)
	}
	mac := requireSingleEpisode(t, h.store, "mac-regression-red-streak")
	nightly := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if mac.ID == nightly.ID {
		t.Fatalf("Mac Regression and Nightly share incident %s, want isolated episodes", mac.ID)
	}
	if got := mac.Metadata["redstreak.workflow_file"]; got != "mac-regression.yml" {
		t.Fatalf("Mac workflow_file = %q, want mac-regression.yml", got)
	}
	if got := nightly.Metadata["redstreak.workflow_file"]; got != "nightly.yml" {
		t.Fatalf("Nightly workflow_file = %q, want nightly.yml", got)
	}
	if got := mac.Metadata["gc.routed_to"]; got != "gascity/mac-triage" {
		t.Fatalf("Mac gc.routed_to = %q, want gascity/mac-triage", got)
	}
	if got := nightly.Metadata["gc.routed_to"]; got != "gascity/nightly-triage" {
		t.Fatalf("Nightly gc.routed_to = %q, want gascity/nightly-triage", got)
	}
}

func TestGitHubRunsEvaluateCreatesNothingBeforeThresholdOrForGreenHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		evaluation redstreak.Evaluation
	}{
		{
			name:       "two consecutive reds",
			evaluation: testRedStreakEvaluation("nightly-red-streak", 751, "failure", 2, 0),
		},
		{
			name:       "green without active incident",
			evaluation: testRedStreakEvaluation("nightly-red-streak", 752, "success", 0, 3),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGitHubRunsContractHarness(t)
			h.client.evaluations["nightly-red-streak"] = tc.evaluation
			runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
			if calls := h.store.Calls(); len(calls) != 0 {
				t.Fatalf("below-threshold/green evaluation mutations = %#v, want none", calls)
			}
			if episodes := requireEpisodes(t, h.store, "nightly-red-streak", true); len(episodes) != 0 {
				t.Fatalf("episodes = %#v, want none", episodes)
			}
		})
	}
}

func TestGitHubRunsEvaluateIsIdempotentForSameRunAndUpdatesForNewRedRun(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 801, "failure", 3, 0,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	first := requireSingleEpisode(t, h.store, "nightly-red-streak")

	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	if calls := h.store.Calls(); len(calls) != 0 {
		t.Fatalf("same completed run produced duplicate mutation(s): %#v", calls)
	}
	same := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if same.ID != first.ID {
		t.Fatalf("same run changed episode from %s to %s", first.ID, same.ID)
	}

	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 802, "cancelled", 4, 0,
	)
	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	if calls := h.store.Calls(); len(calls) != 1 || calls[0].Op != "Update" || calls[0].ID != first.ID {
		t.Fatalf("new red run mutations = %#v, want one update of %s", calls, first.ID)
	}
	updated := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if updated.ID != first.ID {
		t.Fatalf("new red run created episode %s, want update of %s", updated.ID, first.ID)
	}
	if got := updated.Metadata["redstreak.run_id"]; got != "802" {
		t.Fatalf("redstreak.run_id = %q, want 802", got)
	}
	if got := updated.Metadata["redstreak.consecutive_count"]; got != "4" {
		t.Fatalf("redstreak.consecutive_count = %q, want 4", got)
	}
	if got := updated.Metadata["redstreak.aggregate_conclusion"]; got != "cancelled" {
		t.Fatalf("redstreak.aggregate_conclusion = %q, want cancelled", got)
	}
}

func TestGitHubRunsEvaluateRecordsRecoveryWithoutAutoClosing(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 901, "failure", 3, 0,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	active := requireSingleEpisode(t, h.store, "nightly-red-streak")

	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 902, "success", 0, 1,
	)
	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	recovering := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if recovering.ID != active.ID || recovering.Status == "closed" {
		t.Fatalf("first green episode = %#v, want open episode %s", recovering, active.ID)
	}
	if recovering.Metadata["redstreak.recovery_seen_at"] == "" {
		t.Fatal("first green did not record redstreak.recovery_seen_at")
	}
	if recovering.Metadata["redstreak.recovery_ready"] == "true" {
		t.Fatal("one green marked recovery ready; want three consecutive greens")
	}
	if calls := h.store.CallsForOp("Close"); len(calls) != 0 {
		t.Fatalf("first green auto-closed incident: %#v", calls)
	}

	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 904, "success", 0, 3,
	)
	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	recovered := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if recovered.Status == "closed" {
		t.Fatal("three greens auto-closed incident; owner review is required")
	}
	if got := recovered.Metadata["redstreak.recovery_ready"]; got != "true" {
		t.Fatalf("redstreak.recovery_ready = %q, want true after three greens", got)
	}
	if calls := h.store.CallsForOp("Close"); len(calls) != 0 {
		t.Fatalf("three greens auto-closed incident: %#v", calls)
	}
}

func TestGitHubRunsEvaluateCreatesNewEpisodeAfterManualCloseWhileStillRed(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 1001, "failure", 3, 0,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	first := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if err := h.mem.Close(first.ID); err != nil {
		t.Fatalf("manually close %s: %v", first.ID, err)
	}

	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	episodes := requireEpisodes(t, h.store, "nightly-red-streak", true)
	assertOneClosedOneOpenEpisode(t, episodes, first.ID)
	if creates := h.store.CallsForOp("Create"); len(creates) != 1 {
		t.Fatalf("still-red evaluation create calls = %#v, want one new episode", creates)
	}
}

func TestGitHubRunsEvaluateCreatesNewEpisodeAfterRecoveredIncidentWasClosed(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 1101, "failure", 3, 0,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	first := requireSingleEpisode(t, h.store, "nightly-red-streak")

	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 1104, "success", 0, 3,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	recovered := requireSingleEpisode(t, h.store, "nightly-red-streak")
	if recovered.Metadata["redstreak.recovery_ready"] != "true" {
		t.Fatalf("episode metadata = %#v, want recovery-ready before legitimate close", recovered.Metadata)
	}
	if err := h.mem.Close(first.ID); err != nil {
		t.Fatalf("close recovered episode %s: %v", first.ID, err)
	}

	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 1107, "failure", 3, 0,
	)
	h.store.Reset()
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
	episodes := requireEpisodes(t, h.store, "nightly-red-streak", true)
	assertOneClosedOneOpenEpisode(t, episodes, first.ID)
	if creates := h.store.CallsForOp("Create"); len(creates) != 1 {
		t.Fatalf("recurrence create calls = %#v, want one new episode", creates)
	}
}

func TestGitHubRunsEvaluateLeavesExistingEpisodeUntouchedOnObservationFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "malformed API data", err: errors.New("decode workflow runs: invalid character")},
		{name: "GitHub command failure", err: errors.New("GitHub API returned 502")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGitHubRunsContractHarness(t)
			h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
				"nightly-red-streak", 1201, "failure", 3, 0,
			)
			runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
			before := requireSingleEpisode(t, h.store, "nightly-red-streak")

			h.client.errs["nightly-red-streak"] = tc.err
			h.client.evaluations["nightly-red-streak"] = redstreak.Evaluation{}
			h.store.Reset()
			stderr := runGitHubRunsEvaluate(t, h, 1, "--monitor", "nightly-red-streak")
			if !strings.Contains(stderr, tc.err.Error()) {
				t.Fatalf("stderr = %q, want %q", stderr, tc.err)
			}
			if calls := h.store.Calls(); len(calls) != 0 {
				t.Fatalf("observation error mutated episode: %#v", calls)
			}
			after := requireSingleEpisode(t, h.store, "nightly-red-streak")
			if after.ID != before.ID || !reflect.DeepEqual(after.Metadata, before.Metadata) {
				t.Fatalf("episode changed on observation error:\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}

func TestGitHubRunsEvaluateDoesNotPartiallyWriteWhenAnotherMonitorFails(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["mac-regression-red-streak"] = testRedStreakEvaluation(
		"mac-regression-red-streak", 1251, "failure", 3, 0,
	)
	h.client.errs["nightly-red-streak"] = errors.New("decode Nightly jobs: malformed API data")

	runGitHubRunsEvaluate(t, h, 1)
	if got, want := h.client.calls, []string{"mac-regression-red-streak", "nightly-red-streak"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("evaluated monitors = %v, want %v before any mutation", got, want)
	}
	if calls := h.store.Calls(); len(calls) != 0 {
		t.Fatalf("multi-monitor failure left partial writes: %#v", calls)
	}
	for _, monitor := range []string{"mac-regression-red-streak", "nightly-red-streak"} {
		if episodes := requireEpisodes(t, h.store, monitor, true); len(episodes) != 0 {
			t.Fatalf("%s episodes = %#v, want none after failed all-monitor evaluation", monitor, episodes)
		}
	}
}

func TestGitHubRunsEvaluateMarksAggregateContractErrorRedWithoutDuplicatingEpisode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matches int
	}{
		{name: "missing", matches: 0},
		{name: "duplicate", matches: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGitHubRunsContractHarness(t)
			h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
				"nightly-red-streak", 1301, "failure", 3, 0,
			)
			runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak")
			before := requireSingleEpisode(t, h.store, "nightly-red-streak")

			evaluation := testRedStreakEvaluation(
				"nightly-red-streak", 1302, "data-contract-error", 4, 0,
			)
			evaluation.DataContractError = true
			h.client.evaluations["nightly-red-streak"] = evaluation
			h.client.errs["nightly-red-streak"] = &redstreak.DataContractError{
				RunID:        1302,
				AggregateJob: "Nightly summary",
				Matches:      tc.matches,
			}
			h.store.Reset()
			runGitHubRunsEvaluate(t, h, 1, "--monitor", "nightly-red-streak")

			if creates := h.store.CallsForOp("Create"); len(creates) != 0 {
				t.Fatalf("aggregate contract error created duplicate episode: %#v", creates)
			}
			if updates := h.store.CallsForOp("Update"); len(updates) != 1 || updates[0].ID != before.ID {
				t.Fatalf("aggregate contract error updates = %#v, want one update of %s", updates, before.ID)
			}
			after := requireSingleEpisode(t, h.store, "nightly-red-streak")
			if after.ID != before.ID {
				t.Fatalf("aggregate contract error changed episode %s to %s", before.ID, after.ID)
			}
			if got := after.Metadata["redstreak.data_contract_error"]; got != "true" {
				t.Fatalf("redstreak.data_contract_error = %q, want true", got)
			}
			if got := after.Metadata["redstreak.consecutive_count"]; got != "4" {
				t.Fatalf("redstreak.consecutive_count = %q, want 4; contract break must count red", got)
			}
			if after.Status == "closed" {
				t.Fatal("aggregate contract error closed the active episode")
			}
		})
	}
}

func TestGitHubRunsEvaluateDryRunDoesNotMutateBeads(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 1401, "failure", 3, 0,
	)
	runGitHubRunsEvaluate(t, h, 0, "--monitor", "nightly-red-streak", "--dry-run")
	if calls := h.store.Calls(); len(calls) != 0 {
		t.Fatalf("--dry-run mutations = %#v, want none", calls)
	}
	if episodes := requireEpisodes(t, h.store, "nightly-red-streak", true); len(episodes) != 0 {
		t.Fatalf("--dry-run episodes = %#v, want none", episodes)
	}
}

func TestGitHubRunsEvaluateRejectsUnknownMonitorBeforeCallingGitHub(t *testing.T) {
	h := newGitHubRunsContractHarness(t)
	stderr := runGitHubRunsEvaluate(t, h, 1, "--monitor", "missing-monitor")
	if !strings.Contains(stderr, "missing-monitor") || !strings.Contains(stderr, "not found") {
		t.Fatalf("stderr = %q, want actionable unknown-monitor error", stderr)
	}
	if len(h.client.calls) != 0 {
		t.Fatalf("unknown monitor called GitHub for %v", h.client.calls)
	}
	if calls := h.store.Calls(); len(calls) != 0 {
		t.Fatalf("unknown monitor mutations = %#v, want none", calls)
	}
}

func newGitHubRunsContractHarness(t *testing.T) *githubRunsContractHarness {
	t.Helper()
	cityPath := writeGitHubRunsContractCity(t)
	mem := beads.NewMemStore()
	store := beadstest.NewRecordingStore(mem)
	client := &fakeGitHubRunEvaluator{
		evaluations: make(map[string]redstreak.Evaluation),
		errs:        make(map[string]error),
	}

	oldClient := newGitHubRunsEvaluateClient
	oldStore := openGitHubRunEvaluateStore
	newGitHubRunsEvaluateClient = func(token string) githubRunEvaluator {
		if token != "contract-token" {
			t.Fatalf("token = %q, want contract-token", token)
		}
		return client
	}
	openGitHubRunEvaluateStore = func(_, _ string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() {
		newGitHubRunsEvaluateClient = oldClient
		openGitHubRunEvaluateStore = oldStore
	})
	t.Setenv("GITHUB_TOKEN", "contract-token")
	t.Setenv("GH_TOKEN", "")

	return &githubRunsContractHarness{
		cityPath: cityPath,
		mem:      mem,
		store:    store,
		client:   client,
	}
}

func writeGitHubRunsContractCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir .gc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "gascity"), 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	body := `[workspace]
name = "test-city"

[[rigs]]
name = "gascity"
path = "gascity"
prefix = "ga"

[[github.run_monitor]]
name = "mac-regression-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "mac-regression.yml"
aggregate_job = "Mac regression summary"
activated_after = "2026-07-05T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/mac-triage"
priority = "P1"

[[github.run_monitor]]
name = "nightly-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "nightly.yml"
aggregate_job = "Nightly summary"
activated_after = "2026-07-30T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/nightly-triage"
priority = "P1"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityPath
}

func testRedStreakEvaluation(monitor string, runID int64, conclusion string, failures, successes int) redstreak.Evaluation {
	evaluatedAt := time.Date(2026, 7, 30, 12, int(runID%60), 0, 0, time.UTC)
	return redstreak.Evaluation{
		Monitor:              monitor,
		LatestRunID:          runID,
		LatestRunURL:         fmt.Sprintf("https://example.test/runs/%d", runID),
		AggregateConclusion:  conclusion,
		ConsecutiveFailures:  failures,
		ConsecutiveSuccesses: successes,
		EvaluatedRuns:        failures + successes,
		EvaluatedAt:          evaluatedAt,
	}
}

func runGitHubRunsEvaluate(t *testing.T, h *githubRunsContractHarness, wantCode int, extraArgs ...string) string {
	t.Helper()
	args := []string{"--city", h.cityPath, "github", "runs", "evaluate"}
	args = append(args, extraArgs...)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	if code != wantCode {
		t.Fatalf("run %v code = %d, want %d\nstdout=%q\nstderr=%q",
			args, code, wantCode, stdout.String(), stderr.String())
	}
	return stderr.String()
}

func requireSingleEpisode(t *testing.T, store beads.Store, monitor string) beads.Bead {
	t.Helper()
	episodes := requireEpisodes(t, store, monitor, false)
	if len(episodes) != 1 {
		t.Fatalf("%s episodes = %#v, want one", monitor, episodes)
	}
	return episodes[0]
}

func requireEpisodes(t *testing.T, store beads.Store, monitor string, includeClosed bool) []beads.Bead {
	t.Helper()
	opts := []beads.QueryOpt(nil)
	if includeClosed {
		opts = append(opts, beads.IncludeClosed)
	}
	episodes, err := store.ListByMetadata(map[string]string{
		"redstreak.monitor": monitor,
	}, 0, opts...)
	if err != nil {
		t.Fatalf("ListByMetadata(%s): %v", monitor, err)
	}
	return episodes
}

func assertOneClosedOneOpenEpisode(t *testing.T, episodes []beads.Bead, firstID string) {
	t.Helper()
	if len(episodes) != 2 {
		t.Fatalf("episodes = %#v, want one closed and one open", episodes)
	}
	var closed, open *beads.Bead
	for i := range episodes {
		switch episodes[i].Status {
		case "closed":
			closed = &episodes[i]
		default:
			open = &episodes[i]
		}
	}
	if closed == nil || open == nil {
		t.Fatalf("episodes = %#v, want one closed and one open", episodes)
	}
	if closed.ID != firstID {
		t.Fatalf("closed episode = %s, want original %s", closed.ID, firstID)
	}
	if open.ID == firstID {
		t.Fatalf("open episode reused closed id %s", firstID)
	}
	if got := open.Metadata["redstreak.consecutive_count"]; got != "3" {
		t.Fatalf("new episode consecutive_count = %q, want 3", got)
	}
	if _, err := strconv.ParseInt(open.Metadata["redstreak.run_id"], 10, 64); err != nil {
		t.Fatalf("new episode run_id = %q, want integer: %v", open.Metadata["redstreak.run_id"], err)
	}
}
