package sessionlog

import "testing"

// TestOpus5IsNativelyOneMillion pins the regression behind ra-jbbv0.
//
// Opus 5 ships a 1M context window natively — there is no 200K Opus 5 variant,
// and it is the CLI's default Opus. ModelContextWindow matches only the bare
// family word "opus" and returns the 200K default for it, so an agent actually
// being served Opus 5 has its utilization gauge computed against a denominator
// 5x too small. Measured consequence in the incident: a session peaking at
// 771,916 tokens reported 386% and the ADVISORY/URGENT steer saturated, losing
// all ability to discriminate near the real ceiling.
//
// This test FAILS on current upstream main. That is the point: it is the
// falsifiable case, demonstrated red before any fix is allowed to count as
// green.
func TestOpus5IsNativelyOneMillion(t *testing.T) {
	for _, id := range []string{
		"claude-opus-5",
		"opus-5",
		"claude-opus-5[1m]", // suffix is redundant for Opus 5, must not regress
	} {
		if got := ModelContextWindow(id); got != millionTokenWindow {
			t.Errorf("ModelContextWindow(%q) = %d, want %d (Opus 5 is natively 1M)", id, got, millionTokenWindow)
		}
	}
}

// TestPreExistingWindowsUnchanged guards the blast radius of the fix above:
// the native-1M shortcut must not capture any model that is not Opus 5, and
// must leave every existing family/suffix resolution exactly as it was.
func TestPreExistingWindowsUnchanged(t *testing.T) {
	cases := map[string]int{
		"claude-opus-4-8":               200_000,
		"claude-opus-4-7":               200_000,
		"claude-opus-4-8[1m]":           millionTokenWindow,
		"claude-sonnet-5":               200_000, // pre-existing drift; out of this bead's scope (tracked upstream by #4527)
		"claude-sonnet-4-6":             200_000,
		"claude-haiku-4-5-20251001":     200_000,
		"claude-haiku-4-5-20251001[1m]": millionTokenWindow,
		"gemini-2.5-pro":                1_000_000,
		"gpt-4o-2024-08-06":             128_000,
		"gpt-5-20260101":                258_000,
		"codex-mini-latest":             258_000,
		"gpt-4-turbo":                   128_000,
		"unknown-model-xyz":             0,
		"":                              0,
	}
	for id, want := range cases {
		if got := ModelContextWindow(id); got != want {
			t.Errorf("ModelContextWindow(%q) = %d, want %d", id, got, want)
		}
	}
}
