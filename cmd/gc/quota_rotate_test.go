package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// selectLRUAccount — pure function tests
// ---------------------------------------------------------------------------

// TestSelectLRUAccount_PicksOldest verifies that the account with the oldest
// last_used timestamp is selected from the available accounts.
func TestSelectLRUAccount_PicksOldest(t *testing.T) {
	available := []account.Account{
		{Handle: "work1", ConfigDir: "/config/work1"},
		{Handle: "work2", ConfigDir: "/config/work2"},
		{Handle: "work3", ConfigDir: "/config/work3"},
	}
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T14:00:00Z"},
			"work2": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"}, // oldest
			"work3": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T13:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	got, err := selectLRUAccount(available, state, clk)
	if err != nil {
		t.Fatalf("selectLRUAccount: unexpected error: %v", err)
	}
	if got.Handle != "work2" {
		t.Errorf("selected handle = %q, want %q (oldest last_used)", got.Handle, "work2")
	}
}

// TestSelectLRUAccount_NeverUsed verifies that an account with no last_used
// (empty string) is prioritized over accounts that have been used.
func TestSelectLRUAccount_NeverUsed(t *testing.T) {
	available := []account.Account{
		{Handle: "work1", ConfigDir: "/config/work1"},
		{Handle: "work2", ConfigDir: "/config/work2"},
	}
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"},
			// work2 has no entry in state → never used
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	got, err := selectLRUAccount(available, state, clk)
	if err != nil {
		t.Fatalf("selectLRUAccount: unexpected error: %v", err)
	}
	if got.Handle != "work2" {
		t.Errorf("selected handle = %q, want %q (never used should be prioritized)", got.Handle, "work2")
	}
}

// TestSelectLRUAccount_TieBreaking verifies that when two accounts have the
// same last_used timestamp, selection is deterministic (alphabetical by handle).
func TestSelectLRUAccount_TieBreaking(t *testing.T) {
	available := []account.Account{
		{Handle: "beta", ConfigDir: "/config/beta"},
		{Handle: "alpha", ConfigDir: "/config/alpha"},
	}
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"alpha": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"},
			"beta":  {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	got, err := selectLRUAccount(available, state, clk)
	if err != nil {
		t.Fatalf("selectLRUAccount: unexpected error: %v", err)
	}
	if got.Handle != "alpha" {
		t.Errorf("selected handle = %q, want %q (alphabetical tie-breaking)", got.Handle, "alpha")
	}
}

// TestSelectLRUAccount_SingleAvailable verifies that when only one account is
// available, it is returned.
func TestSelectLRUAccount_SingleAvailable(t *testing.T) {
	available := []account.Account{
		{Handle: "work1", ConfigDir: "/config/work1"},
	}
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	got, err := selectLRUAccount(available, state, clk)
	if err != nil {
		t.Fatalf("selectLRUAccount: unexpected error: %v", err)
	}
	if got.Handle != "work1" {
		t.Errorf("selected handle = %q, want %q", got.Handle, "work1")
	}
}

// ---------------------------------------------------------------------------
// doQuotaRotate — full rotation tests with FakeTmuxOps
// ---------------------------------------------------------------------------

// TestDoQuotaRotate_Success verifies that all limited sessions are rotated
// to available accounts and the state is updated to available.
func TestDoQuotaRotate_Success(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	newState, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// work1 should now be available (rotated successfully).
	acctState, ok := newState.Accounts["work1"]
	if !ok {
		t.Fatal("expected 'work1' in updated state")
	}
	if acctState.Status != config.QuotaStatusAvailable {
		t.Errorf("work1 status = %q, want %q", acctState.Status, config.QuotaStatusAvailable)
	}
}

// TestDoQuotaRotate_SetsEnvAndRespawns verifies that for each rotated session,
// SetEnv is called with the new CLAUDE_CONFIG_DIR and then RespawnPane is called.
func TestDoQuotaRotate_SetsEnvAndRespawns(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// After rotation, the session's CLAUDE_CONFIG_DIR should point to work2.
	got := panes["session-1"].Env["CLAUDE_CONFIG_DIR"]
	if got != "/config/work2" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q (should be rotated to work2)", got, "/config/work2")
	}
}

// TestDoQuotaRotate_PartialFailure verifies that when one respawn fails,
// other sessions still succeed and the failed session retains status=limited.
func TestDoQuotaRotate_PartialFailure(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
		account.Account{Handle: "work3", ConfigDir: "/config/work3"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
			RespawnErr: nil, // will succeed
		},
		"session-2": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
			RespawnErr: fmt.Errorf("tmux: session not found"), // will fail
		},
	}
	ops := FakeTmuxOps(panes)
	// Both work1 sessions are limited; work2 and work3 are available.
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	newState, _, err := doQuotaRotate(ops, state, reg, clk)
	// Partial failure should still return some result; error may or may not be nil
	// depending on implementation. The key check is the state.
	_ = err

	if newState == nil {
		t.Fatal("doQuotaRotate returned nil state on partial failure")
	}

	// work1 should still be limited (at least one pane respawn failed).
	acctState, ok := newState.Accounts["work1"]
	if !ok {
		t.Fatal("expected 'work1' in state after partial failure")
	}
	if acctState.Status != config.QuotaStatusLimited {
		t.Errorf("work1 status = %q, want %q (failed respawn should retain limited)", acctState.Status, config.QuotaStatusLimited)
	}
}

// TestDoQuotaRotate_PartialFailure_ExitCode verifies that a partial failure
// returns an error indicating partial failure (for non-zero exit code).
func TestDoQuotaRotate_PartialFailure_ExitCode(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
		account.Account{Handle: "work3", ConfigDir: "/config/work3"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
			RespawnErr: nil,
		},
		"session-2": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
			RespawnErr: fmt.Errorf("tmux: session not found"),
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, _, err := doQuotaRotate(ops, state, reg, clk)
	if err == nil {
		t.Error("expected non-nil error for partial failure (non-zero exit code)")
	}
}

// TestDoQuotaRotate_AllLimited verifies that when all accounts are limited,
// an error with the exact PRD message is returned.
func TestDoQuotaRotate_AllLimited(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, _, err := doQuotaRotate(ops, state, reg, clk)
	if err == nil {
		t.Fatal("expected error when all accounts are limited")
	}

	// Exact match per PRD (Gap 9.8: PRD-specified messages use exact matching).
	want := "error: all registered accounts are rate-limited; no rotation possible."
	if err.Error() != want {
		t.Errorf("error = %q, want exact %q", err.Error(), want)
	}
}

// TestDoQuotaRotate_NoAccounts verifies that an empty registry produces an
// error with the exact PRD message.
func TestDoQuotaRotate_NoAccounts(t *testing.T) {
	reg := account.TestRegistry(t) // empty registry
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, _, err := doQuotaRotate(ops, state, reg, clk)
	if err == nil {
		t.Fatal("expected error when no accounts are registered")
	}

	// Exact match per PRD (Gap 9.8).
	want := "error: no accounts registered. Run gc account add to register at least one account."
	if err.Error() != want {
		t.Errorf("error = %q, want exact %q", err.Error(), want)
	}
}

// TestDoQuotaRotate_OneAvailableMultipleLimited verifies that a single
// available account is assigned to all limited sessions.
func TestDoQuotaRotate_OneAvailableMultipleLimited(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
		account.Account{Handle: "work3", ConfigDir: "/config/work3"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
		"session-2": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work3": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T10:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	newState, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// Both limited sessions should have been reassigned to work3.
	// Verify work3's config dir was set on both sessions.
	for _, sessName := range []string{"session-1", "session-2"} {
		got := panes[sessName].Env["CLAUDE_CONFIG_DIR"]
		if got != "/config/work3" {
			t.Errorf("%s CLAUDE_CONFIG_DIR = %q, want %q", sessName, got, "/config/work3")
		}
	}

	// work3 should have updated last_used.
	acctState, ok := newState.Accounts["work3"]
	if !ok {
		t.Fatal("expected 'work3' in updated state")
	}
	if acctState.LastUsed == "" {
		t.Error("work3 last_used should be updated after rotation")
	}
}

// TestDoQuotaRotate_OrphanedEntry verifies that a handle present in quota.json
// but not in accounts.json is warned about and excluded from rotation.
func TestDoQuotaRotate_OrphanedEntry(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1":   {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"orphan1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"}, // not in registry
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, warnings, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// Expect a warning about the orphaned entry.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "orphan1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning 'orphan1', got warnings: %v", warnings)
	}
}

// TestDoQuotaRotate_NoLimited verifies that when no sessions are limited,
// rotation is a no-op.
func TestDoQuotaRotate_NoLimited(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	_, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error for no-op: %v", err)
	}
}

// TestDoQuotaRotate_UpdatesLastUsed verifies that last_used is updated to the
// clock time after a successful rotation.
func TestDoQuotaRotate_UpdatesLastUsed(t *testing.T) {
	rotateTime := time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: rotateTime}

	newState, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// work2 should have been used for rotation → last_used updated.
	acctState, ok := newState.Accounts["work2"]
	if !ok {
		t.Fatal("expected 'work2' in updated state (used for rotation)")
	}
	want := rotateTime.Format(time.RFC3339)
	if acctState.LastUsed != want {
		t.Errorf("work2 last_used = %q, want %q", acctState.LastUsed, want)
	}
}

// TestDoQuotaRotate_CooldownNotAvailable verifies that accounts with
// status=cooldown are NOT selected for rotation (treated same as limited).
// This tests Gap 9.7 from the implementation plan.
func TestDoQuotaRotate_CooldownNotAvailable(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
		account.Account{Handle: "work3", ConfigDir: "/config/work3"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusCooldown, LimitedAt: "2026-04-07T12:00:00Z"}, // cooldown — not available
			"work3": {Status: config.QuotaStatusAvailable, LastUsed: "2026-04-07T10:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	newState, _, err := doQuotaRotate(ops, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate: unexpected error: %v", err)
	}

	// work2 (cooldown) should NOT have been selected — work3 should be used.
	got := panes["session-1"].Env["CLAUDE_CONFIG_DIR"]
	if got != "/config/work3" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q (cooldown account should be skipped)", got, "/config/work3")
	}

	// work2 should still be in cooldown.
	acctState, ok := newState.Accounts["work2"]
	if !ok {
		t.Fatal("expected 'work2' preserved in state")
	}
	if acctState.Status != config.QuotaStatusCooldown {
		t.Errorf("work2 status = %q, want %q", acctState.Status, config.QuotaStatusCooldown)
	}
}

// TestDoQuotaRotate_AtomicStateWrite verifies that the state reflects ONLY
// successful rotations — failed sessions retain limited status.
func TestDoQuotaRotate_AtomicStateWrite(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
		account.Account{Handle: "work3", ConfigDir: "/config/work3"},
	)
	panes := map[string]*FakePane{
		"session-1": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
			RespawnErr: nil, // succeeds
		},
		"session-2": {
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
			RespawnErr: fmt.Errorf("tmux: session not found"), // fails
		},
	}
	ops := FakeTmuxOps(panes)
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
		},
	}
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 15, 0, 0, 0, time.UTC)}

	newState, _, _ := doQuotaRotate(ops, state, reg, clk)
	if newState == nil {
		t.Fatal("doQuotaRotate returned nil state")
	}

	// work2 should still be limited (respawn failed).
	acctState, ok := newState.Accounts["work2"]
	if !ok {
		t.Fatal("expected 'work2' in state after partial failure")
	}
	if acctState.Status != config.QuotaStatusLimited {
		t.Errorf("work2 status = %q, want %q (failed respawn retains limited)", acctState.Status, config.QuotaStatusLimited)
	}
}
