package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/scripts/prwatchdog"
)

// handlerTransport serves requests in-process from an http.Handler, so these
// tests exercise the real request/response code without a listening server.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func newTestFetcher(h http.Handler) *githubFetcher {
	return &githubFetcher{
		httpClient: &http.Client{Transport: handlerTransport{h}},
		apiBase:    "https://api.github.test",
		repo:       "gastownhall/gascity",
		token:      "t",
		checkNames: []string{prwatchdog.CheckName, prwatchdog.CIRequiredName},
	}
}

func TestFetchCheckRuns_OneRequestPerPollFilteredToTrackedNames(t *testing.T) {
	var requests atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("check_name"); got != "" {
			t.Errorf("request filtered by check_name=%q; want one unfiltered request", got)
		}
		if r.URL.Path != "/repos/gastownhall/gascity/commits/sha1/check-runs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"total_count":3,"check_runs":[
			{"id":1,"name":"Check","head_sha":"sha1","status":"completed","conclusion":"success"},
			{"id":2,"name":"Unrelated job","head_sha":"sha1","status":"completed","conclusion":"failure"},
			{"id":3,"name":"CI / required","head_sha":"sha1","status":"in_progress"}]}`)
	})

	runs, err := newTestFetcher(h).FetchCheckRuns(context.Background(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("made %d requests for one poll, want 1", n)
	}
	if len(runs) != 2 || runs[0].Name != prwatchdog.CheckName || runs[1].Name != prwatchdog.CIRequiredName {
		t.Fatalf("runs = %+v, want only the tracked Check and CI / required", runs)
	}
}

func TestFetchCheckRuns_Paginates(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			_, _ = fmt.Fprint(w, `{"check_runs":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					_, _ = fmt.Fprint(w, ",")
				}
				_, _ = fmt.Fprintf(w, `{"id":%d,"name":"filler","head_sha":"sha1"}`, i)
			}
			_, _ = fmt.Fprint(w, `]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"check_runs":[{"id":500,"name":"Check","head_sha":"sha1","status":"queued"}]}`)
	})

	runs, err := newTestFetcher(h).FetchCheckRuns(context.Background(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 500 {
		t.Fatalf("runs = %+v, want the Check run from page 2", runs)
	}
}

func TestFetchCheckRuns_RateLimitIsTyped(t *testing.T) {
	reset := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		want    time.Time
	}{
		{"primary 403", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, reset},
		{"primary 429", http.StatusTooManyRequests, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, reset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded for installation."}`)
			})

			_, err := newTestFetcher(h).FetchCheckRuns(context.Background(), "sha1")
			var limited *prwatchdog.RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("err = %v, want *prwatchdog.RateLimitError", err)
			}
			if !limited.Reset.Equal(tc.want) {
				t.Fatalf("Reset = %v, want %v", limited.Reset, tc.want)
			}
		})
	}
}

func TestFetchCheckRuns_RetryAfterIsTyped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	})

	before := time.Now()
	_, err := newTestFetcher(h).FetchCheckRuns(context.Background(), "sha1")
	var limited *prwatchdog.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *prwatchdog.RateLimitError", err)
	}
	if d := limited.Reset.Sub(before); d < 89*time.Second || d > 95*time.Second {
		t.Fatalf("Reset is %v after the request, want ~90s", d)
	}
}

func TestFetchCheckRuns_OtherForbiddenIsNotRateLimit(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})

	_, err := newTestFetcher(h).FetchCheckRuns(context.Background(), "sha1")
	var limited *prwatchdog.RateLimitError
	if err == nil || errors.As(err, &limited) {
		t.Fatalf("err = %v, want a plain (fail-closed) error", err)
	}
}

// Suite shapes below are copied from live fork PR heads (2026-10-31): a
// github-actions suite whose workflow awaits approval is completed with
// conclusion action_required and latest_check_runs_count 0.
const (
	suiteAwaiting     = `{"id":%d,"status":"completed","conclusion":"action_required","latest_check_runs_count":0,"app":{"slug":"github-actions"}}`
	suiteRan          = `{"id":%d,"status":"completed","conclusion":"success","latest_check_runs_count":1,"app":{"slug":"github-actions"}}`
	suiteSkippedOther = `{"id":%d,"status":"completed","conclusion":"skipped","latest_check_runs_count":1,"app":{"slug":"blacksmith-sh"}}`
)

func suitesHandler(t *testing.T, suites ...string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/gastownhall/gascity/commits/sha1/check-suites" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"total_count":%d,"check_suites":[%s]}`, len(suites), strings.Join(suites, ","))
	})
}

func TestFetchAwaitingApproval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suites []string
		want   bool
	}{
		{"awaiting-approval suite among others", []string{fmt.Sprintf(suiteSkippedOther, 1), fmt.Sprintf(suiteRan, 2), fmt.Sprintf(suiteAwaiting, 3)}, true},
		{"no suites", nil, false},
		{"every suite ran", []string{fmt.Sprintf(suiteRan, 1), fmt.Sprintf(suiteSkippedOther, 2)}, false},
		{
			"action_required but it did create check runs",
			[]string{`{"id":1,"status":"completed","conclusion":"action_required","latest_check_runs_count":2,"app":{"slug":"github-actions"}}`},
			false,
		},
		{
			"action_required from an app other than github-actions",
			[]string{`{"id":1,"status":"completed","conclusion":"action_required","latest_check_runs_count":0,"app":{"slug":"blacksmith-sh"}}`},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newTestFetcher(suitesHandler(t, tc.suites...)).FetchAwaitingApproval(context.Background(), "sha1")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("FetchAwaitingApproval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFetchAwaitingApproval_Paginates(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			filler := make([]string, 100)
			for i := range filler {
				filler[i] = fmt.Sprintf(suiteRan, i)
			}
			_, _ = fmt.Fprintf(w, `{"check_suites":[%s]}`, strings.Join(filler, ","))
			return
		}
		_, _ = fmt.Fprintf(w, `{"check_suites":[`+suiteAwaiting+`]}`, 500)
	})

	got, err := newTestFetcher(h).FetchAwaitingApproval(context.Background(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("FetchAwaitingApproval = false, want true from the suite on page 2")
	}
}

func TestFetchAwaitingApproval_RateLimitIsTyped(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	})

	_, err := newTestFetcher(h).FetchAwaitingApproval(context.Background(), "sha1")
	var limited *prwatchdog.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *prwatchdog.RateLimitError", err)
	}
}

func TestFetchAwaitingApproval_OtherErrorsAreReturned(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := newTestFetcher(h).FetchAwaitingApproval(context.Background(), "sha1"); err == nil {
		t.Fatal("err = nil, want the unexpected-status error")
	}
}
