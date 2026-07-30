package redstreak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestClientListRunsRequestsConfiguredScheduledWorkflow(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.URL.Path != "/repos/gastownhall/gascity/actions/workflows/nightly.yml/runs" {
			t.Errorf("path = %q, want Nightly workflow runs endpoint", r.URL.Path)
		}
		if got := r.URL.Query().Get("event"); got != "schedule" {
			t.Errorf("event query = %q, want schedule", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflow_runs":[{
			"id":501,
			"event":"schedule",
			"conclusion":"success",
			"run_started_at":"2026-07-30T01:00:00Z",
			"html_url":"https://example.test/runs/501"
		}]}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient("test-token", WithEndpoint(server.URL), WithHTTPClient(server.Client()))
	monitor := config.GitHubRunMonitor{
		Owner:        "gastownhall",
		Repo:         "gascity",
		WorkflowFile: "nightly.yml",
		Event:        "schedule",
	}
	runs, err := client.ListRuns(context.Background(), monitor)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if !sawRequest {
		t.Fatal("ListRuns made no HTTP request")
	}
	if len(runs) != 1 || runs[0].ID != 501 || runs[0].Event != "schedule" {
		t.Fatalf("runs = %#v, want scheduled run 501", runs)
	}
}

func TestClientListJobsReadsTheAggregateJobsOwnConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/gastownhall/gascity/actions/runs/501/jobs" {
			t.Errorf("path = %q, want run jobs endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[
			{"name":"Tier B acceptance tests","conclusion":"success"},
			{"name":"Nightly summary","conclusion":"failure"}
		]}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient("test-token", WithEndpoint(server.URL), WithHTTPClient(server.Client()))
	monitor := config.GitHubRunMonitor{
		Owner: "gastownhall",
		Repo:  "gascity",
	}
	jobs, err := client.ListJobs(context.Background(), monitor, 501)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %#v, want two", jobs)
	}
	if jobs[1].Name != "Nightly summary" || jobs[1].Conclusion != "failure" {
		t.Fatalf("aggregate job = %#v, want Nightly summary failure", jobs[1])
	}
}

func TestClientRejectsMalformedAPIDataAndCommandFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{
			name:       "malformed data",
			statusCode: http.StatusOK,
			body:       `{"workflow_runs":[{"id":"not-a-number"}]}`,
			want:       "decode",
		},
		{
			name:       "github command failure",
			statusCode: http.StatusBadGateway,
			body:       `{"message":"upstream unavailable"}`,
			want:       "502",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)

			client := NewClient("test-token", WithEndpoint(server.URL), WithHTTPClient(server.Client()))
			_, err := client.ListRuns(context.Background(), config.GitHubRunMonitor{
				Owner:        "gastownhall",
				Repo:         "gascity",
				WorkflowFile: "nightly.yml",
			})
			if err == nil {
				t.Fatal("ListRuns() error = nil, want failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("ListRuns() error = %v, want %q", err, tc.want)
			}
		})
	}
}
