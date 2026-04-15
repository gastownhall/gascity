package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestSaveQuotaState_WriteError verifies that saveQuotaState returns an error
// when the target directory is read-only and temp file creation fails.
func TestSaveQuotaState_WriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory test not reliable on Windows")
	}

	tmp := t.TempDir()
	readOnlyDir := filepath.Join(tmp, "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Make directory read-only so file creation inside it fails.
	if err := os.Chmod(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(readOnlyDir, 0o755) //nolint:errcheck // best-effort cleanup
	})

	quotaPath := filepath.Join(readOnlyDir, "quota.json")
	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable},
		},
	}

	err := saveQuotaState(quotaPath, state)
	if err == nil {
		t.Fatal("saveQuotaState should fail when directory is read-only")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "writing temp quota state file") && !strings.Contains(errMsg, "creating quota state dir") {
		t.Errorf("error should describe the I/O failure, got: %s", errMsg)
	}
}
