package githubmonitor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestEvaluatePullRequestsMarksFailedChecksActionable(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "partcl-main",
		Owner:        "partcleda",
		Repo:         "partcl",
		BaseBranches: []string{"main"},
		Rig:          "partcl",
		RepairRoute:  "partcl/polecat",
		Notify:       []string{"ops"},
	}
	prs := []PullRequest{
		{
			Number:           2560,
			Title:            "Fix deployment",
			URL:              "https://github.com/partcleda/partcl/pull/2560",
			BaseRefName:      "main",
			HeadRefName:      "feature/deploy",
			HeadSHA:          "abc123",
			MergeStateStatus: "UNSTABLE",
			Checks: []Check{
				{Name: "version-check / check-versions", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "integration", Status: "IN_PROGRESS"},
			},
		},
	}

	results := EvaluatePullRequests(monitor, prs)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	got := results[0]
	if got.State != StateFailed {
		t.Fatalf("State = %q, want %q", got.State, StateFailed)
	}
	if !got.Actionable {
		t.Fatal("Actionable = false, want true")
	}
	if got.FailureKind != FailureKindChecksFailed {
		t.Fatalf("FailureKind = %q, want %q", got.FailureKind, FailureKindChecksFailed)
	}
	if len(got.FailedChecks) != 1 || got.FailedChecks[0] != "version-check / check-versions" {
		t.Fatalf("FailedChecks = %v, want failed version check", got.FailedChecks)
	}
	if len(got.PendingChecks) != 1 || got.PendingChecks[0] != "integration" {
		t.Fatalf("PendingChecks = %v, want integration", got.PendingChecks)
	}
	if got.RepairRoute != "partcl/polecat" || got.Rig != "partcl" {
		t.Fatalf("route metadata = %q/%q, want partcl/polecat and partcl", got.RepairRoute, got.Rig)
	}
}

func TestEvaluatePullRequestsMarksMergeProblemsActionable(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "main",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 10, BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 11, BaseRefName: "main", MergeStateStatus: "BEHIND"},
	})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].State != StateConflicted || results[0].FailureKind != FailureKindMergeConflict {
		t.Fatalf("dirty result = %#v, want conflict", results[0])
	}
	if results[1].State != StateBehind || results[1].FailureKind != FailureKindBehindBase {
		t.Fatalf("behind result = %#v, want behind base", results[1])
	}
}

func TestEvaluatePullRequestsFiltersUnconfiguredBaseBranches(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "main",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 10, BaseRefName: "release", MergeStateStatus: "DIRTY"},
	})

	if len(results) != 0 {
		t.Fatalf("results = %#v, want no release PRs", results)
	}
}

func TestEvaluatePullRequestsCleanWithPendingChecksIsNotRepairActionable(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "main",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{
			Number:           12,
			BaseRefName:      "main",
			MergeStateStatus: "CLEAN",
			Checks:           []Check{{Name: "ci", Status: "QUEUED"}},
		},
	})

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Actionable {
		t.Fatalf("Actionable = true for pending-only result: %#v", results[0])
	}
	if results[0].State != StatePending {
		t.Fatalf("State = %q, want %q", results[0].State, StatePending)
	}
}

func TestEvaluatePullRequestsAuthorAllowListSkipsOtherAuthors(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "mine",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
		Authors:      []string{"alice"},
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 10, Author: "alice", BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 11, Author: "bob", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	})

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only alice's PR)", len(results))
	}
	if results[0].Number != 10 || results[0].Author != "alice" {
		t.Fatalf("kept result = %#v, want alice's PR #10", results[0])
	}
	if !results[0].Actionable {
		t.Fatal("alice's dirty PR should be actionable")
	}
}

func TestEvaluatePullRequestsAuthorAllowListFailsClosedOnUnresolvedAuthor(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "mine",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
		Authors:      []string{"alice"},
	}

	// An actionable PR with an empty/unresolved author must be skipped entirely
	// when an allow-list is configured — never evaluated, never actionable.
	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 12, Author: "", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	})

	if len(results) != 0 {
		t.Fatalf("results = %#v, want empty (unresolved author fails closed)", results)
	}
}

func TestEvaluatePullRequestsAuthorAllowListIsCaseSensitive(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "mine",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
		Authors:      []string{"Alice"},
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 13, Author: "alice", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	})

	if len(results) != 0 {
		t.Fatalf("results = %#v, want empty (case-sensitive match)", results)
	}
}

func TestEvaluatePullRequestsNoAuthorAllowListActsOnAll(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "all",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
	}

	results := EvaluatePullRequests(monitor, []PullRequest{
		{Number: 10, Author: "alice", BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 11, Author: "", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (no author restriction)", len(results))
	}
}

func TestEvaluatePullRequestsWithAllowedAuthorsFlagUnionsWithConfig(t *testing.T) {
	monitor := config.GitHubPRMonitor{
		Name:         "mine",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
		Authors:      []string{"alice"},
	}

	// The --author flag adds bob for this run; alice (config) stays allowed.
	results := EvaluatePullRequestsWithAllowedAuthors(monitor, []PullRequest{
		{Number: 10, Author: "alice", BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 11, Author: "bob", BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 12, Author: "carol", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	}, []string{"bob"})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (alice ∪ bob)", len(results))
	}
	for _, r := range results {
		if r.Author == "carol" {
			t.Fatalf("carol should be excluded: %#v", r)
		}
	}
}

func TestEvaluatePullRequestsWithAllowedAuthorsFlagAloneRestricts(t *testing.T) {
	// No configured allow-list, but a --author flag value must still fail-close
	// the run to that author only.
	monitor := config.GitHubPRMonitor{
		Name:         "adhoc",
		Owner:        "org",
		Repo:         "repo",
		BaseBranches: []string{"main"},
		Rig:          "repo",
		RepairRoute:  "repo/polecat",
	}

	results := EvaluatePullRequestsWithAllowedAuthors(monitor, []PullRequest{
		{Number: 10, Author: "alice", BaseRefName: "main", MergeStateStatus: "DIRTY"},
		{Number: 11, Author: "bob", BaseRefName: "main", MergeStateStatus: "DIRTY"},
	}, []string{"alice"})

	if len(results) != 1 || results[0].Author != "alice" {
		t.Fatalf("results = %#v, want only alice", results)
	}
}

func TestGraphQLDecodePullRequestsCapturesAuthor(t *testing.T) {
	body := strings.NewReader(`{
		"data": {
			"repository": {
				"pullRequests": {
					"pageInfo": {"hasNextPage": false, "endCursor": null},
					"nodes": [
						{"number": 1, "author": {"login": "alice"}, "baseRefName": "main"},
						{"number": 2, "author": null, "baseRefName": "main"}
					]
				}
			}
		}
	}`)

	prs, _, err := DecodePullRequestsPage(body)
	if err != nil {
		t.Fatalf("DecodePullRequestsPage: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("len(prs) = %d, want 2", len(prs))
	}
	if prs[0].Author != "alice" {
		t.Fatalf("prs[0].Author = %q, want alice", prs[0].Author)
	}
	if prs[1].Author != "" {
		t.Fatalf("prs[1].Author = %q, want empty (null author actor)", prs[1].Author)
	}
}

func TestGraphQLDecodePullRequests(t *testing.T) {
	body := strings.NewReader(`{
		"data": {
			"repository": {
				"pullRequests": {
					"pageInfo": {"hasNextPage": false, "endCursor": null},
					"nodes": [{
						"number": 2560,
						"title": "Deploy",
						"url": "https://github.com/partcleda/partcl/pull/2560",
						"isDraft": false,
						"mergeStateStatus": "BLOCKED",
						"baseRefName": "main",
						"headRefName": "fix",
						"headRefOid": "abc123",
						"commits": {
							"nodes": [{
								"commit": {
									"oid": "abc123",
									"statusCheckRollup": {
										"contexts": {
											"nodes": [
												{"__typename":"CheckRun","checkName":"unit","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://example.test/unit"},
												{"__typename":"StatusContext","checkName":"deploy","state":"FAILURE","targetUrl":"https://example.test/deploy"}
											]
										}
									}
								}
							}]
						}
					}]
				}
			}
		}
	}`)

	prs, next, err := DecodePullRequestsPage(body)
	if err != nil {
		t.Fatalf("DecodePullRequestsPage: %v", err)
	}
	if next != "" {
		t.Fatalf("next = %q, want empty", next)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1", len(prs))
	}
	got := prs[0]
	if got.HeadSHA != "abc123" || got.MergeStateStatus != "BLOCKED" {
		t.Fatalf("decoded PR = %#v", got)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("checks = %#v, want 2", got.Checks)
	}
	if got.Checks[1].Name != "deploy" || got.Checks[1].Conclusion != "FAILURE" {
		t.Fatalf("status context = %#v, want normalized failing deploy check", got.Checks[1])
	}
}
