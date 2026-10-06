package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestSessionCacheAgeBanner(t *testing.T) {
	// The session list view names the STATE column it renders.
	stateBanner := sessionCacheAgeBanner(20, "STATE")
	for _, want := range []string{"cache age: 20s", "STATE may lag", "verify", "transcript", "bead state"} {
		if !strings.Contains(stateBanner, want) {
			t.Errorf("list banner %q missing %q", stateBanner, want)
		}
	}
	if strings.Contains(stateBanner, "reconciler may be lagging") {
		t.Errorf("session banner should not reuse the generic text: %q", stateBanner)
	}

	// The peek view names the cached pane output it renders, not a STATE column
	// it does not show, so the footer never points operators at an absent column.
	peekBanner := sessionCacheAgeBanner(20, "cached pane output")
	for _, want := range []string{"cache age: 20s", "cached pane output may lag", "verify", "transcript", "bead state"} {
		if !strings.Contains(peekBanner, want) {
			t.Errorf("peek banner %q missing %q", peekBanner, want)
		}
	}
	if strings.Contains(peekBanner, "STATE") {
		t.Errorf("peek banner must not name a STATE column peek does not render: %q", peekBanner)
	}
}

func sessionListBannerOutput(t *testing.T, ageSeconds float64) string {
	t.Helper()
	var stdout bytes.Buffer
	code := renderSessionListFromAPI(api.CachedRead[[]SessionView]{
		AgeSeconds: ageSeconds,
		Body: []SessionView{{
			ID:         "gc-abc",
			Template:   "worker",
			State:      "asleep",
			CreatedAt:  "2026-04-23T10:00:00Z",
			LastActive: "2026-04-23T12:00:00Z",
		}},
	}, false, &stdout)
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

// TestRenderSessionPeekStateBannerWakeWindow mirrors the list-path assertion for
// the peek view. peek shares the session-scoped
// sessionStateCacheAgeBannerThresholdSeconds but names the cached pane output it
// renders rather than a STATE column it does not show, so the banner must fire in
// the 15-30s wake-lag window naming the cached pane output, and stay silent for a
// fresh cache.
func TestRenderSessionPeekStateBannerWakeWindow(t *testing.T) {
	// 20s: inside the session-scoped window — banner fires and names the cached view.
	out := sessionPeekBannerOutput(t, 20)
	if !strings.Contains(out, "cached pane output may lag the runtime") {
		t.Errorf("age=20s: want cached-pane banner on peek path, got:\n%s", out)
	}
	if strings.Contains(out, "STATE") {
		t.Errorf("age=20s: peek banner must not name a STATE column, got:\n%s", out)
	}
	// 10s: below threshold, no banner.
	quiet := sessionPeekBannerOutput(t, 10)
	if strings.Contains(quiet, "cache age:") {
		t.Errorf("age=10s: want no banner on peek path, got:\n%s", quiet)
	}
}

// TestRenderSessionListFromAPIJSONHasNoBanner pins the wire-clean invariant for
// the list --json path: it returns before footer emission, so even at a cache
// age well past the banner threshold the JSON output must never carry the human
// "cache age:" footer (the _cache_age_s field is the only JSON age signal). This
// guards against a later refactor moving the footer above the early return.
func TestRenderSessionListFromAPIJSONHasNoBanner(t *testing.T) {
	var stdout bytes.Buffer
	code := renderSessionListFromAPI(api.CachedRead[[]SessionView]{
		AgeSeconds: 20,
		Body: []SessionView{{
			ID:       "gc-abc",
			Template: "worker",
			State:    "asleep",
		}},
	}, true, &stdout)
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
