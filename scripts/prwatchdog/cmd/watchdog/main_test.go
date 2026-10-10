package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
