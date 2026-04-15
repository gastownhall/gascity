package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// matchesRateLimitPattern — pure function tests
// ---------------------------------------------------------------------------

// TestMatchesRateLimitPattern_Claude verifies that a Claude-style rate-limit
// error message matches the claude provider's default patterns.
func TestMatchesRateLimitPattern_Claude(t *testing.T) {
	output := "Your account has reached its rate limit. Please wait before making more requests."
	patterns := []string{"rate limit", "too many requests", "usage limit"}
	if !matchesRateLimitPattern(output, patterns) {
		t.Error("expected match for Claude rate-limit message")
	}
}

// TestMatchesRateLimitPattern_Generic verifies that a generic "rate limit exceeded"
// message matches a simple substring pattern.
func TestMatchesRateLimitPattern_Generic(t *testing.T) {
	output := "Error: rate limit exceeded — retry after 60s"
	patterns := []string{"rate limit"}
	if !matchesRateLimitPattern(output, patterns) {
		t.Error("expected match for generic rate-limit message")
	}
}

// TestMatchesRateLimitPattern_Regex verifies that regex patterns work.
func TestMatchesRateLimitPattern_Regex(t *testing.T) {
	output := "Error: usage limit has been reached for this billing period."
	patterns := []string{"usage limit.*reached"}
	if !matchesRateLimitPattern(output, patterns) {
		t.Error("expected regex match for 'usage limit.*reached'")
	}
}

// TestMatchesRateLimitPattern_NoMatch verifies that normal output does not
// match rate-limit patterns.
func TestMatchesRateLimitPattern_NoMatch(t *testing.T) {
	output := "Task completed successfully.\nAll tests passed."
	patterns := []string{"rate limit", "too many requests", "usage limit"}
	if matchesRateLimitPattern(output, patterns) {
		t.Error("expected no match for normal output")
	}
}

// TestMatchesRateLimitPattern_EmptyPatterns verifies that an empty patterns
// list never matches and does not crash.
func TestMatchesRateLimitPattern_EmptyPatterns(t *testing.T) {
	output := "Error: rate limit exceeded"
	if matchesRateLimitPattern(output, nil) {
		t.Error("expected no match for nil patterns")
	}
	if matchesRateLimitPattern(output, []string{}) {
		t.Error("expected no match for empty patterns slice")
	}
}

// TestMatchesRateLimitPattern_InvalidRegex verifies that an invalid regex
// pattern is skipped without crashing (the function should not panic).
func TestMatchesRateLimitPattern_InvalidRegex(_ *testing.T) {
	output := "Error: rate limit exceeded"
	// "[invalid" is an invalid regex — unclosed bracket.
	patterns := []string{"[invalid"}
	// Should not panic; invalid regex is skipped.
	_ = matchesRateLimitPattern(output, patterns)
}

// TestMatchesRateLimitPattern_MultiLine verifies that a pattern appearing
// on a later line in multi-line output is still detected.
func TestMatchesRateLimitPattern_MultiLine(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "normal output line"
	}
	lines[24] = "Error: rate limit exceeded — please wait."
	output := strings.Join(lines, "\n")
	patterns := []string{"rate limit"}
	if !matchesRateLimitPattern(output, patterns) {
		t.Error("expected match for pattern on line 25 of 30")
	}
}

// TestMatchesRateLimitPattern_PartialLine verifies that a pattern matching
// a substring within a longer line is detected.
func TestMatchesRateLimitPattern_PartialLine(t *testing.T) {
	output := "2026-04-07T12:00:00Z ERROR [api] Your account has reached its rate limit (429). Retry-After: 60"
	patterns := []string{"rate limit"}
	if !matchesRateLimitPattern(output, patterns) {
		t.Error("expected match for partial line substring")
	}
}

// ---------------------------------------------------------------------------
// extractResetsAt — pure function tests
// ---------------------------------------------------------------------------

// TestExtractResetsAt_Found verifies that an RFC3339 timestamp in rate-limit
// output is extracted correctly.
func TestExtractResetsAt_Found(t *testing.T) {
	output := "Rate limit exceeded. Resets at: 2026-04-07T14:30:00Z. Please wait."
	got := extractResetsAt(output)
	if got == "" {
		t.Fatal("expected non-empty resets_at timestamp")
	}
	// Verify the extracted value is parseable as RFC3339.
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("extracted timestamp %q is not valid RFC3339: %v", got, err)
	}
}

// TestExtractResetsAt_NotFound verifies that output without a timestamp
// returns an empty string.
func TestExtractResetsAt_NotFound(t *testing.T) {
	output := "Rate limit exceeded. Please try again later."
	got := extractResetsAt(output)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// doQuotaScan — full scan tests with FakeTmuxOps
// ---------------------------------------------------------------------------

// TestDoQuotaScan_DetectsLimited verifies that a pane with rate-limit output
// results in the associated account's status being set to "limited".
func TestDoQuotaScan_DetectsLimited(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "Error: rate limit exceeded. Please try again later.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}
	if state == nil {
		t.Fatal("doQuotaScan returned nil state")
	}

	acctState, ok := state.Accounts["work1"]
	if !ok {
		t.Fatal("expected account 'work1' in quota state")
	}
	if acctState.Status != config.QuotaStatusLimited {
		t.Errorf("status = %q, want %q", acctState.Status, config.QuotaStatusLimited)
	}
}

// TestDoQuotaScan_SetsLimitedAt verifies that limited_at is set to the scan
// time provided by the clock.
func TestDoQuotaScan_SetsLimitedAt(t *testing.T) {
	scanTime := time.Date(2026, 4, 7, 14, 30, 0, 0, time.UTC)
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
	clk := &clock.Fake{Time: scanTime}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	acctState := state.Accounts["work1"]
	want := scanTime.Format(time.RFC3339)
	if acctState.LimitedAt != want {
		t.Errorf("limited_at = %q, want %q", acctState.LimitedAt, want)
	}
}

// TestDoQuotaScan_ParsesResetsAt verifies that if the rate-limit output
// contains a reset timestamp, resets_at is populated.
func TestDoQuotaScan_ParsesResetsAt(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "Rate limit exceeded. Resets at: 2026-04-07T15:00:00Z",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 14, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	acctState := state.Accounts["work1"]
	if acctState.ResetsAt == "" {
		t.Error("expected non-empty resets_at")
	}
	// Verify the extracted value is parseable as RFC3339.
	if _, err := time.Parse(time.RFC3339, acctState.ResetsAt); err != nil {
		t.Errorf("resets_at %q is not valid RFC3339: %v", acctState.ResetsAt, err)
	}
}

// TestDoQuotaScan_NoMatchNotModified verifies that a pane with normal output
// does not cause the associated account to appear in the quota state.
func TestDoQuotaScan_NoMatchNotModified(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "Build succeeded. All tests passed.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	if _, ok := state.Accounts["work1"]; ok {
		t.Error("expected account 'work1' NOT in quota state when no pattern matched")
	}
}

// TestDoQuotaScan_PaneClosesMidScan verifies that if CapturePane returns an
// error for one pane, that pane is skipped with a warning and others are
// still scanned.
func TestDoQuotaScan_PaneClosesMidScan(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	// "coder" will be present but "reviewer" will be missing from panes
	// so CapturePane will fail for it when ListPanes returns it.
	panes := map[string]*FakePane{
		"coder": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	// Manually create ops to simulate a pane that disappears.
	// ListPanes includes "reviewer" but CapturePane fails for it.
	ops := FakeTmuxOps(panes)
	originalList := ops.ListPanes
	ops.ListPanes = func() ([]PaneInfo, error) {
		listed, err := originalList()
		if err != nil {
			return nil, err
		}
		// Add a phantom pane that will fail CapturePane.
		listed = append(listed, PaneInfo{SessionName: "reviewer", PaneID: "%reviewer"})
		return listed, nil
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// work1 should still be detected as limited.
	if _, ok := state.Accounts["work1"]; !ok {
		t.Error("expected account 'work1' in quota state despite reviewer pane failure")
	}

	// A warning about the failed pane should be emitted.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "reviewer") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning 'reviewer', got warnings: %v", warnings)
	}
}

// TestDoQuotaScan_MapsByConfigDir verifies that sessions are mapped to
// accounts by looking up the CLAUDE_CONFIG_DIR env variable.
func TestDoQuotaScan_MapsByConfigDir(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-a": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
		"session-b": {
			Output: "All good.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// work2 should be limited (session-a had rate-limit output with work2's config dir).
	if acct, ok := state.Accounts["work2"]; !ok {
		t.Error("expected account 'work2' in quota state (session-a maps to work2)")
	} else if acct.Status != config.QuotaStatusLimited {
		t.Errorf("work2 status = %q, want %q", acct.Status, config.QuotaStatusLimited)
	}

	// work1 should NOT be limited (session-b had normal output).
	if _, ok := state.Accounts["work1"]; ok {
		t.Error("expected account 'work1' NOT in quota state (no rate-limit match)")
	}
}

// TestDoQuotaScan_EmptyPane verifies that a pane with no output produces
// no match and no error.
func TestDoQuotaScan_EmptyPane(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"coder": {
			Output: "",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	if _, ok := state.Accounts["work1"]; ok {
		t.Error("expected no match for empty pane output")
	}
}

// TestDoQuotaScan_LessThan30Lines verifies that panes with fewer than 30 lines
// have all available lines checked.
func TestDoQuotaScan_LessThan30Lines(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	// Only 5 lines, last one has rate-limit message.
	output := "line1\nline2\nline3\nline4\nError: rate limit exceeded"
	panes := map[string]*FakePane{
		"coder": {
			Output: output,
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	if _, ok := state.Accounts["work1"]; !ok {
		t.Error("expected match even with fewer than 30 lines")
	}
}

// TestDoQuotaScan_UnmappedSession verifies that a session without
// CLAUDE_CONFIG_DIR set is skipped with a warning.
func TestDoQuotaScan_UnmappedSession(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)
	panes := map[string]*FakePane{
		"orphan": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{}, // no CLAUDE_CONFIG_DIR
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	state, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// No accounts should be marked.
	if len(state.Accounts) != 0 {
		t.Errorf("expected empty accounts, got %d", len(state.Accounts))
	}

	// A warning about the unmapped session should be emitted.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "orphan") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning 'orphan', got warnings: %v", warnings)
	}
}

// TestDoQuotaScan_TmuxNotRunning verifies that when tmux is not running,
// doQuotaScan returns an immediate error with the exact PRD error message.
func TestDoQuotaScan_TmuxNotRunning(t *testing.T) {
	reg := account.TestRegistry(t)
	ops := FakeTmuxOps(map[string]*FakePane{}) // empty panes → IsRunning=false
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	_, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err == nil {
		t.Fatal("expected error when tmux is not running")
	}

	wantMsg := "tmux is not running"
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), wantMsg)
	}
}

// TestDoQuotaScan_EmptyRateLimitPatterns_Warning verifies that when a provider
// has empty patterns, the exact PRD warning is emitted.
func TestDoQuotaScan_EmptyRateLimitPatterns_Warning(t *testing.T) {
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

	// Provider with empty patterns list.
	providerPatterns := map[string][]string{"claude": {}}
	state, warnings, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// No accounts should be marked limited with empty patterns.
	if _, ok := state.Accounts["work1"]; ok {
		t.Error("expected no match with empty patterns")
	}

	// The exact PRD warning should be emitted.
	wantWarning := "provider claude has no RateLimitPatterns \u2014 skipping pattern scan for its sessions"
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
