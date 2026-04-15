package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
)

// TestResolveAccountEnv_InjectsConfigDir verifies that a valid account with
// an existing, readable config_dir resolves to that config_dir path.
func TestResolveAccountEnv_InjectsConfigDir(t *testing.T) {
	dir := t.TempDir()
	reg := account.TestRegistry(t, account.Account{
		Handle:    "work1",
		ConfigDir: dir,
	})

	configDir, err := resolveAccountEnv(reg, "work1", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if configDir != dir {
		t.Errorf("configDir = %q, want %q", configDir, dir)
	}
}

// TestResolveAccountEnv_PreFlightFail_MissingDir verifies that when the
// resolved account's config_dir does not exist, an error is returned that
// names both the handle and the path.
func TestResolveAccountEnv_PreFlightFail_MissingDir(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "nonexistent")
	reg := account.TestRegistry(t, account.Account{
		Handle:    "work1",
		ConfigDir: missingDir,
	})

	_, err := resolveAccountEnv(reg, "work1", "", "")
	if err == nil {
		t.Fatal("expected error for missing config_dir, got nil")
	}
	if !strings.Contains(err.Error(), "work1") {
		t.Errorf("error should mention handle \"work1\": %v", err)
	}
}

// TestResolveAccountEnv_PreFlightFail_NotReadable verifies that when the
// resolved account's config_dir exists but is not readable, an error is
// returned that names the handle.
func TestResolveAccountEnv_PreFlightFail_NotReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 not effective on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions")
	}

	dir := t.TempDir()
	unreadable := filepath.Join(dir, "locked")
	if err := os.Mkdir(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	reg := account.TestRegistry(t, account.Account{
		Handle:    "work1",
		ConfigDir: unreadable,
	})

	_, err := resolveAccountEnv(reg, "work1", "", "")
	if err == nil {
		t.Fatal("expected error for unreadable config_dir, got nil")
	}
	if !strings.Contains(err.Error(), "work1") {
		t.Errorf("error should mention handle \"work1\": %v", err)
	}
}

// TestResolveAccountEnv_NoAccount verifies that when all handle inputs are
// empty and the registry has no default, an empty string is returned with
// no error (graceful no-op).
func TestResolveAccountEnv_NoAccount(t *testing.T) {
	reg := account.TestRegistry(t) // empty registry

	configDir, err := resolveAccountEnv(reg, "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if configDir != "" {
		t.Errorf("configDir = %q, want empty string", configDir)
	}
}

// TestResolveAccountEnv_UnknownHandle verifies that when the resolved handle
// is not found in the registry, an error is returned naming the unknown handle.
func TestResolveAccountEnv_UnknownHandle(t *testing.T) {
	reg := account.TestRegistry(t, account.Account{
		Handle:    "work1",
		ConfigDir: t.TempDir(),
	})

	_, err := resolveAccountEnv(reg, "nonexistent", "", "")
	if err == nil {
		t.Fatal("expected error for unknown handle, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention handle \"nonexistent\": %v", err)
	}
}
