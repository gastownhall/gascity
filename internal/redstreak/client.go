package redstreak

import (
	"context"
	"errors"
	"net/http"

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
	_, _ = ctx, monitor
	return nil, errors.New("not implemented")
}

// ListJobs fetches the jobs executed within one workflow run.
func (c *Client) ListJobs(ctx context.Context, monitor config.GitHubRunMonitor, runID int64) ([]Job, error) {
	_, _, _ = ctx, monitor, runID
	return nil, errors.New("not implemented")
}

// Evaluate observes a monitor's current run/job history and evaluates its
// red-streak state. It satisfies the cmd/gc githubRunEvaluator interface.
func (c *Client) Evaluate(ctx context.Context, monitor config.GitHubRunMonitor) (Evaluation, error) {
	_, _ = ctx, monitor
	return Evaluation{}, errors.New("not implemented")
}
