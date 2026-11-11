package prwatchdog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// openPullRequestsPerPage is the page size of the open-PR listing; a page
	// shorter than this is the last one.
	openPullRequestsPerPage = 100

	// needsMacLabel and needsReviewFormulasLabel are the PR labels the
	// watchdog workflow turns into NeedsMacLabel and NeedsReviewFormulasLabel.
	needsMacLabel            = "needs-mac"
	needsReviewFormulasLabel = "needs-review-formulas"
)

var (
	headSHAPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// ValidateHeadSHA rejects anything but a full lowercase hex commit sha.
func ValidateHeadSHA(sha string) error {
	if !headSHAPattern.MatchString(sha) {
		return fmt.Errorf("head sha %q is not 40 lowercase hex characters", sha)
	}
	return nil
}

// ValidateRepository rejects anything but an owner/name pair.
func ValidateRepository(repo string) error {
	if !repositoryPattern.MatchString(repo) {
		return fmt.Errorf("repository %q is not owner/name", repo)
	}
	return nil
}

// WorkflowRun is one run of a workflow, as the Actions API reports it.
type WorkflowRun struct {
	ID         int64
	RunAttempt int
	Status     string
	Conclusion string
	CreatedAt  time.Time
}

// PullRequest is an open pull request's number, head commit and label names.
type PullRequest struct {
	Number  int
	HeadSHA string
	Labels  []string
}

// RewatchAPI is everything Rewatch reads from or writes to GitHub.
type RewatchAPI interface {
	Fetcher
	ListWatchdogRuns(ctx context.Context, headSHA string) ([]WorkflowRun, error)
	ListOpenPullRequests(ctx context.Context, page int) ([]PullRequest, error)
	Rerun(ctx context.Context, runID int64) error
}

// RewatchRequest is the commit to look at and whether to stop before the
// re-run.
type RewatchRequest struct {
	HeadSHA string
	DryRun  bool
}

// Decision is what Rewatch did, or would have done.
type Decision string

const (
	// DecisionNoOp means nothing was re-run.
	DecisionNoOp Decision = "no-op"
	// DecisionRerun means the watchdog run was re-run.
	DecisionRerun Decision = "re-run"
	// DecisionWouldRerun means a dry run stopped before the re-run.
	DecisionWouldRerun Decision = "would re-run"
)

// RewatchResult is the outcome of one Rewatch, filled in as far as the
// procedure got so a failure can still be summarized.
type RewatchResult struct {
	HeadSHA    string
	PRNumber   int
	RunID      int64
	RunAttempt int
	Decision   Decision
	Reason     string
	Checks     Summary
}

// benignRerunRefusals are the only re-run refusals that end as a no-op. A
// refusal matches when its status is equal and its body contains Body. Both
// pins are quoted from public reports, not captured live: actions/runner#3872
// ("This workflow is already running") and cli/cli#9989 ("Unable to retry
// this workflow run because it was created over a month ago"). Every other
// refusal, the one for the 50-run limit included, is deliberately unpinned: no
// public source quotes it, so it ends as an error the step summary shows.
var benignRerunRefusals = []struct {
	Name   string
	Status int
	Body   string
}{
	{"workflow already running", http.StatusForbidden, "This workflow is already running"},
	{"run past the 30-day re-run limit", http.StatusForbidden, "created over a month ago"},
}

// benignRefusal names the benign refusal err is, or returns "".
func benignRefusal(err *APIError) string {
	for _, b := range benignRerunRefusals {
		if err.Status == b.Status && strings.Contains(err.Body, b.Body) {
			return b.Name
		}
	}
	return ""
}

// Rewatch re-runs the PR evidence watchdog for a commit whose watchdog run
// failed or was canceled while the evidence it was waiting for has since
// passed. The checks are ordered cheapest first and each stops at a no-op:
// the latest watchdog run, label-agnostic evidence, that the commit is still
// an open PR's head, then the evidence under that PR's current labels.
func Rewatch(ctx context.Context, api RewatchAPI, req RewatchRequest) (RewatchResult, error) {
	res := RewatchResult{HeadSHA: req.HeadSHA, Decision: DecisionNoOp}
	if err := ValidateHeadSHA(req.HeadSHA); err != nil {
		return res, err
	}

	runs, err := api.ListWatchdogRuns(ctx, req.HeadSHA)
	if err != nil {
		return res, fmt.Errorf("listing watchdog runs: %w", err)
	}
	if len(runs) == 0 {
		res.Reason = "no watchdog run for this commit"
		return res, nil
	}
	latest := runs[0]
	for _, r := range runs[1:] {
		if r.CreatedAt.After(latest.CreatedAt) || (r.CreatedAt.Equal(latest.CreatedAt) && r.ID > latest.ID) {
			latest = r
		}
	}
	res.RunID, res.RunAttempt = latest.ID, latest.RunAttempt
	if latest.Status != "completed" {
		res.Reason = fmt.Sprintf("latest watchdog run %d is %s", latest.ID, latest.Status)
		return res, nil
	}
	// The workflow-run API spells this conclusion with two l; the check-run
	// constant ConclusionCancelled does not, and must not be used here.
	if latest.Conclusion != "failure" && latest.Conclusion != "cancelled" { //nolint:misspell // the REST API's spelling
		res.Reason = fmt.Sprintf("latest watchdog run %d concluded %s", latest.ID, latest.Conclusion)
		return res, nil
	}

	checkRuns, err := api.FetchCheckRuns(ctx, req.HeadSHA)
	if err != nil {
		return res, fmt.Errorf("fetching check runs: %w", err)
	}
	pre := evaluateAtDeadline(req.HeadSHA, checkRuns, false, false)
	res.Checks = pre.Summary
	if !pre.Pass {
		res.Reason = "evidence still does not pass: " + pre.Reason
		return res, nil
	}

	pr, found, err := findOpenPullRequest(ctx, api, req.HeadSHA)
	if err != nil {
		return res, err
	}
	if !found {
		res.Reason = req.HeadSHA + " is not the current head of an open PR"
		return res, nil
	}
	res.PRNumber = pr.Number

	post := evaluateAtDeadline(req.HeadSHA, checkRuns, slices.Contains(pr.Labels, needsMacLabel), slices.Contains(pr.Labels, needsReviewFormulasLabel))
	res.Checks = post.Summary
	if !post.Pass {
		res.Reason = "evidence does not pass under the PR's current labels: " + post.Reason
		return res, nil
	}

	if req.DryRun {
		res.Decision = DecisionWouldRerun
		res.Reason = fmt.Sprintf("evidence passes; dry run, not re-running watchdog run %d", latest.ID)
		return res, nil
	}
	if err := api.Rerun(ctx, latest.ID); err != nil {
		var limited *RateLimitError
		var apiErr *APIError
		if !errors.As(err, &limited) && errors.As(err, &apiErr) {
			if name := benignRefusal(apiErr); name != "" {
				res.Reason = fmt.Sprintf("re-run refused (%s): %s", name, apiErr.Body)
				return res, nil
			}
		}
		return res, fmt.Errorf("re-running watchdog run %d: %w", latest.ID, err)
	}
	res.Decision = DecisionRerun
	res.Reason = fmt.Sprintf("evidence passes; re-ran watchdog run %d", latest.ID)
	return res, nil
}

// evaluateAtDeadline judges the evidence as the watchdog would after its
// deadline: nothing is waited for.
func evaluateAtDeadline(headSHA string, runs []CheckRun, needsMac, needsReviewFormulas bool) Evaluation {
	return Evaluate(Input{
		HeadSHA:                  headSHA,
		CheckRuns:                runs,
		Elapsed:                  ObservationDeadline,
		Deadline:                 ObservationDeadline,
		NeedsMacLabel:            needsMac,
		NeedsReviewFormulasLabel: needsReviewFormulas,
	})
}

// findOpenPullRequest pages the open PRs until one has headSHA as its head.
func findOpenPullRequest(ctx context.Context, api RewatchAPI, headSHA string) (PullRequest, bool, error) {
	for page := 1; ; page++ {
		prs, err := api.ListOpenPullRequests(ctx, page)
		if err != nil {
			return PullRequest{}, false, fmt.Errorf("listing open pull requests (page %d): %w", page, err)
		}
		for _, pr := range prs {
			if pr.HeadSHA == headSHA {
				return pr, true, nil
			}
		}
		if len(prs) < openPullRequestsPerPage {
			return PullRequest{}, false, nil
		}
	}
}
