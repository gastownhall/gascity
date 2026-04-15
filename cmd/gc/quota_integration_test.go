package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// Step 2.5/2.6 Integration — Scan Persistence Tests
// ---------------------------------------------------------------------------

// TestDoQuotaScan_PersistsToFile verifies that scan results can be persisted
// to disk via saveQuotaState and re-read via loadQuotaState with identical
// state. This tests the PRD acceptance criterion: "Results written to
// quota.json before scan exits."
func TestDoQuotaScan_PersistsToFile(t *testing.T) {
	// Set up registry with two accounts.
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)

	// Simulate tmux panes: work1 is rate-limited, work2 is fine.
	panes := map[string]*FakePane{
		"session-a": {
			Output: "Error: rate limit exceeded. Resets at: 2026-04-07T15:00:00Z",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
		"session-b": {
			Output: "Build succeeded. All tests passed.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
	}
	ops := FakeTmuxOps(panes)
	scanTime := time.Date(2026, 4, 7, 14, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: scanTime}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	// Run the scan.
	state, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// Persist to disk.
	quotaPath := filepath.Join(t.TempDir(), "quota.json")
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatalf("saveQuotaState: unexpected error: %v", err)
	}

	// Re-read from disk.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loadQuotaState: unexpected error: %v", err)
	}

	// Verify work1 is limited with correct fields.
	acctState, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("expected account 'work1' in loaded quota state")
	}
	if acctState.Status != config.QuotaStatusLimited {
		t.Errorf("work1 status = %q, want %q", acctState.Status, config.QuotaStatusLimited)
	}
	wantLimitedAt := scanTime.Format(time.RFC3339)
	if acctState.LimitedAt != wantLimitedAt {
		t.Errorf("work1 limited_at = %q, want %q", acctState.LimitedAt, wantLimitedAt)
	}
	if acctState.ResetsAt == "" {
		t.Error("work1 resets_at should be non-empty (timestamp was in output)")
	}

	// Verify work2 is NOT in the persisted state (no rate-limit match).
	if _, ok := loaded.Accounts["work2"]; ok {
		t.Error("expected account 'work2' NOT in loaded quota state (no rate-limit match)")
	}
}

// TestScanThenRotate_SeparateCommands verifies the two-step design per PRD:
// "gc quota rotate reads from quota.json — does not re-scan." The scan step
// writes state to a file, and the rotate step reads from that same file
// without needing access to the original tmux state.
func TestScanThenRotate_SeparateCommands(t *testing.T) {
	// Set up registry with two accounts.
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)

	// Step 1: SCAN — work1 is rate-limited.
	panes := map[string]*FakePane{
		"session-a": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
		"session-b": {
			Output: "All tests passed.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
	}
	ops := FakeTmuxOps(panes)
	scanTime := time.Date(2026, 4, 7, 14, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: scanTime}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	scanState, _, err := doQuotaScan(ops, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan: unexpected error: %v", err)
	}

	// Persist scan results to quota.json.
	quotaPath := filepath.Join(t.TempDir(), "quota.json")
	if err := saveQuotaState(quotaPath, scanState); err != nil {
		t.Fatalf("saveQuotaState: unexpected error: %v", err)
	}

	// Step 2: ROTATE — reads from persisted file, NOT from tmux.
	// Load the persisted state as a separate operation (simulating a
	// different command invocation).
	rotateState, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loadQuotaState (rotate step): unexpected error: %v", err)
	}

	// Verify the rotate step can see the scan results from the file.
	acctState, ok := rotateState.Accounts["work1"]
	if !ok {
		t.Fatal("rotate step: expected 'work1' in persisted state")
	}
	if acctState.Status != config.QuotaStatusLimited {
		t.Errorf("rotate step: work1 status = %q, want %q", acctState.Status, config.QuotaStatusLimited)
	}

	// work2 should not be present (not rate-limited during scan).
	if _, ok := rotateState.Accounts["work2"]; ok {
		t.Error("rotate step: expected 'work2' NOT in persisted state")
	}

	// Verify the loaded state is structurally identical to what was scanned.
	if len(rotateState.Accounts) != len(scanState.Accounts) {
		t.Errorf("rotate step: loaded %d accounts, scan had %d",
			len(rotateState.Accounts), len(scanState.Accounts))
	}
	for handle, scanAcct := range scanState.Accounts {
		rotateAcct, ok := rotateState.Accounts[handle]
		if !ok {
			t.Errorf("rotate step: missing account %q from scan", handle)
			continue
		}
		if rotateAcct.Status != scanAcct.Status {
			t.Errorf("rotate step: %s status = %q, scan had %q", handle, rotateAcct.Status, scanAcct.Status)
		}
		if rotateAcct.LimitedAt != scanAcct.LimitedAt {
			t.Errorf("rotate step: %s limited_at = %q, scan had %q", handle, rotateAcct.LimitedAt, scanAcct.LimitedAt)
		}
		if rotateAcct.ResetsAt != scanAcct.ResetsAt {
			t.Errorf("rotate step: %s resets_at = %q, scan had %q", handle, rotateAcct.ResetsAt, scanAcct.ResetsAt)
		}
	}
}
