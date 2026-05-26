package githubmonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultGraphQLEndpoint = "https://api.github.com/graphql"

// GraphQLClient queries GitHub's GraphQL API for pull-request readiness state.
type GraphQLClient struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

// ClientOption configures a GraphQLClient.
type ClientOption func(*GraphQLClient)

// WithEndpoint overrides the GitHub GraphQL endpoint.
func WithEndpoint(endpoint string) ClientOption {
	return func(c *GraphQLClient) {
		if strings.TrimSpace(endpoint) != "" {
			c.endpoint = strings.TrimSpace(endpoint)
		}
	}
}

// WithHTTPClient overrides the HTTP client used for GitHub requests.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *GraphQLClient) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// NewGraphQLClient constructs a GitHub GraphQL API client.
func NewGraphQLClient(token string, opts ...ClientOption) *GraphQLClient {
	c := &GraphQLClient{
		endpoint: defaultGraphQLEndpoint,
		token:    strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// ListOpenPullRequests returns all currently open pull requests for a repo.
func (c *GraphQLClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	if c == nil {
		return nil, fmt.Errorf("GitHub client is nil")
	}
	if c.token == "" {
		return nil, fmt.Errorf("GitHub token is required")
	}
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("GitHub owner and repo are required")
	}

	var out []PullRequest
	cursor := ""
	for {
		page, next, err := c.listOpenPullRequestsPage(ctx, owner, repo, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

func (c *GraphQLClient) listOpenPullRequestsPage(ctx context.Context, owner, repo, cursor string) ([]PullRequest, string, error) {
	payload := graphQLRequest{
		Query: pullRequestsQuery,
		Variables: pullRequestsQueryVariables{
			Owner:  owner,
			Repo:   repo,
			Cursor: nullableString(cursor),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("encoding GitHub GraphQL request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("creating GitHub GraphQL request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("calling GitHub GraphQL: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close is best-effort

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, "", fmt.Errorf("GitHub GraphQL HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return DecodePullRequestsPage(resp.Body)
}

type graphQLRequest struct {
	Query     string                     `json:"query"`
	Variables pullRequestsQueryVariables `json:"variables"`
}

type pullRequestsQueryVariables struct {
	Owner  string  `json:"owner"`
	Repo   string  `json:"repo"`
	Cursor *string `json:"cursor"`
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

const pullRequestsQuery = `
query GasCityPRReadiness($owner: String!, $repo: String!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequests(first: 50, after: $cursor, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo {
        hasNextPage
        endCursor
      }
      nodes {
        number
        title
        url
        isDraft
        mergeStateStatus
        baseRefName
        headRefName
        headRefOid
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                contexts(first: 100) {
                  nodes {
                    __typename
                    ... on CheckRun {
                      checkName: name
                      status
                      conclusion
                      detailsUrl
                    }
                    ... on StatusContext {
                      checkName: context
                      state
                      targetUrl
                    }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`
