package prwatchdog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestAPI(h http.Handler) *GitHubAPI {
	return &GitHubAPI{
		HTTPClient: &http.Client{Transport: handlerTransport{h}},
		APIBase:    "https://api.github.test",
		Repo:       "gastownhall/gascity",
		Token:      "t",
	}
}

func TestListWatchdogRuns_RequestAndDecode(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if want := "/repos/gastownhall/gascity/actions/workflows/pr-evidence-watchdog.yml/runs"; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		q := r.URL.Query()
		if q.Get("head_sha") != rewatchSHA || q.Get("event") != "pull_request_target" || q.Get("per_page") != "100" {
			t.Errorf("query = %v, want head_sha=%s&event=pull_request_target&per_page=100", q, rewatchSHA)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = fmt.Fprint(w, `{"total_count":2,"workflow_runs":[
			{"id":11,"run_attempt":2,"status":"completed","conclusion":"failure","created_at":"2026-10-27T12:00:00Z"},
			{"id":12,"run_attempt":1,"status":"in_progress","conclusion":null,"created_at":"2026-10-27T12:05:00Z"}]}`)
	})
	runs, err := newTestAPI(h).ListWatchdogRuns(context.Background(), rewatchSHA)
	if err != nil {
		t.Fatal(err)
	}
	want := []WorkflowRun{
		{ID: 11, RunAttempt: 2, Status: "completed", Conclusion: "failure", CreatedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)},
		{ID: 12, RunAttempt: 1, Status: "in_progress", Conclusion: "", CreatedAt: time.Date(2026, 10, 27, 12, 5, 0, 0, time.UTC)},
	}
	if len(runs) != len(want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Fatalf("runs[%d] = %+v, want %+v", i, runs[i], want[i])
		}
	}
}

func TestListOpenPullRequests_RequestAndDecode(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/gastownhall/gascity/pulls"; r.URL.Path != want {
			t.Errorf("path = %q, want %q (commits/{sha}/pulls is empty for forks)", r.URL.Path, want)
		}
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("per_page") != "100" || q.Get("page") != "3" {
			t.Errorf("query = %v, want state=open&per_page=100&page=3", q)
		}
		_, _ = fmt.Fprint(w, `[{"number":5,"head":{"sha":"abc"},"labels":[{"name":"needs-mac"},{"name":"bug"}]},
			{"number":6,"head":{"sha":"def"},"labels":[]}]`)
	})
	prs, err := newTestAPI(h).ListOpenPullRequests(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].Number != 5 || prs[0].HeadSHA != "abc" || strings.Join(prs[0].Labels, ",") != "needs-mac,bug" || prs[1].Number != 6 || len(prs[1].Labels) != 0 {
		t.Fatalf("prs = %+v", prs)
	}
}

func TestRerun_PostsToTheRunRerunEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{}`)
	})
	if err := newTestAPI(h).Rerun(context.Background(), 900); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/gastownhall/gascity/actions/runs/900/rerun" {
		t.Fatalf("request = %s %s, want POST /repos/gastownhall/gascity/actions/runs/900/rerun", gotMethod, gotPath)
	}
}

func TestRerun_RefusalIsATypedAPIErrorCarryingStatusAndBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	err := newTestAPI(h).Rerun(context.Background(), 900)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Body, "Resource not accessible by integration") {
		t.Fatalf("APIError = %+v, want status 403 and the response body", apiErr)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Fatalf("error text %q must put the status and body in the log", err)
	}
}

func TestRerun_RateLimitIsTypedNotAnAPIError(t *testing.T) {
	reset := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	})
	err := newTestAPI(h).Rerun(context.Background(), 900)
	var limited *RateLimitError
	if !errors.As(err, &limited) || !limited.Reset.Equal(reset) {
		t.Fatalf("err = %v, want *RateLimitError resetting at %v", err, reset)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && benignRefusal(apiErr) != "" {
		t.Fatalf("a rate limit must never classify as a benign refusal")
	}
}

func TestListCalls_NonOKIsAnError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `boom`)
	})
	api := newTestAPI(h)
	if _, err := api.ListWatchdogRuns(context.Background(), rewatchSHA); err == nil {
		t.Error("ListWatchdogRuns: want an error on 500")
	}
	if _, err := api.ListOpenPullRequests(context.Background(), 1); err == nil {
		t.Error("ListOpenPullRequests: want an error on 500")
	}
}

func TestListCalls_RateLimitIsTyped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"message":"secondary rate limit"}`)
	})
	api := newTestAPI(h)
	var limited *RateLimitError
	if _, err := api.ListWatchdogRuns(context.Background(), rewatchSHA); !errors.As(err, &limited) {
		t.Errorf("ListWatchdogRuns err = %v, want *RateLimitError", err)
	}
	if _, err := api.ListOpenPullRequests(context.Background(), 1); !errors.As(err, &limited) {
		t.Errorf("ListOpenPullRequests err = %v, want *RateLimitError", err)
	}
}

func TestFetchCheckRuns_OnTheRewatchClientTracksAllFourChecks(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"check_runs":[
			{"id":1,"name":"Check","head_sha":"s","status":"completed","conclusion":"success"},
			{"id":2,"name":"CI / required","head_sha":"s","status":"completed","conclusion":"success"},
			{"id":3,"name":"Mac regression summary","head_sha":"s","status":"completed","conclusion":"success"},
			{"id":4,"name":"Integration / review-formulas","head_sha":"s","status":"completed","conclusion":"success"},
			{"id":5,"name":"Evidence / critical-path suites","head_sha":"s","status":"completed","conclusion":"failure"}]}`)
	})
	runs, err := newTestAPI(h).FetchCheckRuns(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Fatalf("runs = %+v, want the four tracked checks and not the watchdog's own", runs)
	}
}

// TestBenignRefusal_PinsTheTwoPublicBodies pins the two benign refusals by
// literal status and body, deliberately without reading benignRerunRefusals,
// so a changed pin fails here. Both bodies are quoted from public reports, not
// captured live: "This workflow is already running" from actions/runner#3872
// and "Unable to retry this workflow run because it was created over a month
// ago" from cli/cli#9989.
func TestBenignRefusal_PinsTheTwoPublicBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"already running", "This workflow is already running", "workflow already running"},
		{"past the 30-day limit", "Unable to retry this workflow run because it was created over a month ago", "run past the 30-day re-run limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := benignRefusal(&APIError{Status: 403, Body: tc.body}); got != tc.want {
				t.Fatalf("benignRefusal(403, %q) = %q, want %q", tc.body, got, tc.want)
			}
			for _, status := range []int{404, 422, 500} {
				if got := benignRefusal(&APIError{Status: status, Body: tc.body}); got != "" {
					t.Fatalf("benignRefusal(%d, %q) = %q, want not benign", status, tc.body, got)
				}
			}
		})
	}
}

func TestBenignRefusal_IsATableNotProse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{"unrelated 403", &APIError{Status: 403, Body: `{"message":"Resource not accessible by integration"}`}, false},
		{"empty body", &APIError{Status: 403}, false},
		{"404", &APIError{Status: 404, Body: `{"message":"Not Found"}`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := benignRefusal(tc.err) != ""; got != tc.want {
				t.Fatalf("benignRefusal(%+v) benign = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	for _, entry := range benignRerunRefusals {
		if entry.Name == "" || entry.Status == 0 || entry.Body == "" {
			t.Errorf("benign refusal %+v must name itself and pin a status and a body fragment", entry)
		}
		if got := benignRefusal(&APIError{Status: entry.Status, Body: entry.Body}); got != entry.Name {
			t.Errorf("benignRefusal for %+v = %q, want %q", entry, got, entry.Name)
		}
	}
}
