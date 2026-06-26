package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestSessionStateCacheAgeBanner(t *testing.T) {
	got := sessionStateCacheAgeBanner(20)
	for _, want := range []string{"cache age: 20s", "STATE", "verify", "transcript", "bead state"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "reconciler may be lagging") {
		t.Errorf("STATE banner should not reuse the generic text: %q", got)
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
// the peek view. peek reuses sessionStateCacheAgeBanner under the same
// session-scoped sessionStateCacheAgeBannerThresholdSeconds, so the STATE banner
// must fire in the 15-30s wake-lag window and name STATE, and stay silent for a
// fresh cache.
func TestRenderSessionPeekStateBannerWakeWindow(t *testing.T) {
	// 20s: inside the session-scoped window — banner fires and names STATE.
	out := sessionPeekBannerOutput(t, 20)
	if !strings.Contains(out, "STATE may lag the runtime") {
		t.Errorf("age=20s: want STATE banner on peek path, got:\n%s", out)
	}
	// 10s: below threshold, no banner.
	quiet := sessionPeekBannerOutput(t, 10)
	if strings.Contains(quiet, "cache age:") {
		t.Errorf("age=10s: want no banner on peek path, got:\n%s", quiet)
	}
}
