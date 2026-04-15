package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// selectLRUAccount selects the least-recently-used account from available.
// Accounts with no last_used entry (never used) are prioritized over all
// used accounts. Ties are broken alphabetically by handle for determinism.
// Returns an error if available is empty.
func selectLRUAccount(available []account.Account, state *config.QuotaState, _ clock.Clock) (account.Account, error) {
	if len(available) == 0 {
		return account.Account{}, fmt.Errorf("no available accounts for LRU selection")
	}

	// Sort by (last_used ascending, handle ascending).
	// Never-used accounts (empty last_used or absent from state) sort first.
	type candidate struct {
		acct     account.Account
		lastUsed string
	}
	candidates := make([]candidate, len(available))
	for i, acct := range available {
		lu := ""
		if as, ok := state.Accounts[acct.Handle]; ok {
			lu = as.LastUsed
		}
		candidates[i] = candidate{acct: acct, lastUsed: lu}
	}

	sort.Slice(candidates, func(i, j int) bool {
		li, lj := candidates[i].lastUsed, candidates[j].lastUsed
		// Never-used (empty) sorts before any timestamp.
		if li == "" && lj != "" {
			return true
		}
		if li != "" && lj == "" {
			return false
		}
		if li != lj {
			return li < lj
		}
		// Tie-break alphabetically by handle.
		return candidates[i].acct.Handle < candidates[j].acct.Handle
	})

	return candidates[0].acct, nil
}

// doQuotaRotate performs quota rotation for all limited sessions.
// For each tmux pane whose current account is limited, it selects the
// least-recently-used available account, updates the session's
// CLAUDE_CONFIG_DIR environment variable, and respawns the pane.
//
// Returns the updated quota state, a list of warnings (e.g. orphaned
// entries), and an error. Partial failures are not fatal — successfully
// rotated sessions are updated in the state while failed sessions retain
// their limited status. A non-nil error is returned if any respawn fails.
func doQuotaRotate(tmux TmuxOps, state *config.QuotaState, reg account.Registry, clk clock.Clock) (*config.QuotaState, []string, error) {
	var warnings []string

	// Check for empty registry.
	if len(reg.Accounts) == 0 {
		return nil, warnings, fmt.Errorf("error: no accounts registered. Run gc account add to register at least one account.") //nolint:revive,staticcheck // PRD-specified user-facing message
	}

	// Build handle→account and configDir→handle lookups.
	handleToAccount := make(map[string]account.Account)
	configDirToHandle := make(map[string]string)
	for _, acct := range reg.Accounts {
		handleToAccount[acct.Handle] = acct
		if acct.ConfigDir != "" {
			configDirToHandle[acct.ConfigDir] = acct.Handle
		}
	}

	// Identify orphaned entries (in state but not in registry) and warn.
	for handle := range state.Accounts {
		if _, ok := handleToAccount[handle]; !ok {
			warnings = append(warnings, fmt.Sprintf("quota entry for unknown account %s is stale and will be ignored", handle))
		}
	}

	// Classify accounts: available vs limited/cooldown.
	var availableAccounts []account.Account
	for _, acct := range reg.Accounts {
		as, inState := state.Accounts[acct.Handle]
		if !inState || as.Status == config.QuotaStatusAvailable {
			availableAccounts = append(availableAccounts, acct)
		}
		// limited and cooldown accounts are NOT available for rotation.
	}

	// Find limited sessions by scanning tmux panes.
	panes, err := tmux.ListPanes()
	if err != nil {
		return state, warnings, fmt.Errorf("listing tmux panes: %w", err)
	}

	// Collect limited sessions: session name → limited handle.
	type limitedSession struct {
		sessionName string
		handle      string
	}
	var limitedSessions []limitedSession
	for _, pane := range panes {
		configDir, err := tmux.ShowEnv(pane.SessionName, "CLAUDE_CONFIG_DIR")
		if err != nil || configDir == "" {
			continue
		}
		handle, ok := configDirToHandle[configDir]
		if !ok {
			continue
		}
		as, inState := state.Accounts[handle]
		if inState && as.Status == config.QuotaStatusLimited {
			limitedSessions = append(limitedSessions, limitedSession{
				sessionName: pane.SessionName,
				handle:      handle,
			})
		}
	}

	// No limited sessions → no-op.
	if len(limitedSessions) == 0 {
		return state, warnings, nil
	}

	// All accounts limited (no available for rotation)?
	if len(availableAccounts) == 0 {
		return nil, warnings, fmt.Errorf("error: all registered accounts are rate-limited; no rotation possible.") //nolint:revive,staticcheck // PRD-specified user-facing message
	}

	// Sort limited sessions by name for deterministic processing order.
	sort.Slice(limitedSessions, func(i, j int) bool {
		return limitedSessions[i].sessionName < limitedSessions[j].sessionName
	})

	// Deep copy state for updates.
	newState := &config.QuotaState{
		Accounts: make(map[string]config.QuotaAccountState),
	}
	for k, v := range state.Accounts {
		newState.Accounts[k] = v
	}

	// Track which handles had at least one respawn failure.
	handleFailed := make(map[string]bool)
	now := clk.Now().Format(time.RFC3339)
	var respawnErrors []string

	for _, ls := range limitedSessions {
		// Select LRU available account for this session.
		target, err := selectLRUAccount(availableAccounts, newState, clk)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("no available account for session %q: %v", ls.sessionName, err))
			continue
		}

		// Set the new CLAUDE_CONFIG_DIR on the session.
		if err := tmux.SetEnv(ls.sessionName, "CLAUDE_CONFIG_DIR", target.ConfigDir); err != nil {
			handleFailed[ls.handle] = true
			respawnErrors = append(respawnErrors, fmt.Sprintf("session %q: SetEnv failed: %v", ls.sessionName, err))
			continue
		}

		// Respawn the pane.
		if err := tmux.RespawnPane(ls.sessionName); err != nil {
			handleFailed[ls.handle] = true
			respawnErrors = append(respawnErrors, fmt.Sprintf("session %q: RespawnPane failed: %v", ls.sessionName, err))
			continue
		}

		// Update the target account's last_used.
		as := newState.Accounts[target.Handle]
		as.Status = config.QuotaStatusAvailable
		as.LastUsed = now
		newState.Accounts[target.Handle] = as
	}

	// Update limited account statuses: only mark as available if no pane
	// for that handle failed.
	for _, ls := range limitedSessions {
		if handleFailed[ls.handle] {
			// At least one pane for this handle failed — retain limited.
			continue
		}
		// All panes for this handle succeeded (or it was rotated away from).
		as := newState.Accounts[ls.handle]
		as.Status = config.QuotaStatusAvailable
		as.LimitedAt = ""
		newState.Accounts[ls.handle] = as
	}

	if len(respawnErrors) > 0 {
		return newState, warnings, fmt.Errorf("partial rotation failure: %d of %d sessions failed", len(respawnErrors), len(limitedSessions))
	}
	return newState, warnings, nil
}
