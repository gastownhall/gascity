package main

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// matchesRateLimitPattern reports whether any pattern in patterns matches the
// output string. Each pattern is compiled as a case-insensitive regexp.
// Invalid regex patterns are silently skipped (no panic).
func matchesRateLimitPattern(output string, patterns []string) bool {
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			// Invalid regex — skip without crashing.
			continue
		}
		if re.MatchString(output) {
			return true
		}
	}
	return false
}

// rfc3339Re matches RFC3339 timestamps like 2026-04-07T14:30:00Z.
var rfc3339Re = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})`)

// extractResetsAt does a best-effort extraction of an RFC3339 reset timestamp
// from rate-limit output. Returns the first valid RFC3339 match or empty string.
func extractResetsAt(output string) string {
	match := rfc3339Re.FindString(output)
	if match == "" {
		return ""
	}
	// Verify it actually parses as RFC3339.
	if _, err := time.Parse(time.RFC3339, match); err != nil {
		return ""
	}
	return match
}

// doQuotaScan scans all tmux panes for rate-limit patterns, mapping each pane
// to an account via the CLAUDE_CONFIG_DIR environment variable.
// providerPatterns maps provider name → list of rate-limit patterns. Providers
// with empty pattern lists emit a per-provider warning (exact PRD message).
// Returns the resulting quota state, a list of warnings, and an error.
func doQuotaScan(tmux TmuxOps, providerPatterns map[string][]string, registry account.Registry, clk clock.Clock) (*config.QuotaState, []string, error) {
	if !tmux.IsRunning() {
		return nil, nil, fmt.Errorf("tmux is not running. gc quota commands require an active tmux server.") //nolint:revive,staticcheck // PRD-specified user-facing message
	}

	var warnings []string

	// Emit per-provider warnings for empty pattern lists and merge
	// non-empty patterns into a flat list for matching.
	var allPatterns []string
	providerNames := make([]string, 0, len(providerPatterns))
	for name := range providerPatterns {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames) // deterministic order for warnings

	for _, name := range providerNames {
		patterns := providerPatterns[name]
		if len(patterns) == 0 {
			warnings = append(warnings, fmt.Sprintf("provider %s has no RateLimitPatterns \u2014 skipping pattern scan for its sessions", name))
		} else {
			allPatterns = append(allPatterns, patterns...)
		}
	}

	if len(allPatterns) == 0 && len(providerPatterns) == 0 {
		warnings = append(warnings, "no rate-limit patterns configured; no matches will be detected")
	}

	panes, err := tmux.ListPanes()
	if err != nil {
		return nil, warnings, fmt.Errorf("listing tmux panes: %w", err)
	}

	// Build configDir→handle lookup from registry.
	configDirToHandle := make(map[string]string)
	for _, acct := range registry.Accounts {
		if acct.ConfigDir != "" {
			configDirToHandle[acct.ConfigDir] = acct.Handle
		}
	}

	state := &config.QuotaState{
		Accounts: make(map[string]config.QuotaAccountState),
	}

	for _, pane := range panes {
		// Capture pane output.
		output, err := tmux.CapturePane(pane.SessionName, 30)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping pane %q: capture failed: %v", pane.SessionName, err))
			continue
		}

		// Read CLAUDE_CONFIG_DIR from the session's environment.
		configDir, err := tmux.ShowEnv(pane.SessionName, "CLAUDE_CONFIG_DIR")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping pane %q: cannot read CLAUDE_CONFIG_DIR: %v", pane.SessionName, err))
			continue
		}
		if configDir == "" {
			warnings = append(warnings, fmt.Sprintf("skipping pane %q: CLAUDE_CONFIG_DIR not set", pane.SessionName))
			continue
		}

		// Map config dir to account handle.
		handle, ok := configDirToHandle[configDir]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("skipping pane %q: config dir %q does not match any registered account", pane.SessionName, configDir))
			continue
		}

		// Check for rate-limit patterns (merged from all non-empty providers).
		if matchesRateLimitPattern(output, allPatterns) {
			state.Accounts[handle] = config.QuotaAccountState{
				Status:    config.QuotaStatusLimited,
				LimitedAt: clk.Now().Format(time.RFC3339),
				ResetsAt:  extractResetsAt(output),
			}
		}
	}

	return state, warnings, nil
}
