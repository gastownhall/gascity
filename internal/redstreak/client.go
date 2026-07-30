package redstreak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

const defaultEndpoint = "https://api.github.com"

// Client polls the GitHub Actions API for workflow run and job history.
type Client struct {
	token      string
	endpoint   string
	httpClient *http.Client
}

// ClientOption configures a Client constructed by NewClient.
type ClientOption func(*Client)

// WithEndpoint overrides the GitHub API base URL (used by tests).
func WithEndpoint(url string) ClientOption {
	return func(c *Client) { c.endpoint = url }
}

// WithHTTPClient overrides the HTTP client used for requests (used by tests).
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.httpClient = hc }
}

// NewClient constructs a Client authenticated with the given GitHub token.
func NewClient(token string, opts ...ClientOption) *Client {
	c := &Client{
		token:      token,
		endpoint:   defaultEndpoint,
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ListRuns fetches the monitor's workflow runs matching its trigger event.
func (c *Client) ListRuns(ctx context.Context, monitor config.GitHubRunMonitor) ([]ObservedRun, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs", monitor.Owner, monitor.Repo, monitor.WorkflowFile)
	query := url.Values{"event": {monitor.EventOrDefault()}}

	var wire struct {
		WorkflowRuns []struct {
			ID           int64     `json:"id"`
			Event        string    `json:"event"`
			Conclusion   string    `json:"conclusion"`
			RunStartedAt time.Time `json:"run_started_at"`
			HTMLURL      string    `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if err := c.get(ctx, path, query, &wire); err != nil {
		return nil, err
	}

	runs := make([]ObservedRun, 0, len(wire.WorkflowRuns))
	for _, r := range wire.WorkflowRuns {
		runs = append(runs, ObservedRun{
			Run: Run{
				ID:         r.ID,
				Event:      r.Event,
				Conclusion: r.Conclusion,
				StartedAt:  r.RunStartedAt,
				URL:        r.HTMLURL,
			},
		})
	}
	return runs, nil
}

// ListJobs fetches the jobs executed within one workflow run.
func (c *Client) ListJobs(ctx context.Context, monitor config.GitHubRunMonitor, runID int64) ([]Job, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs", monitor.Owner, monitor.Repo, runID)

	var wire struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.get(ctx, path, nil, &wire); err != nil {
		return nil, err
	}
	return wire.Jobs, nil
}

// Evaluate observes a monitor's current run/job history and evaluates its
// red-streak state. It satisfies the cmd/gc githubRunEvaluator interface.
func (c *Client) Evaluate(ctx context.Context, monitor config.GitHubRunMonitor) (Evaluation, error) {
	runs, err := c.ListRuns(ctx, monitor)
	if err != nil {
		return Evaluation{}, err
	}

	observed := make([]ObservedRun, len(runs))
	for i, run := range runs {
		jobs, err := c.ListJobs(ctx, monitor, run.ID)
		if err != nil {
			return Evaluation{}, err
		}
		run.Jobs = jobs
		observed[i] = run
	}

	eval, evalErr := EvaluateHistory(monitor, observed)
	eval.Monitor = monitor.Name
	eval.EvaluatedAt = time.Now().UTC()
	return eval, evalErr
}

// get issues an authenticated GET request against the GitHub API and decodes
// the JSON response body into out. query may be nil.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	requestURL := c.endpoint + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build github api request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github api request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github api request %s failed: status %d: %s", path, resp.StatusCode, body)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode github api response for %s: %w", path, err)
	}
	return nil
}
