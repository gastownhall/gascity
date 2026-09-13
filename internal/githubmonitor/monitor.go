// Package githubmonitor evaluates configured GitHub pull-request readiness
// monitors against live or fixture-supplied pull-request state.
package githubmonitor

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	// StateClean means the PR has no currently observed readiness problem.
	StateClean = "clean"
	// StatePending means the PR has checks still running or queued.
	StatePending = "pending"
	// StateFailed means one or more checks or status contexts failed.
	StateFailed = "failed"
	// StateConflicted means GitHub reports the PR as dirty/conflicted.
	StateConflicted = "conflicted"
	// StateBehind means the PR branch is behind its base branch.
	StateBehind = "behind"
	// StateBlocked means GitHub reports a branch-protection/mergeability block.
	StateBlocked = "blocked"
	// StateDraft means the PR is draft and should not be repaired.
	StateDraft = "draft"
	// StateUnknown means GitHub did not provide a recognized readiness state.
	StateUnknown = "unknown"

	// FailureKindChecksFailed identifies failed required or reported checks.
	FailureKindChecksFailed = "checks_failed"
	// FailureKindMergeConflict identifies a dirty/conflicted PR branch.
	FailureKindMergeConflict = "merge_conflict"
	// FailureKindBehindBase identifies a PR branch that needs updating.
	FailureKindBehindBase = "behind_base"
	// FailureKindBlocked identifies a GitHub mergeability block without a more
	// specific failed-check or conflict cause.
	FailureKindBlocked = "blocked"
)

// Check is a normalized GitHub check run or commit status context.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
}

// PullRequest is the normalized GitHub pull-request state used by monitors.
type PullRequest struct {
	Number           int     `json:"number"`
	Title            string  `json:"title"`
	URL              string  `json:"url"`
	BaseRefName      string  `json:"base_ref_name"`
	HeadRefName      string  `json:"head_ref_name"`
	HeadSHA          string  `json:"head_sha"`
	MergeStateStatus string  `json:"merge_state_status"`
	IsDraft          bool    `json:"is_draft"`
	Checks           []Check `json:"checks,omitempty"`
}

// Result is one monitor evaluation for one pull request.
type Result struct {
	Monitor          string   `json:"monitor"`
	Owner            string   `json:"owner"`
	Repo             string   `json:"repo"`
	Number           int      `json:"number"`
	Title            string   `json:"title,omitempty"`
	URL              string   `json:"url,omitempty"`
	BaseRefName      string   `json:"base_ref_name"`
	HeadRefName      string   `json:"head_ref_name,omitempty"`
	HeadSHA          string   `json:"head_sha,omitempty"`
	MergeStateStatus string   `json:"merge_state_status,omitempty"`
	State            string   `json:"state"`
	FailureKind      string   `json:"failure_kind,omitempty"`
	Actionable       bool     `json:"actionable"`
	FailedChecks     []string `json:"failed_checks,omitempty"`
	PendingChecks    []string `json:"pending_checks,omitempty"`
	Rig              string   `json:"rig"`
	RepairRoute      string   `json:"repair_route"`
	Notify           []string `json:"notify,omitempty"`
}

// EvaluatePullRequests evaluates PR readiness for one configured monitor.
func EvaluatePullRequests(monitor config.GitHubPRMonitor, prs []PullRequest) []Result {
	baseBranches := normalizedBranchSet(monitor.BaseBranches)
	results := make([]Result, 0, len(prs))
	for _, pr := range prs {
		if len(baseBranches) > 0 && !baseBranches[strings.ToLower(strings.TrimSpace(pr.BaseRefName))] {
			continue
		}
		failed, pending := classifyChecks(pr.Checks)
		state, failureKind, actionable := classifyPullRequest(pr, failed, pending)
		results = append(results, Result{
			Monitor:          strings.TrimSpace(monitor.Name),
			Owner:            strings.TrimSpace(monitor.Owner),
			Repo:             strings.TrimSpace(monitor.Repo),
			Number:           pr.Number,
			Title:            pr.Title,
			URL:              pr.URL,
			BaseRefName:      pr.BaseRefName,
			HeadRefName:      pr.HeadRefName,
			HeadSHA:          pr.HeadSHA,
			MergeStateStatus: strings.TrimSpace(strings.ToUpper(pr.MergeStateStatus)),
			State:            state,
			FailureKind:      failureKind,
			Actionable:       actionable,
			FailedChecks:     failed,
			PendingChecks:    pending,
			Rig:              strings.TrimSpace(monitor.Rig),
			RepairRoute:      strings.TrimSpace(monitor.RepairRoute),
			Notify:           cleanStrings(monitor.Notify),
		})
	}
	return results
}

func normalizedBranchSet(branches []string) map[string]bool {
	out := make(map[string]bool, len(branches))
	for _, branch := range branches {
		branch = strings.ToLower(strings.TrimSpace(branch))
		if branch != "" {
			out[branch] = true
		}
	}
	return out
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func classifyChecks(checks []Check) (failed, pending []string) {
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		status := strings.ToUpper(strings.TrimSpace(check.Status))
		conclusion := strings.ToUpper(strings.TrimSpace(check.Conclusion))
		switch {
		case checkFailed(status, conclusion):
			failed = append(failed, name)
		case checkPending(status, conclusion):
			pending = append(pending, name)
		}
	}
	slices.Sort(failed)
	slices.Sort(pending)
	return failed, pending
}

func checkFailed(status, conclusion string) bool {
	switch conclusion {
	case "FAILURE", "ERROR", "CANCELED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
		return true
	}
	switch status {
	case "FAILURE", "ERROR":
		return true
	}
	return false
}

func checkPending(status, conclusion string) bool {
	if conclusion != "" {
		switch conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			return false
		}
		return !checkFailed(status, conclusion)
	}
	switch status {
	case "PENDING", "EXPECTED", "QUEUED", "REQUESTED", "WAITING", "IN_PROGRESS":
		return true
	case "COMPLETED", "SUCCESS":
		return false
	}
	return false
}

func classifyPullRequest(pr PullRequest, failed, pending []string) (state, failureKind string, actionable bool) {
	if pr.IsDraft {
		return StateDraft, "", false
	}
	if len(failed) > 0 {
		return StateFailed, FailureKindChecksFailed, true
	}

	switch strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus)) {
	case "DIRTY":
		return StateConflicted, FailureKindMergeConflict, true
	case "BEHIND":
		return StateBehind, FailureKindBehindBase, true
	case "UNSTABLE":
		return StateFailed, FailureKindChecksFailed, true
	case "BLOCKED":
		return StateBlocked, FailureKindBlocked, true
	}

	if len(pending) > 0 {
		return StatePending, "", false
	}
	switch strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus)) {
	case "", "UNKNOWN":
		return StateUnknown, "", false
	default:
		return StateClean, "", false
	}
}

type graphQLPullRequestsResponse struct {
	Data   graphQLData    `json:"data"`
	Errors []graphQLError `json:"errors,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLData struct {
	Repository graphQLRepository `json:"repository"`
}

type graphQLRepository struct {
	PullRequests graphQLPullRequestConnection `json:"pullRequests"`
}

type graphQLPullRequestConnection struct {
	PageInfo graphQLPageInfo      `json:"pageInfo"`
	Nodes    []graphQLPullRequest `json:"nodes"`
}

type graphQLPageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type graphQLPullRequest struct {
	Number           int                `json:"number"`
	Title            string             `json:"title"`
	URL              string             `json:"url"`
	IsDraft          bool               `json:"isDraft"`
	MergeStateStatus string             `json:"mergeStateStatus"`
	BaseRefName      string             `json:"baseRefName"`
	HeadRefName      string             `json:"headRefName"`
	HeadRefOID       string             `json:"headRefOid"`
	Commits          graphQLCommitNodes `json:"commits"`
}

type graphQLCommitNodes struct {
	Nodes []graphQLCommitNode `json:"nodes"`
}

type graphQLCommitNode struct {
	Commit graphQLCommit `json:"commit"`
}

type graphQLCommit struct {
	OID               string              `json:"oid"`
	StatusCheckRollup *graphQLCheckRollup `json:"statusCheckRollup"`
}

type graphQLCheckRollup struct {
	Contexts graphQLCheckContextConnection `json:"contexts"`
}

type graphQLCheckContextConnection struct {
	Nodes []graphQLCheckContext `json:"nodes"`
}

type graphQLCheckContext struct {
	Type       string `json:"__typename"`
	CheckName  string `json:"checkName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	DetailsURL string `json:"detailsUrl"`
	TargetURL  string `json:"targetUrl"`
}

// DecodePullRequestsPage decodes one GraphQL pull-request page.
func DecodePullRequestsPage(r io.Reader) ([]PullRequest, string, error) {
	var resp graphQLPullRequestsResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, "", fmt.Errorf("decoding GitHub GraphQL response: %w", err)
	}
	if len(resp.Errors) > 0 {
		messages := make([]string, 0, len(resp.Errors))
		for _, graphErr := range resp.Errors {
			if msg := strings.TrimSpace(graphErr.Message); msg != "" {
				messages = append(messages, msg)
			}
		}
		if len(messages) == 0 {
			messages = append(messages, "unknown GraphQL error")
		}
		return nil, "", fmt.Errorf("GitHub GraphQL error: %s", strings.Join(messages, "; "))
	}

	conn := resp.Data.Repository.PullRequests
	prs := make([]PullRequest, 0, len(conn.Nodes))
	for _, node := range conn.Nodes {
		pr := PullRequest{
			Number:           node.Number,
			Title:            node.Title,
			URL:              node.URL,
			IsDraft:          node.IsDraft,
			MergeStateStatus: node.MergeStateStatus,
			BaseRefName:      node.BaseRefName,
			HeadRefName:      node.HeadRefName,
			HeadSHA:          node.HeadRefOID,
		}
		if len(node.Commits.Nodes) > 0 {
			commit := node.Commits.Nodes[len(node.Commits.Nodes)-1].Commit
			if pr.HeadSHA == "" {
				pr.HeadSHA = commit.OID
			}
			if commit.StatusCheckRollup != nil {
				pr.Checks = normalizeGraphQLChecks(commit.StatusCheckRollup.Contexts.Nodes)
			}
		}
		prs = append(prs, pr)
	}

	next := ""
	if conn.PageInfo.HasNextPage && conn.PageInfo.EndCursor != nil {
		next = *conn.PageInfo.EndCursor
	}
	return prs, next, nil
}

func normalizeGraphQLChecks(nodes []graphQLCheckContext) []Check {
	checks := make([]Check, 0, len(nodes))
	for _, node := range nodes {
		name := strings.TrimSpace(node.CheckName)
		if name == "" {
			continue
		}
		check := Check{
			Name:   name,
			Status: strings.TrimSpace(node.Status),
			URL:    strings.TrimSpace(node.DetailsURL),
		}
		if node.State != "" {
			check.Status = strings.TrimSpace(node.State)
			check.Conclusion = strings.TrimSpace(node.State)
			if check.URL == "" {
				check.URL = strings.TrimSpace(node.TargetURL)
			}
		} else {
			check.Conclusion = strings.TrimSpace(node.Conclusion)
		}
		checks = append(checks, check)
	}
	return checks
}
