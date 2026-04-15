package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

// TestAccountRemove_WarnsActiveSessions verifies that doAccountRemove emits a
// warning to stderr when active tmux sessions reference the account being
// removed. The warning should list affected session names.
func TestAccountRemove_WarnsActiveSessions(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[city]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	cfgDir1 := t.TempDir()
	cfgDir2 := t.TempDir()

	// Seed accounts.json with work1 and work2.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir1},
			{Handle: "work2", Email: "w2@example.com", ConfigDir: cfgDir2},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// FakeTmuxOps: two sessions, one using work1's config dir.
	panes := map[string]*FakePane{
		"session-a": {Env: map[string]string{"CLAUDE_CONFIG_DIR": cfgDir1}},
		"session-b": {Env: map[string]string{"CLAUDE_CONFIG_DIR": cfgDir2}},
	}
	ops := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// The warning should mention the handle and the affected session.
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "work1") {
		t.Errorf("stderr should mention handle 'work1'; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "session-a") {
		t.Errorf("stderr should mention affected session 'session-a'; got: %s", errMsg)
	}
	// session-b uses work2, so it should NOT appear in the warning.
	if strings.Contains(errMsg, "session-b") {
		t.Errorf("stderr should NOT mention unaffected session 'session-b'; got: %s", errMsg)
	}
	// The account should still have been removed.
	if !strings.Contains(stdout.String(), "account work1 removed") {
		t.Errorf("stdout should confirm removal; got: %s", stdout.String())
	}
}

// TestAccountRemove_NoActiveSessions verifies that no warning is emitted when
// tmux is running but no active sessions reference the account being removed.
func TestAccountRemove_NoActiveSessions(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[city]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	cfgDir1 := t.TempDir()
	cfgDir2 := t.TempDir()

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir1},
			{Handle: "work2", Email: "w2@example.com", ConfigDir: cfgDir2},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// FakeTmuxOps: one session using work2's config dir — not work1.
	panes := map[string]*FakePane{
		"session-b": {Env: map[string]string{"CLAUDE_CONFIG_DIR": cfgDir2}},
	}
	ops := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// No warning should be emitted about active sessions.
	errMsg := stderr.String()
	if strings.Contains(errMsg, "in use by active session") {
		t.Errorf("stderr should NOT contain active session warning; got: %s", errMsg)
	}
}

// TestAccountRemove_TmuxNotRunning_NoWarning verifies that when tmux is not
// running, no warning is emitted and the removal proceeds normally.
func TestAccountRemove_TmuxNotRunning_NoWarning(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[city]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	cfgDir := t.TempDir()

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Empty panes → IsRunning returns false.
	ops := FakeTmuxOps(map[string]*FakePane{})

	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// No warning about active sessions when tmux is not running.
	errMsg := stderr.String()
	if strings.Contains(errMsg, "in use by active session") {
		t.Errorf("stderr should NOT contain active session warning when tmux not running; got: %s", errMsg)
	}
	// Removal should still succeed.
	if !strings.Contains(stdout.String(), "account work1 removed") {
		t.Errorf("stdout should confirm removal; got: %s", stdout.String())
	}
}

// TestAccountRemove_ActiveSessions_StillRemoves verifies that even when active
// sessions reference the account, the account is still removed from
// accounts.json and quota.json. The warning is informational, not blocking.
func TestAccountRemove_ActiveSessions_StillRemoves(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[city]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	cfgDir1 := t.TempDir()
	cfgDir2 := t.TempDir()

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir1},
			{Handle: "work2", Email: "w2@example.com", ConfigDir: cfgDir2},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Seed quota.json with entries for both handles.
	quotaPath := citylayout.QuotaFilePath(cityDir)
	initialState := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusLimited, LimitedAt: "2026-04-07T12:00:00Z"},
			"work2": {Status: config.QuotaStatusAvailable},
		},
	}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatal(err)
	}

	// FakeTmuxOps: session using work1's config dir (active session).
	panes := map[string]*FakePane{
		"session-a": {Env: map[string]string{"CLAUDE_CONFIG_DIR": cfgDir1}},
	}
	ops := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// Verify warning was emitted.
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "work1") || !strings.Contains(errMsg, "session-a") {
		t.Errorf("stderr should warn about active session; got: %s", errMsg)
	}

	// Verify account was removed from accounts.json despite the warning.
	afterReg, err := account.Load(regPath)
	if err != nil {
		t.Fatalf("loading accounts.json after remove: %v", err)
	}
	for _, acct := range afterReg.Accounts {
		if acct.Handle == "work1" {
			t.Errorf("accounts.json still contains work1 after removal with active sessions")
		}
	}

	// Verify quota.json no longer contains work1.
	afterRaw, err := os.ReadFile(quotaPath)
	if err != nil {
		t.Fatalf("reading quota.json after remove: %v", err)
	}
	var afterQuota map[string]json.RawMessage
	if err := json.Unmarshal(afterRaw, &afterQuota); err != nil {
		t.Fatalf("parsing quota.json: %v", err)
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(afterQuota["accounts"], &accounts); err != nil {
		t.Fatalf("parsing quota.json accounts: %v", err)
	}
	if _, ok := accounts["work1"]; ok {
		t.Errorf("quota.json still contains work1 after removal with active sessions")
	}
	// work2 should remain.
	if _, ok := accounts["work2"]; !ok {
		t.Errorf("quota.json missing work2 — should be unaffected")
	}
}
