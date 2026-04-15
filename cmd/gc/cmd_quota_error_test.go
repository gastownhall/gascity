package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestDoQuotaClearCmd_AllNoAccounts verifies that doQuotaClearCmd --all
// with an empty quota state (no accounts) succeeds with exit code 0 and
// reports "all accounts cleared to available".
func TestDoQuotaClearCmd_AllNoAccounts(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")
	accountsPath := filepath.Join(gcDir, "accounts.json")

	// Pre-seed empty quota state (no accounts at all).
	state := &config.QuotaState{
		Accounts: make(map[string]config.QuotaAccountState),
	}
	if err := saveQuotaState(quotaPath, state); err != nil {
		t.Fatal(err)
	}

	// Pre-seed an empty account registry (no accounts registered).
	if err := os.WriteFile(accountsPath, []byte(`{"default":"","accounts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doQuotaClearCmd("", true, false, quotaPath, accountsPath, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "all accounts cleared to available") {
		t.Errorf("stdout should contain %q, got: %s", "all accounts cleared to available", out)
	}

	// Verify the quota state is empty on disk.
	loaded, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Accounts) != 0 {
		t.Errorf("expected 0 accounts in quota state after clear --all on empty, got %d", len(loaded.Accounts))
	}
}
