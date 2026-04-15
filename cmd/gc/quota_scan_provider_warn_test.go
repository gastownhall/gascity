package main

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// Per-provider empty RateLimitPatterns warning tests (Step 4.5 / GAP-4)
//
// These tests verify that doQuotaScan emits the exact PRD-specified warning
// message when a provider has an empty RateLimitPatterns list:
//   "provider <name> has no RateLimitPatterns — skipping pattern scan for its sessions"
//
// The doQuotaScan signature is expected to change from:
//   doQuotaScan(tmux TmuxOps, allPatterns []string, ...) → ...
// to:
//   doQuotaScan(tmux TmuxOps, providerPatterns map[string][]string, ...) → ...
// ---------------------------------------------------------------------------

// TestDoQuotaScan_EmptyProviderPatterns_ExactWarning verifies that when a
// specific provider has an empty RateLimitPatterns list, the exact PRD-specified
// warning is emitted. The warning format must be:
//
//	"provider <name> has no RateLimitPatterns — skipping pattern scan for its sessions"
//
// (using em-dash — as specified in the PRD).
func TestDoQuotaScan_EmptyProviderPatterns_ExactWarning(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	// Per-provider patterns: "claude" has patterns, "codex" has empty patterns.
	providerPatterns := map[string][]string{
		"claude": {"rate limit", "429"},
		"codex":  {},
	}

	state, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// The "claude" patterns should still detect the rate-limited pane.
	if state == nil {
		t.Fatal("doQuotaScan returned nil state")
	}
	if _, ok := state.Accounts["work1"]; !ok {
		t.Error("expected account 'work1' in quota state (matched by claude patterns)")
	}

	// Verify the exact PRD warning for the empty-patterns provider.
	wantWarning := "provider codex has no RateLimitPatterns \u2014 skipping pattern scan for its sessions"
	found := false
	for _, w := range warnings {
		if w == wantWarning {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected exact warning %q, got warnings: %v", wantWarning, warnings)
	}
}

// TestDoQuotaScan_AllProvidersHavePatterns_NoEmptyProviderWarning verifies that
// when all providers have non-empty RateLimitPatterns, no per-provider empty
// patterns warning is emitted.
func TestDoQuotaScan_AllProvidersHavePatterns_NoEmptyProviderWarning(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "All good.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	providerPatterns := map[string][]string{
		"claude": {"rate limit", "429"},
		"codex":  {"too many requests"},
	}

	_, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// No provider should trigger an empty-patterns warning.
	for _, w := range warnings {
		if strings.Contains(w, "has no RateLimitPatterns") {
			t.Errorf("unexpected empty-patterns warning: %q", w)
		}
	}
}

// TestDoQuotaScan_MultipleEmptyProviders_EachWarned verifies that when multiple
// providers have empty RateLimitPatterns, each gets its own warning.
func TestDoQuotaScan_MultipleEmptyProviders_EachWarned(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "normal output",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	providerPatterns := map[string][]string{
		"claude": {"rate limit"},
		"codex":  {},
		"gemini": {},
	}

	_, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// Collect per-provider empty-patterns warnings.
	var emptyProviderWarnings []string
	for _, w := range warnings {
		if strings.Contains(w, "has no RateLimitPatterns") {
			emptyProviderWarnings = append(emptyProviderWarnings, w)
		}
	}

	if len(emptyProviderWarnings) != 2 {
		t.Fatalf("expected 2 empty-patterns warnings, got %d: %v", len(emptyProviderWarnings), emptyProviderWarnings)
	}

	// Sort for deterministic comparison.
	sort.Strings(emptyProviderWarnings)
	wantCodex := "provider codex has no RateLimitPatterns \u2014 skipping pattern scan for its sessions"
	wantGemini := "provider gemini has no RateLimitPatterns \u2014 skipping pattern scan for its sessions"
	if emptyProviderWarnings[0] != wantCodex {
		t.Errorf("warning[0] = %q, want %q", emptyProviderWarnings[0], wantCodex)
	}
	if emptyProviderWarnings[1] != wantGemini {
		t.Errorf("warning[1] = %q, want %q", emptyProviderWarnings[1], wantGemini)
	}
}

// TestDoQuotaScan_EmptyProviderPatterns_StillScansOtherProviders verifies that
// even when one provider has empty patterns, panes are still scanned against
// the non-empty providers' patterns. The empty provider warning is informational;
// scanning proceeds with whatever patterns are available.
func TestDoQuotaScan_EmptyProviderPatterns_StillScansOtherProviders(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"sess-a": {
			Output: "Error: rate limit exceeded — please wait",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
		"sess-b": {
			Output: "Build complete. All tests passed.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	// "codex" has empty patterns; "claude" has patterns that will match sess-a.
	providerPatterns := map[string][]string{
		"claude": {"rate limit"},
		"codex":  {},
	}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// work1 should be detected as limited (matched by "claude" patterns).
	acct, ok := state.Accounts["work1"]
	if !ok {
		t.Fatal("expected account 'work1' in quota state despite codex having empty patterns")
	}
	if acct.Status != config.QuotaStatusLimited {
		t.Errorf("work1 status = %q, want %q", acct.Status, config.QuotaStatusLimited)
	}

	// work2 should NOT be in state (no pattern match for normal output).
	if _, ok := state.Accounts["work2"]; ok {
		t.Error("expected account 'work2' NOT in quota state (no rate-limit match)")
	}
}
