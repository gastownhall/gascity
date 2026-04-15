package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
)

// TestDoAccountAdd_DuplicateHandle verifies that doAccountAdd returns exit
// code 1 and reports an error when the handle already exists in the registry.
func TestDoAccountAdd_DuplicateHandle(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("creating .gc dir: %v", err)
	}

	// Pre-seed a registry with an existing "work1" account.
	regPath := filepath.Join(gcDir, "accounts.json")
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: cityDir},
		},
	}
	if err := account.Save(regPath, reg); err != nil {
		t.Fatalf("pre-seeding registry: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	exitCode := doAccountAdd("work1", "dup@example.com", "duplicate", cityDir, &stdout, &stderr)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "already registered") {
		t.Errorf("stderr should contain %q, got: %s", "already registered", errOut)
	}
}

// TestDoAccountAdd_InvalidConfigDir verifies that doAccountAdd returns exit
// code 1 when config_dir does not exist.
func TestDoAccountAdd_InvalidConfigDir(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("creating .gc dir: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)

	nonExistentDir := filepath.Join(cityDir, "no-such-dir")

	var stdout, stderr bytes.Buffer
	exitCode := doAccountAdd("work2", "w2@example.com", "test", nonExistentDir, &stdout, &stderr)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "does not exist") {
		t.Errorf("stderr should contain %q, got: %s", "does not exist", errOut)
	}
}

// TestDoAccountDefault_NonExistentHandle verifies that doAccountDefault returns
// exit code 1 when the specified handle is not in the registry.
func TestDoAccountDefault_NonExistentHandle(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("creating .gc dir: %v", err)
	}

	// Pre-seed a registry with "work1" only.
	regPath := filepath.Join(gcDir, "accounts.json")
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: cityDir},
		},
	}
	if err := account.Save(regPath, reg); err != nil {
		t.Fatalf("pre-seeding registry: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	exitCode := doAccountDefault("nonexistent", &stdout, &stderr)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "not registered") {
		t.Errorf("stderr should contain %q, got: %s", "not registered", errOut)
	}
}

// TestDoAccountDefault_AlreadyDefault verifies that doAccountDefault succeeds
// (exit code 0) when setting a handle that is already the default. The command
// should still write the registry and report success.
func TestDoAccountDefault_AlreadyDefault(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("creating .gc dir: %v", err)
	}

	// Pre-seed a registry with "work1" as both an account and the default.
	regPath := filepath.Join(gcDir, "accounts.json")
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: cityDir},
		},
	}
	if err := account.Save(regPath, reg); err != nil {
		t.Fatalf("pre-seeding registry: %v", err)
	}

	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	exitCode := doAccountDefault("work1", &stdout, &stderr)

	if exitCode != 0 {
		t.Errorf("expected exit code 0 (idempotent set), got %d; stderr: %s", exitCode, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "default account set to work1") {
		t.Errorf("stdout should contain %q, got: %s", "default account set to work1", out)
	}
}
