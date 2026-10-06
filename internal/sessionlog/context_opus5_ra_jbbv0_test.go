package sessionlog

import (
	"testing"

	"github.com/gastownhall/gascity/internal/modelwindow"
)

// TestOpus5IsNativelyOneMillion pins the regression behind ra-jbbv0 at the
// sessionlog boundary.
//
// Opus 5 ships a 1M context window natively — there is no 200K Opus 5 variant,
// and it is the CLI's default Opus. ModelContextWindow originally matched only
// the bare family word "opus" and returned the 200K default for it, so an agent
// actually being served Opus 5 had its utilization gauge computed against a
// denominator 5x too small. Measured consequence in the incident: a session
// peaking at 771,916 tokens reported 386% and the ADVISORY/URGENT steer
// saturated, losing all ability to discriminate near the real ceiling.
//
// The window table itself now lives in internal/modelwindow — the single source
// of truth shared with the CLI context-pressure injector — and it carries the
// "opus-5" marker. This test remains as the guard on the sessionlog delegation:
// it asserts the projection callers actually reach still resolves Opus 5 to 1M,
// so a future change to ModelContextWindow cannot reintroduce the 200K
// denominator without going red here.
func TestOpus5IsNativelyOneMillion(t *testing.T) {
	for _, id := range []string{
		"claude-opus-5",
		"opus-5",
		"claude-opus-5[1m]", // suffix is redundant for Opus 5, must not regress
	} {
		if got := ModelContextWindow(id); got != modelwindow.Million {
			t.Errorf("ModelContextWindow(%q) = %d, want %d (Opus 5 is natively 1M)", id, got, modelwindow.Million)
		}
	}
}

// TestPreExistingWindowsUnchanged guards the blast radius of the resolution
// above: Opus 5 must not capture any model that is not Opus 5, and every other
// family/suffix resolution must reach callers intact.
//
// The modern Claude variants below resolve to 1M without the "[1m]" suffix —
// that is their plain default, and the provider echoes the model ID back
// without the launch flag, so a session log only ever carries the bare form.
// Older variants (Opus 4.5 and earlier, Haiku) stay at the conservative 200K
// default, which is also what pins the "opus-5" marker against swallowing
// "opus-4-5" by substring.
func TestPreExistingWindowsUnchanged(t *testing.T) {
	cases := map[string]int{
		"claude-opus-4-8":               modelwindow.Million,
		"claude-opus-4-7":               modelwindow.Million,
		"claude-opus-4-8[1m]":           modelwindow.Million,
		"claude-sonnet-5":               modelwindow.Million,
		"claude-sonnet-4-6":             modelwindow.Million,
		"claude-opus-4-5-20251101":      modelwindow.Default,
		"claude-haiku-4-5-20251001":     modelwindow.Default,
		"claude-haiku-4-5-20251001[1m]": modelwindow.Million,
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
