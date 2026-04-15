package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// newQuotaCmd creates the "gc quota" parent command with subcommands
// for managing per-account quota state and rotation.
func newQuotaCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Manage per-account quota state and rotation",
		Long: `Track provider rate-limit quota across registered accounts.

Use gc quota scan to detect rate-limited sessions, gc quota status to
view current state, gc quota rotate to reassign limited sessions to
available accounts, and gc quota clear for manual remediation.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc quota: missing subcommand (scan, status, rotate, clear)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc quota: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newQuotaScanCmd(stdout, stderr),
		newQuotaStatusCmd(stdout, stderr),
		newQuotaRotateCmd(stdout, stderr),
		newQuotaClearCmd(stdout, stderr),
	)
	return cmd
}

// newQuotaScanCmd creates the "gc quota scan" command.
func newQuotaScanCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Scan tmux sessions for rate-limited accounts",
		Long: `Scan all active tmux sessions' last 30 lines for provider-specific
rate-limit patterns and map sessions to accounts via CLAUDE_CONFIG_DIR.
Results are written to .gc/quota.json.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc quota scan: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			regPath := citylayout.AccountsFilePath(cityPath)
			reg, err := account.Load(regPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc quota scan: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			// Load rate-limit patterns from city config provider.
			patterns := loadRateLimitPatterns(cityPath, stderr)

			quotaPath := citylayout.QuotaFilePath(cityPath)
			tmux := DefaultTmuxOps(filepath.Base(cityPath))
			clk := clock.Real{}

			code := doQuotaScanCmd(tmux, patterns, reg, quotaPath, clk, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
}

// newQuotaStatusCmd creates the "gc quota status" command.
func newQuotaStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Display current quota state per account",
		Long: `Read .gc/quota.json and display per-account quota state including
handle, status, limited_at, and resets_at. Does not modify the file.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc quota status: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			quotaPath := citylayout.QuotaFilePath(cityPath)
			code := doQuotaStatusCmd(quotaPath, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
}

// newQuotaRotateCmd creates the "gc quota rotate" command.
func newQuotaRotateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Reassign rate-limited sessions to available accounts",
		Long: `Acquire an exclusive lock on .gc/quota.json, identify all sessions
whose current account is rate-limited, and reassign each to the
least-recently-used available account by respawning the tmux pane.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc quota rotate: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			regPath := citylayout.AccountsFilePath(cityPath)
			reg, err := account.Load(regPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc quota rotate: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			quotaPath := citylayout.QuotaFilePath(cityPath)
			tmux := DefaultTmuxOps(filepath.Base(cityPath))
			clk := clock.Real{}

			code := doQuotaRotateCmd(tmux, reg, quotaPath, clk, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
}

// newQuotaClearCmd creates the "gc quota clear" command.
func newQuotaClearCmd(stdout, stderr io.Writer) *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{
		Use:   "clear [handle]",
		Short: "Reset account quota status to available",
		Long: `Unconditional operator override for manual remediation. Resets the
specified account (or all accounts with --all) to "available" status
in .gc/quota.json regardless of current state.`,
		Example: `  gc quota clear work1
  gc quota clear --all
  gc quota clear --all --force`,
		RunE: func(_ *cobra.Command, args []string) error {
			handle := ""
			if len(args) > 0 {
				handle = args[0]
			}
			if handle == "" && !all {
				fmt.Fprintln(stderr, "gc quota clear: specify a handle or use --all") //nolint:errcheck // best-effort stderr
				return errExit
			}

			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc quota clear: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			quotaPath := citylayout.QuotaFilePath(cityPath)
			accountsPath := citylayout.AccountsFilePath(cityPath)
			code := doQuotaClearCmd(handle, all, force, quotaPath, accountsPath, stdout, stderr)
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "clear all accounts")
	cmd.Flags().BoolVar(&force, "force", false, "reset file even if corrupted (use with --all)")
	return cmd
}

// loadRateLimitPatterns attempts to load per-provider rate-limit patterns from
// the city config. Returns a map of provider name → patterns. If anything fails
// (no config, no provider), returns a single "default" provider with sensible
// default patterns.
func loadRateLimitPatterns(cityPath string, _ io.Writer) map[string][]string {
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		// Fall back to default patterns if city config is not loadable.
		return map[string][]string{"default": defaultRateLimitPatterns()}
	}

	result := make(map[string][]string)

	// Collect patterns from configured providers.
	for name, spec := range cfg.Providers {
		result[name] = spec.RateLimitPatterns
	}

	// Also check builtin providers (only add if not already configured).
	for name, spec := range config.BuiltinProviders() {
		if _, exists := result[name]; !exists {
			result[name] = spec.RateLimitPatterns
		}
	}

	if len(result) == 0 {
		return map[string][]string{"default": defaultRateLimitPatterns()}
	}

	return result
}

// defaultRateLimitPatterns returns a sensible default set of rate-limit
// patterns when no provider-specific patterns are configured.
func defaultRateLimitPatterns() []string {
	return []string{"rate limit", "429", "quota exceeded"}
}

// doQuotaScanCmd is the testable implementation of "gc quota scan".
// It checks tmux availability, runs the scan, persists results to quotaPath,
// and returns an exit code.
func doQuotaScanCmd(tmux TmuxOps, providerPatterns map[string][]string, reg account.Registry, quotaPath string, clk clock.Clock, stdout, stderr io.Writer) int {
	state, warnings, err := doQuotaScan(tmux, providerPatterns, reg, clk)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Persist results to quota.json before returning (PRD requirement).
	if err := saveQuotaState(quotaPath, state); err != nil {
		fmt.Fprintf(stderr, "gc quota scan: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Summary output.
	limited := 0
	for _, as := range state.Accounts {
		if as.Status == config.QuotaStatusLimited {
			limited++
		}
	}
	if limited > 0 {
		fmt.Fprintf(stdout, "scan complete: %d account(s) rate-limited\n", limited) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintln(stdout, "scan complete: no rate-limited accounts detected") //nolint:errcheck // best-effort stdout
	}

	// GAP-5 Fix: non-zero exit code for partial scan so callers can
	// distinguish a clean scan from a degraded one (PRD §Scenario #27).
	// Results are already persisted above, so the non-zero exit is safe.
	if len(warnings) > 0 {
		return 2
	}
	return 0
}

// doQuotaRotateCmd is the testable implementation of "gc quota rotate".
// It checks tmux availability, acquires the quota lock, performs rotation,
// and returns an exit code.
func doQuotaRotateCmd(tmux TmuxOps, reg account.Registry, quotaPath string, clk clock.Clock, stdout, stderr io.Writer) int {
	// Preflight: tmux must be running.
	if !tmux.IsRunning() {
		fmt.Fprintln(stderr, "error: tmux is not running. gc quota commands require an active tmux server.") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use withQuotaLock to ensure exclusive access and TOCTOU prevention.
	// GAP-6 fix: capture the partial rotation error in a closure variable
	// and return nil from the callback so withQuotaLock always persists
	// the state. This ensures successfully rotated sessions are saved to
	// quota.json even when some pane respawns fail.
	var rotateWarnings []string
	var rotateErr error
	err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
		newState, warnings, rErr := doQuotaRotate(tmux, state, reg, clk)
		rotateWarnings = warnings
		rotateErr = rErr

		// Copy the new state back into the locked state for persistence.
		if newState != nil {
			state.Accounts = newState.Accounts
		}

		// Return nil so withQuotaLock persists state even on partial failure.
		return nil
	})

	for _, w := range rotateWarnings {
		fmt.Fprintf(stderr, "warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}

	if err != nil {
		// Lock/IO error — state was not persisted.
		fmt.Fprintf(stderr, "%v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if rotateErr != nil {
		// Partial failure — state IS persisted, but rotation had errors.
		fmt.Fprintf(stderr, "%v\n", rotateErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintln(stdout, "rotation complete") //nolint:errcheck // best-effort stdout
	return 0
}

// doQuotaClearCmd is the testable implementation of "gc quota clear".
// It resets the specified account (or all accounts) to available status.
// When all is true (without force), it loads the account registry from
// accountsPath and removes orphaned entries not in the registry (GAP-10).
func doQuotaClearCmd(handle string, all bool, force bool, quotaPath string, accountsPath string, stdout, stderr io.Writer) int {
	// Force clear: overwrite with empty state regardless of current content.
	if all && force {
		empty := &config.QuotaState{
			Accounts: make(map[string]config.QuotaAccountState),
		}
		if err := saveQuotaState(quotaPath, empty); err != nil {
			fmt.Fprintf(stderr, "gc quota clear: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, "quota state reset") //nolint:errcheck // best-effort stdout
		return 0
	}

	state, err := loadQuotaState(quotaPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc quota clear: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if all {
		// GAP-10: Load the account registry and build a new map containing
		// only currently-registered handles set to "available". This naturally
		// discards orphaned/stale entries not in the registry.
		reg, regErr := account.Load(accountsPath)
		if regErr != nil {
			fmt.Fprintf(stderr, "gc quota clear: %v\n", regErr) //nolint:errcheck // best-effort stderr
			return 1
		}
		registered := make(map[string]bool, len(reg.Accounts))
		for _, a := range reg.Accounts {
			registered[a.Handle] = true
		}
		newAccounts := make(map[string]config.QuotaAccountState, len(reg.Accounts))
		for h := range state.Accounts {
			if registered[h] {
				newAccounts[h] = config.QuotaAccountState{
					Status: config.QuotaStatusAvailable,
				}
			}
		}
		state.Accounts = newAccounts
	} else {
		// Clear specific account.
		as, ok := state.Accounts[handle]
		if !ok {
			// Account not in quota state — nothing to clear, but not an error.
			as = config.QuotaAccountState{}
		}
		as.Status = config.QuotaStatusAvailable
		as.LimitedAt = ""
		as.ResetsAt = ""
		state.Accounts[handle] = as
	}

	if err := saveQuotaState(quotaPath, state); err != nil {
		fmt.Fprintf(stderr, "gc quota clear: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if all {
		fmt.Fprintln(stdout, "all accounts cleared to available") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "account %s cleared to available\n", handle) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// doQuotaStatusCmd is the testable implementation of "gc quota status".
// It reads and displays quota.json content.
func doQuotaStatusCmd(quotaPath string, stdout, stderr io.Writer) int {
	state, err := loadQuotaState(quotaPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc quota status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if len(state.Accounts) == 0 {
		fmt.Fprintln(stdout, "no quota state") //nolint:errcheck // best-effort stdout
		return 0
	}

	// Sort handles for deterministic output.
	handles := make([]string, 0, len(state.Accounts))
	for h := range state.Accounts {
		handles = append(handles, h)
	}
	sort.Strings(handles)

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HANDLE\tSTATUS\tLIMITED AT\tRESETS AT\tLAST USED") //nolint:errcheck // best-effort stdout
	for _, h := range handles {
		as := state.Accounts[h]
		status := string(as.Status)
		if status == "" {
			status = "available"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h, status, as.LimitedAt, as.ResetsAt, as.LastUsed) //nolint:errcheck // best-effort stdout
	}
	w.Flush() //nolint:errcheck // best-effort flush
	return 0
}
