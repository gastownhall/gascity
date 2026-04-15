package account

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAccountSave_MkdirAllError verifies that Save returns an error when
// the parent directory cannot be created (e.g., the path component is a file).
func TestAccountSave_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()

	// Create a regular file where MkdirAll expects a directory.
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Attempt to save with the directory path going through the blocker file.
	path := filepath.Join(blocker, "subdir", "accounts.json")
	reg := Registry{
		Default:  "w",
		Accounts: []Account{{Handle: "w", ConfigDir: "/tmp/w"}},
	}

	err := Save(path, reg)
	if err == nil {
		t.Fatal("Save should fail when parent directory cannot be created")
	}

	if !strings.Contains(err.Error(), "creating directory for account registry") {
		t.Errorf("error should mention directory creation failure, got: %s", err.Error())
	}
}

// TestAccountSave_WriteError verifies that Save returns an error when
// the target directory is read-only and temp file creation fails.
func TestAccountSave_WriteError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory test not reliable on Windows")
	}

	tmp := t.TempDir()
	readOnlyDir := filepath.Join(tmp, "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Make directory read-only so temp file creation fails.
	if err := os.Chmod(readOnlyDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(readOnlyDir, 0o755) //nolint:errcheck // best-effort cleanup
	})

	path := filepath.Join(readOnlyDir, "accounts.json")
	reg := Registry{
		Default:  "w",
		Accounts: []Account{{Handle: "w", ConfigDir: "/tmp/w"}},
	}

	err := Save(path, reg)
	if err == nil {
		t.Fatal("Save should fail when directory is read-only")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "creating temp file for account registry") {
		t.Errorf("error should mention temp file creation failure, got: %s", errMsg)
	}
}
