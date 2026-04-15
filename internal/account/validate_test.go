package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHandle_Valid(t *testing.T) {
	for _, h := range []string{"work1", "my-account", "test_2", "ABC", "a", "Z9-x_y"} {
		if err := ValidateHandle(h); err != nil {
			t.Errorf("ValidateHandle(%q) = %v, want nil", h, err)
		}
	}
}

func TestValidateHandle_Empty(t *testing.T) {
	if err := ValidateHandle(""); err == nil {
		t.Error("ValidateHandle(\"\") = nil, want error")
	}
}

func TestValidateHandle_Spaces(t *testing.T) {
	err := ValidateHandle("my account")
	if err == nil {
		t.Fatal("expected error for handle with space")
	}
	// PRD says error must name the disallowed characters.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "disallowed") {
		t.Errorf("error should mention disallowed characters, got: %v", err)
	}
}

func TestValidateHandle_Slashes(t *testing.T) {
	err := ValidateHandle("work/1")
	if err == nil {
		t.Fatal("expected error for handle with slash")
	}
}

func TestValidateHandle_SpecialChars(t *testing.T) {
	for _, h := range []string{"work@1", "work!1", "a b", "foo/bar", "x.y", "hello\tworld"} {
		if err := ValidateHandle(h); err == nil {
			t.Errorf("ValidateHandle(%q) = nil, want error", h)
		}
	}
}

func TestValidateHandle_Unicode(t *testing.T) {
	// POSIX-safe only — unicode should be rejected.
	if err := ValidateHandle("caf\u00e9"); err == nil {
		t.Error("ValidateHandle(\"caf\\u00e9\") = nil, want error for unicode chars")
	}
}

func TestValidateNewAccount_Duplicate(t *testing.T) {
	dir := t.TempDir()
	reg := Registry{
		Accounts: []Account{{Handle: "work1", ConfigDir: dir}},
	}
	err := ValidateNewAccount(reg, Account{Handle: "work1", ConfigDir: dir})
	if err == nil {
		t.Fatal("expected error for duplicate handle")
	}
	// PRD: exact message must include "handle work1 is already registered".
	if !strings.Contains(err.Error(), "handle work1 is already registered") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateNewAccount_MissingDir(t *testing.T) {
	reg := Registry{}
	err := ValidateNewAccount(reg, Account{
		Handle:    "work1",
		ConfigDir: "/nonexistent/path/that/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected error for missing config_dir")
	}
}

func TestValidateNewAccount_UnreadableDir(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "noperm")
	if err := os.Mkdir(unreadable, 0o000); err != nil {
		t.Fatalf("creating unreadable dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	reg := Registry{}
	err := ValidateNewAccount(reg, Account{
		Handle:    "work1",
		ConfigDir: unreadable,
	})
	if err == nil {
		t.Fatal("expected error for unreadable dir")
	}
}

func TestValidateNewAccount_DirIsFile(t *testing.T) {
	// Edge case: config_dir that is a file, not a directory.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("data"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	reg := Registry{}
	err := ValidateNewAccount(reg, Account{
		Handle:    "work1",
		ConfigDir: filePath,
	})
	if err == nil {
		t.Fatal("expected error when config_dir is a file, not a directory")
	}
}

func TestValidateNewAccount_Success(t *testing.T) {
	dir := t.TempDir()
	reg := Registry{}
	err := ValidateNewAccount(reg, Account{
		Handle:    "work1",
		ConfigDir: dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
