package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/redstreak"
)

// This file covers the ga-clmemp.5 enrollment: Mac Regression and Nightly
// registered in the generic red-streak mechanism (ga-clmemp.4), routed to
// gascity/investigator via configuration only. It reuses the harness and
// helpers from cmd_github_runs_contract_test.go and adds nothing to
// production code -- the generic mechanism already threads monitor.Route
// into gc.routed_to at episode creation (createRedStreakEpisode).

func TestGitHubRunsEnrollmentRoutesCreatedEpisodesToInvestigator(t *testing.T) {
	h := newGitHubRunsEnrollmentHarness(t)
	h.client.evaluations["mac-regression-red-streak"] = testRedStreakEvaluation(
		"mac-regression-red-streak", 2001, "failure", 3, 0,
	)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 2101, "failure", 3, 0,
	)

	runGitHubRunsEvaluate(t, h, 0)

	routeTargets := hookClaimRouteTargets("gascity/investigator")
	for _, tc := range []struct {
		monitor      string
		workflowFile string
	}{
		{monitor: "mac-regression-red-streak", workflowFile: "mac-regression.yml"},
		{monitor: "nightly-red-streak", workflowFile: "nightly.yml"},
	} {
		episode := requireSingleEpisode(t, h.store, tc.monitor)
		if got := episode.Metadata["gc.routed_to"]; got != "gascity/investigator" {
			t.Fatalf("%s gc.routed_to = %q, want gascity/investigator", tc.monitor, got)
		}
		if got := episode.Metadata["redstreak.workflow_file"]; got != tc.workflowFile {
			t.Fatalf("%s workflow_file = %q, want %s", tc.monitor, got, tc.workflowFile)
		}
		if got := episode.Labels; len(got) != 1 || got[0] != "ci-nightly-red-streak" {
			t.Fatalf("%s labels = %v, want exactly [ci-nightly-red-streak] (no hardcoded role label)", tc.monitor, got)
		}
		if !hookClaimMatchesRoute(episode, routeTargets) {
			t.Fatalf("%s episode %#v does not match gascity/investigator's routed work query", tc.monitor, episode)
		}
	}

	otherRoute := hookClaimRouteTargets("gascity/ci-triage")
	mac := requireSingleEpisode(t, h.store, "mac-regression-red-streak")
	if hookClaimMatchesRoute(mac, otherRoute) {
		t.Fatal("Mac Regression episode matched an unrelated route; routing must be config-supplied, not implicit")
	}
}

func TestGitHubRunsEnrollmentRepeatedTicksDoNotDuplicateEpisodes(t *testing.T) {
	h := newGitHubRunsEnrollmentHarness(t)
	h.client.evaluations["mac-regression-red-streak"] = testRedStreakEvaluation(
		"mac-regression-red-streak", 2201, "failure", 3, 0,
	)
	h.client.evaluations["nightly-red-streak"] = testRedStreakEvaluation(
		"nightly-red-streak", 2301, "failure", 3, 0,
	)

	runGitHubRunsEvaluate(t, h, 0)
	mac := requireSingleEpisode(t, h.store, "mac-regression-red-streak")
	nightly := requireSingleEpisode(t, h.store, "nightly-red-streak")

	for tick := 0; tick < 3; tick++ {
		h.store.Reset()
		runGitHubRunsEvaluate(t, h, 0)
		if calls := h.store.Calls(); len(calls) != 0 {
			t.Fatalf("tick %d: repeated evaluation against the same observed runs mutated beads: %#v", tick, calls)
		}
	}

	if got := requireSingleEpisode(t, h.store, "mac-regression-red-streak"); got.ID != mac.ID {
		t.Fatalf("Mac Regression episode changed from %s to %s across repeated ticks", mac.ID, got.ID)
	}
	if got := requireSingleEpisode(t, h.store, "nightly-red-streak"); got.ID != nightly.ID {
		t.Fatalf("Nightly episode changed from %s to %s across repeated ticks", nightly.ID, got.ID)
	}
}

func newGitHubRunsEnrollmentHarness(t *testing.T) *githubRunsContractHarness {
	t.Helper()
	cityPath := writeGitHubRunsEnrollmentCity(t)
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

// writeGitHubRunsEnrollmentCity mirrors writeGitHubRunsContractCity's shape
// but declares the two ga-clmemp.5 enrollment records: Mac Regression and
// Nightly, both routed to gascity/investigator (not a hardcoded role name --
// the route is plain configuration) with the activation boundaries the
// enrollment replay tests in internal/redstreak/enrollment_replay_test.go
// already validate against captured history (2026-07-02 / 2026-07-26).
func writeGitHubRunsEnrollmentCity(t *testing.T) string {
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
activated_after = "2026-07-02T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/investigator"
priority = "P1"

[[github.run_monitor]]
name = "nightly-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "nightly.yml"
aggregate_job = "Nightly summary"
activated_after = "2026-07-26T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/investigator"
priority = "P1"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityPath
}
