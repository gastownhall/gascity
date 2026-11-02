package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func TestSessionCacheAgeBanner(t *testing.T) {
	// The session list view names the STATE column it renders.
	stateBanner := sessionCacheAgeBanner(20)
	for _, want := range []string{"cache age: 20s", "STATE may lag", "verify", "transcript", "bead state"} {
		if !strings.Contains(stateBanner, want) {
			t.Errorf("list banner %q missing %q", stateBanner, want)
		}
	}
	if strings.Contains(stateBanner, "reconciler may be lagging") {
		t.Errorf("session banner should not reuse the generic text: %q", stateBanner)
	}
}

func sessionListBannerOutput(t *testing.T, ageSeconds float64) string {
	t.Helper()
	var stdout bytes.Buffer
	code := renderSessionListFromAPI(api.CachedRead[[]api.SessionListView]{
		AgeSeconds: ageSeconds,
		Body: []api.SessionListView{{
			ID:         "gc-abc",
			Template:   "worker",
			State:      "asleep",
			CreatedAt:  time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC),
			LastActive: time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC),
		}},
	}, "", "", false, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("renderSessionListFromAPI = %d, want 0", code)
	}
	return stdout.String()
}

// TestRenderSessionListStateBannerWakeWindow pins the lowered threshold (30 -> 15)
// together with the STATE-specific reword: the banner now fires in the 15-30s
// wake-lag window (previously silent) and names the STATE column, while staying
// quiet for a fresh cache.
func TestRenderSessionListStateBannerWakeWindow(t *testing.T) {
	// 20s: inside the newly covered window (silent under the old 30s cutoff).
	out := sessionListBannerOutput(t, 20)
	if !strings.Contains(out, "STATE may lag the runtime") {
		t.Errorf("age=20s: want STATE banner, got:\n%s", out)
	}
	// 10s: below threshold, still no banner (low-noise).
	quiet := sessionListBannerOutput(t, 10)
	if strings.Contains(quiet, "cache age:") {
		t.Errorf("age=10s: want no banner, got:\n%s", quiet)
	}
}

func sessionPeekBannerOutput(t *testing.T, ageSeconds float64) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := renderSessionPeekFromAPI(api.CachedRead[api.SessionView]{
		AgeSeconds: ageSeconds,
		Body: api.SessionView{
			ID:         "gc-abc",
			LastOutput: "recent pane output\n",
		},
	}, "gc-abc", 50, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("renderSessionPeekFromAPI = %d, want 0", code)
	}
	return stdout.String()
}

// TestRenderSessionPeekFromAPIHasNoCacheAgeFooter pins that human peek output
// never carries the cache-age footer. The server reads pane output live from
// the runtime (peekHandle.Peek), so the bead cache's age says nothing about its
// freshness; a footer would falsely claim the output is cached and would make
// the API route's human output differ from the local route's (#8453).
func TestRenderSessionPeekFromAPIHasNoCacheAgeFooter(t *testing.T) {
	for _, age := range []float64{0, 10, 20, 120} {
		out := sessionPeekBannerOutput(t, age)
		if out != "recent pane output\n" {
			t.Errorf("age=%.0fs: peek stdout = %q, want only the live pane output", age, out)
		}
	}
}

// TestRenderSessionListFromAPIJSONHasNoBanner pins the wire-clean invariant for
// the list --json path: it returns before footer emission, so even at a cache
// age well past the banner threshold the JSON output must never carry the human
// "cache age:" footer (the _cache_age_s field is the only JSON age signal). This
// guards against a later refactor moving the footer above the early return.
func TestRenderSessionListFromAPIJSONHasNoBanner(t *testing.T) {
	var stdout bytes.Buffer
	code := renderSessionListFromAPI(api.CachedRead[[]api.SessionListView]{
		AgeSeconds: 20,
		Body: []api.SessionListView{{
			ID:       "gc-abc",
			Template: "worker",
			State:    "asleep",
		}},
	}, "", "", true, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("renderSessionListFromAPI = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), "cache age:") {
		t.Errorf("json list output must not contain the human cache-age footer, got:\n%s", stdout.String())
	}
}

// TestRenderSessionPeekFromAPIJSONHasNoBanner mirrors the list assertion for the
// peek --json path: it returns before footer emission, so no "cache age:" footer
// may leak into JSON even when the cache age is past the banner threshold.
func TestRenderSessionPeekFromAPIJSONHasNoBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := renderSessionPeekFromAPI(api.CachedRead[api.SessionView]{
		AgeSeconds: 20,
		Body: api.SessionView{
			ID:         "gc-abc",
			LastOutput: "recent pane output\n",
		},
	}, "gc-abc", 50, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("renderSessionPeekFromAPI = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), "cache age:") {
		t.Errorf("json peek output must not contain the human cache-age footer, got:\n%s", stdout.String())
	}
}
