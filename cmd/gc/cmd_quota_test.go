package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// Tmux-dependent Go unit tests for gc quota CLI commands (Step 2.8)
// ---------------------------------------------------------------------------

// TestQuotaScanCmd_TmuxNotRunning verifies that gc quota scan fails immediately
// with the correct PRD error message when tmux is not running.
func TestQuotaScanCmd_TmuxNotRunning(t *testing.T) {
	tmp := t.TempDir()
	quotaPath := filepath.Join(tmp, ".gc", "quota.json")

	// Set up a registry with one account.
	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
		},
	}

	tmux := FakeTmuxOps(nil) // nil panes → IsRunning returns false

	var stdout, stderr bytes.Buffer
	code := doQuotaScanCmd(tmux, map[string][]string{"test": {"rate limit"}}, reg, quotaPath, clock.Real{}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code when tmux is not running")
	}
	if !strings.Contains(stderr.String(), "tmux is not running") {
		t.Errorf("expected stderr to contain 'tmux is not running', got: %s", stderr.String())
	}

	// Verify no changes made to quota.json.
	if _, err := os.Stat(quotaPath); !os.IsNotExist(err) {
		t.Error("quota.json should not have been created when tmux is not running")
	}
}

// TestQuotaRotateCmd_TmuxNotRunning verifies that gc quota rotate fails
// immediately with the correct PRD error message when tmux is not running.
func TestQuotaRotateCmd_TmuxNotRunning(t *testing.T) {
	tmp := t.TempDir()
	quotaPath := filepath.Join(tmp, ".gc", "quota.json")

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
		},
	}

	tmux := FakeTmuxOps(nil) // IsRunning returns false

	var stdout, stderr bytes.Buffer
	code := doQuotaRotateCmd(tmux, reg, quotaPath, clock.Real{}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code when tmux is not running")
	}
	if !strings.Contains(stderr.String(), "tmux is not running") {
		t.Errorf("expected stderr to contain 'tmux is not running', got: %s", stderr.String())
	}
}

// TestQuotaRotateCmd_LocksAndWrites verifies that doQuotaRotateCmd uses
// withQuotaLock to ensure exclusive access and writes state atomically
// once at the end. After a successful rotation, the quota.json file
// should contain the updated state.
func TestQuotaRotateCmd_LocksAndWrites(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	// Pre-seed quota state with work1 as limited.
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:00:00Z",
			},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
			{Handle: "work2", ConfigDir: "/tmp/cfg2"},
		},
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	// Set up FakeTmuxOps: work1 session using cfg1 (limited), work2 not present.
	panes := map[string]*FakePane{
		"sess-work1": {
			Output: "rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cfg1"},
		},
	}
	tmux := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doQuotaRotateCmd(tmux, reg, quotaPath, clk, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	// Read the written state and verify.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loading persisted quota state: %v", err)
	}

	// work2 should have been used (available, updated last_used).
	w2, ok := loaded.Accounts["work2"]
	if !ok {
		t.Fatal("work2 should be in quota state after rotation")
	}
	if w2.LastUsed == "" {
		t.Error("work2 last_used should be updated after rotation")
	}

	// work1 should have been cleared (available, not limited).
	w1, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("work1 should be in quota state after rotation")
	}
	if w1.Status != config.QuotaStatusAvailable {
		t.Errorf("work1 should be available after rotation, got %q", w1.Status)
	}

	// Verify the lock file was created (evidence of flock usage).
	lockPath := quotaPath + ".lock"
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		t.Error("expected lock file to exist (evidence of withQuotaLock usage)")
	}
}

// TestQuotaScanCmd_WritesBeforeExit verifies that doQuotaScanCmd persists
// the scan results to quota.json before returning, per PRD requirement
// "Results written to quota.json before scan exits."
func TestQuotaScanCmd_WritesBeforeExit(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
			{Handle: "work2", ConfigDir: "/tmp/cfg2"},
		},
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 14, 0, 0, 0, time.UTC)}

	panes := map[string]*FakePane{
		"sess-work1": {
			Output: "Error: rate limit exceeded. Resets at 2026-04-07T15:00:00Z",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cfg1"},
		},
		"sess-work2": {
			Output: "Task completed successfully.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cfg2"},
		},
	}
	tmux := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doQuotaScanCmd(tmux, map[string][]string{"test": {"rate limit"}}, reg, quotaPath, clk, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	// Verify quota.json exists and has the scan results.
	data, err := os.ReadFile(quotaPath)
	if err != nil {
		t.Fatalf("quota.json should exist after scan: %v", err)
	}

	var loaded config.QuotaState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("quota.json should be valid JSON: %v", err)
	}

	// work1 should be limited.
	w1, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("work1 should be in quota state")
	}
	if w1.Status != config.QuotaStatusLimited {
		t.Errorf("work1 should be limited, got %q", w1.Status)
	}
	if w1.LimitedAt != "2026-04-07T14:00:00Z" {
		t.Errorf("work1 limited_at should match clock, got %q", w1.LimitedAt)
	}
	if w1.ResetsAt != "2026-04-07T15:00:00Z" {
		t.Errorf("work1 resets_at should be parsed, got %q", w1.ResetsAt)
	}

	// work2 should NOT be in state (no rate-limit match).
	if _, ok := loaded.Accounts["work2"]; ok {
		t.Error("work2 should not be in quota state (no rate-limit match)")
	}
}

// TestQuotaClearCmd_SpecificAccount verifies that doQuotaClearCmd resets
// a specific account to available.
func TestQuotaClearCmd_SpecificAccount(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	// Pre-seed with limited state.
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-03T10:00:00Z",
				ResetsAt:  "2026-04-03T11:00:00Z",
			},
			"work2": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-03T10:00:00Z",
			},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaClearCmd("work1", false, false, quotaPath, "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	// Verify: work1 should be available, work2 should still be limited.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatal(err)
	}
	w1 := loaded.Accounts["work1"]
	if w1.Status != config.QuotaStatusAvailable {
		t.Errorf("work1 should be available after clear, got %q", w1.Status)
	}
	w2 := loaded.Accounts["work2"]
	if w2.Status != config.QuotaStatusLimited {
		t.Errorf("work2 should still be limited, got %q", w2.Status)
	}
}

// TestQuotaClearCmd_All verifies that doQuotaClearCmd --all resets all accounts.
func TestQuotaClearCmd_All(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")
	accountsPath := filepath.Join(gcDir, "accounts.json")

	// Set up account registry with both work1 and work2 registered.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: tmp},
			{Handle: "work2", ConfigDir: tmp},
		},
	}
	if err := account.Save(accountsPath, reg); err != nil {
		t.Fatalf("saving accounts.json: %v", err)
	}

	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-03T10:00:00Z"},
			"work2": {Status: config.QuotaStatusCooldown, LimitedAt: "2026-04-03T10:00:00Z"},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaClearCmd("", true, false, quotaPath, accountsPath, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatal(err)
	}

	for handle, as := range loaded.Accounts {
		if as.Status != config.QuotaStatusAvailable {
			t.Errorf("account %s should be available after clear --all, got %q", handle, as.Status)
		}
	}
}

// TestQuotaClearCmd_ForceCorrupted verifies that --all --force resets the
// quota file even when it contains corrupt JSON.
func TestQuotaClearCmd_ForceCorrupted(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	// Write garbage JSON.
	if err := os.WriteFile(quotaPath, []byte("{broken json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaClearCmd("", true, true, quotaPath, "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0 for force clear, got %d; stderr: %s", code, stderr.String())
	}

	// File should now be valid and empty.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("quota.json should be valid after force clear: %v", err)
	}
	if len(loaded.Accounts) != 0 {
		t.Errorf("expected empty accounts after force clear, got %d", len(loaded.Accounts))
	}
}

// TestQuotaStatusCmd_ReadsState verifies that doQuotaStatusCmd reads and
// displays quota.json content correctly.
func TestQuotaStatusCmd_ReadsState(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:00:00Z",
				ResetsAt:  "2026-04-07T11:00:00Z",
				LastUsed:  "2026-04-07T09:50:00Z",
			},
			"work2": {
				Status: config.QuotaStatusAvailable,
			},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaStatusCmd(quotaPath, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "work1") {
		t.Error("output should contain work1")
	}
	if !strings.Contains(out, "limited") {
		t.Error("output should contain 'limited'")
	}
	if !strings.Contains(out, "work2") {
		t.Error("output should contain work2")
	}
	if !strings.Contains(out, "available") {
		t.Error("output should contain 'available'")
	}
}

// TestQuotaStatusCmd_Empty verifies that doQuotaStatusCmd handles missing
// quota.json gracefully with a "no quota state" message.
func TestQuotaStatusCmd_Empty(t *testing.T) {
	tmp := t.TempDir()
	quotaPath := filepath.Join(tmp, ".gc", "quota.json")

	var stdout, stderr bytes.Buffer
	code := doQuotaStatusCmd(quotaPath, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	// Should succeed but show a message about no quota state.
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "no quota state") {
		t.Errorf("expected 'no quota state' message, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestQuotaClearCmd_AlreadyAvailable verifies that clearing an account
// that is already available still succeeds (no precondition checks).
func TestQuotaClearCmd_AlreadyAvailable(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaClearCmd("work1", false, false, quotaPath, "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0 (no precondition checks), got %d; stderr: %s", code, stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Step 6.2 — Partial Rotation Failure State Persistence (GAP-6 Fix)
// ---------------------------------------------------------------------------

// TestQuotaRotateCmd_PartialFailure_PersistsState verifies that when
// doQuotaRotateCmd encounters a partial rotation failure (some pane respawns
// succeed, others fail), the successfully rotated sessions are still persisted
// to quota.json on disk. This is the critical GAP-6 bug: the withQuotaLock
// callback returns the partial error, causing withQuotaLock to skip
// saveQuotaState.
//
// PRD requirement: "successfully rotated sessions are reflected in
// .gc/quota.json" and "the failed session retains status=limited in
// quota.json" during partial failures.
func TestQuotaRotateCmd_PartialFailure_PersistsState(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	// Pre-seed quota state: work1 and work2 are both limited.
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:00:00Z",
			},
			"work2": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T10:05:00Z",
			},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	// Registry: work1, work2 (limited), and work3, work4 (available targets).
	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
			{Handle: "work2", ConfigDir: "/tmp/cfg2"},
			{Handle: "work3", ConfigDir: "/tmp/cfg3"},
			{Handle: "work4", ConfigDir: "/tmp/cfg4"},
		},
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}

	// FakeTmuxOps: two limited sessions. sess-work1 respawn succeeds,
	// sess-work2 respawn fails (simulating partial failure).
	panes := map[string]*FakePane{
		"sess-work1": {
			Output:     "rate limit exceeded",
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cfg1"},
			RespawnErr: nil, // respawn succeeds
		},
		"sess-work2": {
			Output:     "rate limit exceeded",
			Env:        map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/cfg2"},
			RespawnErr: fmt.Errorf("fake respawn error: pane not found"),
		},
	}
	tmux := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doQuotaRotateCmd(tmux, reg, quotaPath, clk, &stdout, &stderr)

	// The command should return non-zero because of partial failure.
	if code == 0 {
		t.Fatal("expected non-zero exit code for partial rotation failure")
	}

	// CRITICAL CHECK: Read quota.json from disk and verify state was persisted
	// despite the partial failure.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loading persisted quota state after partial failure: %v", err)
	}

	// work1's session was successfully rotated → work1 should be available.
	w1, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("work1 should be in persisted quota state after partial rotation")
	}
	if w1.Status != config.QuotaStatusAvailable {
		t.Errorf("work1 should be available after successful rotation, got %q", w1.Status)
	}

	// work2's session failed → work2 should retain limited status.
	w2, ok := loaded.Accounts["work2"]
	if !ok {
		t.Fatal("work2 should be in persisted quota state after partial rotation")
	}
	if w2.Status != config.QuotaStatusLimited {
		t.Errorf("work2 should retain limited status after failed respawn, got %q", w2.Status)
	}

	// The target account (work3 or work4) should have last_used set.
	var targetFound bool
	for _, handle := range []string{"work3", "work4"} {
		if as, ok := loaded.Accounts[handle]; ok && as.LastUsed != "" {
			targetFound = true
			break
		}
	}
	if !targetFound {
		t.Error("at least one target account (work3 or work4) should have last_used set in persisted state")
	}
}

// ---------------------------------------------------------------------------
// Step 6.6 — gc quota clear --all removes stale/orphaned entries (GAP-10)
// ---------------------------------------------------------------------------

// TestQuotaClearCmd_AllRemovesOrphaned verifies that doQuotaClearCmd with
// --all (without --force) removes entries for handles not in the account
// registry, while keeping registered handles reset to "available".
func TestQuotaClearCmd_AllRemovesOrphaned(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")
	accountsPath := filepath.Join(gcDir, "accounts.json")

	// Set up account registry with only "work1" registered.
	reg := account.Registry{
		Default:  "work1",
		Accounts: []account.Account{{Handle: "work1", ConfigDir: tmp}},
	}
	if err := account.Save(accountsPath, reg); err != nil {
		t.Fatalf("saving accounts.json: %v", err)
	}

	// Pre-seed quota.json with entries for "work1" (registered) and
	// "stale1" (NOT registered — orphaned entry).
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-03T10:00:00Z",
				ResetsAt:  "2026-04-03T11:00:00Z",
			},
			"stale1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-03T09:00:00Z",
			},
		},
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// Pass accountsPath so doQuotaClearCmd can determine which handles are
	// currently registered. This is the new parameter added by GAP-10.
	code := doQuotaClearCmd("", true, false, quotaPath, accountsPath, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatal(err)
	}

	// work1 should still be present, reset to "available".
	w1, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("work1 should still be in quota state after clear --all")
	}
	if w1.Status != config.QuotaStatusAvailable {
		t.Errorf("work1 should be available after clear --all, got %q", w1.Status)
	}
	if w1.LimitedAt != "" {
		t.Errorf("work1 LimitedAt should be cleared, got %q", w1.LimitedAt)
	}
	if w1.ResetsAt != "" {
		t.Errorf("work1 ResetsAt should be cleared, got %q", w1.ResetsAt)
	}

	// stale1 should NOT be present — it was orphaned and should be removed.
	if _, ok := loaded.Accounts["stale1"]; ok {
		t.Error("stale1 should have been removed from quota state (orphaned entry)")
	}

	// Verify output message.
	if !strings.Contains(stdout.String(), "all accounts cleared to available") {
		t.Errorf("expected output to contain 'all accounts cleared to available', got %q", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// GAP-11 fix: CMD-level test for rotate with corrupted quota.json
// ---------------------------------------------------------------------------

// TestQuotaRotateCmd_CorruptedQuotaJSON verifies that doQuotaRotateCmd returns
// a non-zero exit code and prints the PRD-specified error message to stderr
// when quota.json contains malformed JSON. The error should guide the user to
// run "gc quota clear --all --force" to recover.
func TestQuotaRotateCmd_CorruptedQuotaJSON(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	// Write malformed content to quota.json.
	if err := os.WriteFile(quotaPath, []byte("{not valid json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/tmp/cfg1"},
		},
	}

	// tmux is running so we reach the withQuotaLock/loadQuotaState path.
	tmux := FakeTmuxOps(map[string]*FakePane{
		"work1": {Output: "$ idle"},
	})

	var stdout, stderr bytes.Buffer
	code := doQuotaRotateCmd(tmux, reg, quotaPath, clock.Real{}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for corrupted quota.json")
	}

	errMsg := stderr.String()
	// PRD (GAP-7): error should contain "malformed".
	if !strings.Contains(errMsg, "malformed") {
		t.Errorf("stderr should contain 'malformed' per PRD, got: %s", errMsg)
	}
	// PRD (GAP-7): error should include the exact recovery command.
	if !strings.Contains(errMsg, "gc quota clear --all --force") {
		t.Errorf("stderr should contain 'gc quota clear --all --force' per PRD, got: %s", errMsg)
	}
}
