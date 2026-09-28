// Command watchdog observes the GitHub Checks API for a pull request's head
// commit and fails closed if complete CI evidence never appears for it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

// pollInterval is how often the watchdog re-checks the Checks API while
// waiting for evidence to complete.
const pollInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prwatchdog: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	repo := os.Getenv("REPOSITORY")
	headSHA := os.Getenv("PR_HEAD_SHA")
	token := os.Getenv("GH_TOKEN")
	if repo == "" || headSHA == "" || token == "" {
		return fmt.Errorf("REPOSITORY, PR_HEAD_SHA, and GH_TOKEN must all be set")
	}

	fetcher := &githubFetcher{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		repo:       repo,
		token:      token,
		checkNames: []string{
			prwatchdog.CheckName,
			prwatchdog.CIRequiredName,
			prwatchdog.MacCheckName,
			prwatchdog.ReviewFormulasCheckName,
		},
	}

	eval := prwatchdog.Watch(context.Background(), fetcher, realClock{}, realSleeper{}, prwatchdog.PollOptions{
		HeadSHA:                  headSHA,
		NeedsMacLabel:            parseBoolEnv("NEEDS_MAC_LABEL"),
		NeedsReviewFormulasLabel: parseBoolEnv("NEEDS_REVIEW_FORMULAS_LABEL"),
		Deadline:                 prwatchdog.ObservationDeadline,
		Interval:                 pollInterval,
	})

	summary := renderSummary(eval)
	fmt.Println(summary)
	if err := writeStepSummary(summary); err != nil {
		fmt.Fprintln(os.Stderr, "prwatchdog: writing step summary: "+err.Error())
	}

	if !eval.Pass {
		return fmt.Errorf("%s", eval.Reason)
	}
	return nil
}

func parseBoolEnv(name string) bool {
	v, _ := strconv.ParseBool(os.Getenv(name))
	return v
}

// realClock reports wall-clock time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// realSleeper sleeps for a real duration, honoring context cancellation.
type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// githubFetcher retrieves check runs from the GitHub Checks API, one
// paginated request per tracked check name.
type githubFetcher struct {
	httpClient *http.Client
	repo       string
	token      string
	checkNames []string
}

type checkRunsResponse struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []checkRun `json:"check_runs"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"started_at"`
}

func (f *githubFetcher) FetchCheckRuns(ctx context.Context, headSHA string) ([]prwatchdog.CheckRun, error) {
	var all []prwatchdog.CheckRun
	for _, name := range f.checkNames {
		runs, err := f.fetchByName(ctx, headSHA, name)
		if err != nil {
			return nil, fmt.Errorf("fetching check runs named %q: %w", name, err)
		}
		all = append(all, runs...)
	}
	return all, nil
}

func (f *githubFetcher) fetchByName(ctx context.Context, headSHA, name string) ([]prwatchdog.CheckRun, error) {
	var out []prwatchdog.CheckRun
	for pageNum := 1; ; pageNum++ {
		page, err := f.fetchPage(ctx, headSHA, name, pageNum)
		if err != nil {
			return nil, err
		}
		for _, r := range page.CheckRuns {
			startedAt, _ := time.Parse(time.RFC3339, r.StartedAt)
			out = append(out, prwatchdog.CheckRun{
				Name:       r.Name,
				HeadSHA:    r.HeadSHA,
				Status:     prwatchdog.Status(r.Status),
				Conclusion: prwatchdog.Conclusion(r.Conclusion),
				StartedAt:  startedAt,
				ID:         r.ID,
			})
		}
		if len(page.CheckRuns) < 100 {
			return out, nil
		}
	}
}

func (f *githubFetcher) fetchPage(ctx context.Context, headSHA, name string, pageNum int) (checkRunsResponse, error) {
	q := url.Values{}
	q.Set("check_name", name)
	q.Set("per_page", "100")
	q.Set("page", strconv.Itoa(pageNum))
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s/check-runs?%s", f.repo, headSHA, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return checkRunsResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return checkRunsResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return checkRunsResponse{}, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return checkRunsResponse{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var page checkRunsResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return checkRunsResponse{}, fmt.Errorf("decoding response: %w", err)
	}
	return page, nil
}

// renderSummary produces the Markdown rendering shared by stdout and
// $GITHUB_STEP_SUMMARY.
func renderSummary(eval prwatchdog.Evaluation) string {
	var b strings.Builder
	if eval.Pass {
		b.WriteString("## PR evidence watchdog: PASS\n\n")
	} else {
		b.WriteString("## PR evidence watchdog: FAIL\n\n")
	}
	fmt.Fprintf(&b, "**Reason:** %s\n\n", eval.Reason)
	b.WriteString("| Check | State |\n")
	b.WriteString("| --- | --- |\n")
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CheckName, eval.Summary.Check)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.CIRequiredName, eval.Summary.CIRequired)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.MacCheckName, eval.Summary.Mac)
	fmt.Fprintf(&b, "| %s | %s |\n", prwatchdog.ReviewFormulasCheckName, eval.Summary.ReviewFormulas)
	return b.String()
}

func writeStepSummary(summary string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(summary)
	return err
}
