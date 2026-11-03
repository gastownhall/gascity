package main

import (
	"bytes"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestCacheAgeFooter(t *testing.T) {
	refresh := func(interval float64, state api.CacheRefreshState) api.CacheRefresh {
		return api.CacheRefresh{IntervalS: interval, State: state}
	}
	const (
		late       = "(cache age: 75s — more than twice the bead cache's 30s re-scan interval: its re-scans of the store are not landing on schedule)"
		paused     = "(cache age: 75s — the server has paused its bead cache's re-scans while the city is suspended; changes made to the store outside the server since then are not shown)"
		failing    = "(cache age: 75s — the server's bead cache is failing to re-scan its store; the server's log has the error)"
		rebuilding = "(cache age: 75s — the server answered with a response it saved that long ago and is building a fresh one; run the command again for a current view)"
	)
	for _, tc := range []struct {
		name string
		age  float64
		r    api.CacheRefresh
		want string
	}{
		{"zero age", 0, refresh(30, api.CacheRefreshScheduled), ""},
		{"healthy idle 45s at 30s cadence", 45, refresh(30, api.CacheRefreshScheduled), ""},
		{"exactly twice the interval", 60, refresh(30, api.CacheRefreshScheduled), ""},
		{"behind its 30s cadence", 75, refresh(30, api.CacheRefreshScheduled), late},
		{
			"just past twice the interval rounds the age up", 60.4, refresh(30, api.CacheRefreshScheduled),
			"(cache age: 61s — more than twice the bead cache's 30s re-scan interval: its re-scans of the store are not landing on schedule)",
		},
		{"within its 60s cadence", 75, refresh(60, api.CacheRefreshScheduled), ""},
		{"paused under the bound", 50, refresh(30, api.CacheRefreshPaused), ""},
		{"paused past the bound", 75, refresh(30, api.CacheRefreshPaused), paused},
		{"failing", 75, refresh(30, api.CacheRefreshFailing), failing},
		{"rebuilding", 75, refresh(30, api.CacheRefreshRebuilding), rebuilding},
		{"rebuilding without interval under 240s", 100, refresh(0, api.CacheRefreshRebuilding), ""},
		{
			"rebuilding without interval past 240s", 250, refresh(0, api.CacheRefreshRebuilding),
			"(cache age: 250s — the server answered with a response it saved that long ago and is building a fresh one; run the command again for a current view)",
		},
		{"older supervisor under 240s", 200, api.CacheRefresh{}, ""},
		{
			"older supervisor past 240s", 241,
			api.CacheRefresh{},
			"(cache age: 241s — this read is over 240s old, and the server does not report why)",
		},
		{
			"interval without state", 75, refresh(30, ""),
			"(cache age: 75s — this read is over 60s old, and the server does not report why)",
		},
		{"NaN age", math.NaN(), refresh(30, api.CacheRefreshScheduled), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheAgeFooter(tc.age, tc.r)
			if got != tc.want {
				t.Fatalf("cacheAgeFooter(%v, %+v) =\n  %q\nwant\n  %q", tc.age, tc.r, got, tc.want)
			}
			if got == "" {
				return
			}
			if !strings.HasPrefix(got, "(cache age: ") {
				t.Errorf("footer %q does not start with %q", got, "(cache age: ")
			}
			if strings.Contains(got, "reconciler may be lagging") || strings.Contains(got, "\n") {
				t.Errorf("footer %q must be one line without the old guess", got)
			}
		})
	}
}

func TestWriteCacheAgeFooter(t *testing.T) {
	var buf bytes.Buffer
	writeCacheAgeFooter(&buf, 45, api.CacheRefresh{IntervalS: 30, State: api.CacheRefreshScheduled})
	if buf.Len() != 0 {
		t.Fatalf("healthy read wrote %q, want nothing", buf.String())
	}
	writeCacheAgeFooter(&buf, 75, api.CacheRefresh{IntervalS: 30, State: api.CacheRefreshFailing})
	out := buf.String()
	if !strings.HasPrefix(out, "(cache age: 75s") || !strings.HasSuffix(out, ")\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("writeCacheAgeFooter wrote %q, want one newline-terminated footer line", out)
	}
}

// The footer has one source: no other production file in cmd/gc spells it.
func TestCacheAgeFooterHasOneSource(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files under %q", dir)
	}
	for _, f := range files {
		if base := filepath.Base(f); strings.HasSuffix(base, "_test.go") || base == "cache_age_footer.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"(cache age: ", "reconciler may be lagging"} {
			if bytes.Contains(data, []byte(needle)) {
				t.Errorf("%s contains %q: print the cache-age footer through writeCacheAgeFooter", f, needle)
			}
		}
	}
}

// setBehindCacheHeaders sets the bead-cache freshness headers of a read whose
// cache is 75 s old at a 30 s re-scan interval with its re-scans scheduled:
// past the footer's bound of twice the interval.
func setBehindCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("X-GC-Cache-Age-S", "75")
	w.Header().Set("X-GC-Cache-Refresh-Interval-S", "30")
	w.Header().Set("X-GC-Cache-Refresh", "scheduled")
}

// assertCacheAgeFooterOnStderr fails unless the 75 s cache-age footer
// setBehindCacheHeaders provokes is on stderr and no footer is on stdout.
func assertCacheAgeFooterOnStderr(t *testing.T, stdout, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, "(cache age: 75s") {
		t.Errorf("stderr is missing the cache-age footer:\n%s", stderr)
	}
	if strings.Contains(stdout, "(cache age:") {
		t.Errorf("stdout carries a cache-age footer; it belongs on stderr:\n%s", stdout)
	}
}

// assertNoCacheAgeFooter fails if either stream carries a cache-age footer.
func assertNoCacheAgeFooter(t *testing.T, stdout, stderr string) {
	t.Helper()
	for name, out := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(out, "(cache age:") {
			t.Errorf("%s carries a cache-age footer, want none:\n%s", name, out)
		}
	}
}
