package main

import (
	"bytes"
	"encoding/json"
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
// Step 4.6 — gc quota scan exit code tests (GAP-5 Fix)
// ---------------------------------------------------------------------------

// TestQuotaScanCmd_PartialScan_NonZeroExit verifies that when panes are skipped
// during a scan (e.g., pane closes mid-scan), doQuotaScanCmd returns a non-zero
// exit code so callers can distinguish a clean scan from a degraded one.
// PRD: "the exit code reflects a partial result (non-zero)"
func TestQuotaScanCmd_PartialScan_NonZeroExit(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/config/work1"},
			{Handle: "work2", ConfigDir: "/config/work2"},
		},
	}

	// Set up FakeTmuxOps with one good pane ("coder") and inject a phantom
	// pane ("reviewer") that will fail CapturePane, simulating a pane that
	// closes between ListPanes and CapturePane.
	panes := map[string]*FakePane{
		"coder": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	originalList := ops.ListPanes
	ops.ListPanes = func() ([]PaneInfo, error) {
		listed, err := originalList()
		if err != nil {
			return nil, err
		}
		// Add a phantom pane that will fail CapturePane (simulates mid-scan close).
		listed = append(listed, PaneInfo{SessionName: "reviewer", PaneID: "%reviewer"})
		return listed, nil
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	var stdout, stderr bytes.Buffer
	code := doQuotaScanCmd(ops, providerPatterns, reg, quotaPath, clk, &stdout, &stderr)

	if code == 0 {
		t.Fatal("expected non-zero exit code for partial scan (pane skipped), got 0")
	}

	// Stderr should contain a warning about the skipped pane.
	if !strings.Contains(stderr.String(), "reviewer") {
		t.Errorf("expected stderr to mention skipped pane 'reviewer', got: %s", stderr.String())
	}
}

// TestQuotaScanCmd_CleanScan_ZeroExit verifies that when all panes are
// scanned successfully (no warnings), doQuotaScanCmd returns exit code 0.
func TestQuotaScanCmd_CleanScan_ZeroExit(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/config/work1"},
			{Handle: "work2", ConfigDir: "/config/work2"},
		},
	}

	panes := map[string]*FakePane{
		"sess-work1": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
		"sess-work2": {
			Output: "Task completed successfully.",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"},
		},
	}
	ops := FakeTmuxOps(panes)
	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	var stdout, stderr bytes.Buffer
	code := doQuotaScanCmd(ops, providerPatterns, reg, quotaPath, clk, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("expected exit code 0 for clean scan, got %d; stderr: %s", code, stderr.String())
	}
}

// TestQuotaScanCmd_PartialScan_StillPersists verifies that even when the scan
// is partial (non-zero exit), the scan results are still persisted to
// quota.json before exiting. PRD: "Results written to quota.json before scan
// exits."
func TestQuotaScanCmd_PartialScan_StillPersists(t *testing.T) {
	tmp := t.TempDir()
	gcDir := filepath.Join(tmp, ".gc")
	if err := os.MkdirAll(gcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(gcDir, "quota.json")

	reg := account.Registry{
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: "/config/work1"},
			{Handle: "work2", ConfigDir: "/config/work2"},
		},
	}

	// "coder" is a real pane that will be scanned. "reviewer" is a phantom
	// pane that will fail CapturePane, triggering a partial scan.
	panes := map[string]*FakePane{
		"coder": {
			Output: "Error: rate limit exceeded",
			Env:    map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"},
		},
	}
	ops := FakeTmuxOps(panes)
	originalList := ops.ListPanes
	ops.ListPanes = func() ([]PaneInfo, error) {
		listed, err := originalList()
		if err != nil {
			return nil, err
		}
		listed = append(listed, PaneInfo{SessionName: "reviewer", PaneID: "%reviewer"})
		return listed, nil
	}

	clk := &clock.Fake{Time: time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)}
	providerPatterns := map[string][]string{"test": {"rate limit"}}

	var stdout, stderr bytes.Buffer
	code := doQuotaScanCmd(ops, providerPatterns, reg, quotaPath, clk, &stdout, &stderr)

	// Should be non-zero (partial scan).
	if code == 0 {
		t.Fatal("expected non-zero exit code for partial scan")
	}

	// Even with non-zero exit, quota.json must have been written.
	data, err := os.ReadFile(quotaPath)
	if err != nil {
		t.Fatalf("quota.json should exist after partial scan: %v", err)
	}

	var loaded config.QuotaState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("quota.json should be valid JSON: %v", err)
	}

	// work1 should be detected as limited despite the partial scan.
	w1, ok := loaded.Accounts["work1"]
	if !ok {
		t.Fatal("work1 should be in quota state even during partial scan")
	}
	if w1.Status != config.QuotaStatusLimited {
		t.Errorf("work1 should be limited, got %q", w1.Status)
	}
}
