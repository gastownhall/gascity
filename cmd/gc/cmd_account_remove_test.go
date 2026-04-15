package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

// TestAccountRemove_ClearsQuotaJSON verifies that gc account remove deletes the
// handle's entry from quota.json when it exists.
func TestAccountRemove_ClearsQuotaJSON(t *testing.T) {
	// Set up a minimal city directory.
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create city.toml so resolveCity can find it.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[city]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Point resolveCity to this directory.
	t.Setenv("GC_CITY", cityDir)

	// Create a config dir for the account.
	cfgDir := t.TempDir()

	// Seed accounts.json with work1 and work2.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir},
			{Handle: "work2", Email: "w2@example.com", ConfigDir: cfgDir},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Seed quota.json with entries for both handles.
	quotaPath := filepath.Join(gcDir, "quota.json")
	quotaData := map[string]interface{}{
		"accounts": map[string]interface{}{
			"work1": map[string]interface{}{
				"status":     "limited",
				"limited_at": "2026-04-07T12:00:00Z",
			},
			"work2": map[string]interface{}{
				"status": "available",
			},
		},
	}
	raw, err := json.MarshalIndent(quotaData, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quotaPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Run doAccountRemove for work1.
	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", FakeTmuxOps(map[string]*FakePane{}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// Verify quota.json no longer contains work1.
	afterRaw, err := os.ReadFile(quotaPath)
	if err != nil {
		t.Fatalf("reading quota.json after remove: %v", err)
	}
	var afterQuota map[string]json.RawMessage
	if err := json.Unmarshal(afterRaw, &afterQuota); err != nil {
		t.Fatalf("parsing quota.json after remove: %v", err)
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(afterQuota["accounts"], &accounts); err != nil {
		t.Fatalf("parsing quota.json accounts: %v", err)
	}
	if _, ok := accounts["work1"]; ok {
		t.Errorf("quota.json still contains work1 after account remove")
	}
	// work2 should still be present.
	if _, ok := accounts["work2"]; !ok {
		t.Errorf("quota.json does not contain work2 — should be unaffected")
	}
}

// TestAccountRemove_NoQuotaFile verifies that gc account remove succeeds even
// when quota.json does not exist (Level 0 compatibility).
func TestAccountRemove_NoQuotaFile(t *testing.T) {
	// Set up a minimal city directory.
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

	// Seed accounts.json with work1.
	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// No quota.json exists.
	quotaPath := filepath.Join(gcDir, "quota.json")
	if _, err := os.Stat(quotaPath); err == nil {
		t.Fatal("quota.json unexpectedly exists before test")
	}

	// Run doAccountRemove for work1 — should succeed without error.
	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", FakeTmuxOps(map[string]*FakePane{}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// Verify no quota.json was created.
	if _, err := os.Stat(quotaPath); err == nil {
		t.Errorf("quota.json was created during account remove — should remain absent")
	}
}

// TestAccountRemove_ClearsQuotaJSON_MatchesQuotaIOFormat verifies that the
// quota.json written by doAccountRemove is readable by loadQuotaState and
// round-trips correctly. This ensures doAccountRemove's quota I/O is
// consistent with the formal quota I/O layer used by the quota subsystem.
func TestAccountRemove_ClearsQuotaJSON_MatchesQuotaIOFormat(t *testing.T) {
	// Set up a minimal city directory.
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

	// Seed accounts.json with work1 and work2.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", Email: "w1@example.com", ConfigDir: cfgDir},
			{Handle: "work2", Email: "w2@example.com", ConfigDir: cfgDir},
		},
	}
	regPath := citylayout.AccountsFilePath(cityDir)
	if err := account.Save(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Seed quota.json using saveQuotaState (the proper quota I/O layer)
	// to ensure the initial file is in canonical format.
	quotaPath := citylayout.QuotaFilePath(cityDir)
	initialState := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T12:00:00Z",
				ResetsAt:  "2026-04-07T17:00:00Z",
			},
			"work2": {
				Status: config.QuotaStatusAvailable,
			},
		},
	}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatal(err)
	}

	// Remove work1.
	var stdout, stderr bytes.Buffer
	code := doAccountRemove("work1", FakeTmuxOps(map[string]*FakePane{}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountRemove returned %d; stderr: %s", code, stderr.String())
	}

	// Load the resulting quota.json using loadQuotaState — this is the
	// round-trip consistency check. If doAccountRemove wrote a format
	// that loadQuotaState cannot parse, this will fail.
	afterState, err := loadQuotaState(quotaPath)
	if err != nil {
		t.Fatalf("loadQuotaState failed after doAccountRemove: %v", err)
	}

	// work1 should be gone.
	if _, ok := afterState.Accounts["work1"]; ok {
		t.Errorf("loadQuotaState still shows work1 after account remove")
	}

	// work2 should be present and unchanged.
	work2, ok := afterState.Accounts["work2"]
	if !ok {
		t.Fatalf("loadQuotaState does not contain work2 — should be unaffected")
	}
	if work2.Status != config.QuotaStatusAvailable {
		t.Errorf("work2 status = %q, want %q", work2.Status, config.QuotaStatusAvailable)
	}
}
