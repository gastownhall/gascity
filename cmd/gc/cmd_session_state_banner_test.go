package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// footerProvokingRefresh is a cache refresh report that, with a 75 s age,
// would print the cache-age footer on any read that prints one.
var footerProvokingRefresh = api.CacheRefresh{IntervalS: 30, State: api.CacheRefreshScheduled}

func sessionPeekBannerOutput(t *testing.T, ageSeconds float64) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := renderSessionPeekFromAPI(api.CachedRead[api.SessionView]{
		AgeSeconds: ageSeconds,
		Refresh:    footerProvokingRefresh,
		Body: api.SessionView{
			ID:         "gc-abc",
			LastOutput: "recent pane output\n",
		},
	}, "gc-abc", 50, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("renderSessionPeekFromAPI = %d, want 0", code)
	}
	return stdout.String(), stderr.String()
}

// TestRenderSessionPeekFromAPIHasNoCacheAgeFooter pins that human peek output
// never carries the cache-age footer, on either stream. The server reads pane
// output live from the runtime (peekHandle.Peek), so the bead cache's age says
// nothing about its freshness; a footer would falsely claim the output is
// cached (#8453).
func TestRenderSessionPeekFromAPIHasNoCacheAgeFooter(t *testing.T) {
	for _, age := range []float64{0, 10, 75, 300} {
		stdout, stderr := sessionPeekBannerOutput(t, age)
		if stdout != "recent pane output\n" {
			t.Errorf("age=%.0fs: peek stdout = %q, want only the live pane output", age, stdout)
		}
		assertNoCacheAgeFooter(t, stdout, stderr)
	}
}

// TestRenderSessionListFromAPIJSONHasNoBanner pins the wire-clean invariant for
// the list --json path: even at a cache age past the footer's bound, neither
// stream carries the human cache-age footer (the _cache_age_s field is the
// only JSON age signal). This guards against a later refactor moving the
// footer above the early return.
func TestRenderSessionListFromAPIJSONHasNoBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := renderSessionListFromAPI(api.CachedRead[[]api.SessionListView]{
		AgeSeconds: 75,
		Refresh:    footerProvokingRefresh,
		Body: []api.SessionListView{{
			ID:       "gc-abc",
			Template: "worker",
			State:    "asleep",
		}},
	}, "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("renderSessionListFromAPI = %d, want 0", code)
	}
	assertNoCacheAgeFooter(t, stdout.String(), stderr.String())
}

// TestRenderSessionPeekFromAPIJSONHasNoBanner mirrors the list assertion for the
// peek --json path.
func TestRenderSessionPeekFromAPIJSONHasNoBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := renderSessionPeekFromAPI(api.CachedRead[api.SessionView]{
		AgeSeconds: 75,
		Refresh:    footerProvokingRefresh,
		Body: api.SessionView{
			ID:         "gc-abc",
			LastOutput: "recent pane output\n",
		},
	}, "gc-abc", 50, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("renderSessionPeekFromAPI = %d, want 0", code)
	}
	assertNoCacheAgeFooter(t, stdout.String(), stderr.String())
}
