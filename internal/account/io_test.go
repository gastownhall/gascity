package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoad_MissingFile(t *testing.T) {
	// Loading from a nonexistent file should return an empty Registry, not an error.
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) returned error for missing file: %v", path, err)
	}
	if len(reg.Accounts) != 0 {
		t.Fatalf("Load(missing) returned %d accounts, want 0", len(reg.Accounts))
	}
	if reg.Default != "" {
		t.Fatalf("Load(missing) returned Default=%q, want \"\"", reg.Default)
	}
}

func TestLoad_ValidRoundTrip(t *testing.T) {
	// Save then Load should return an identical registry.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	want := Registry{
		Default: "work1",
		Accounts: []Account{
			{Handle: "work1", Email: "a@b.com", Description: "primary", ConfigDir: "/tmp/w1"},
			{Handle: "personal", Email: "c@d.com", Description: "secondary", ConfigDir: "/tmp/p"},
		},
	}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n  got:  %+v\n  want: %+v", got, want)
	}
}

func TestLoad_CorruptJSON(t *testing.T) {
	// Malformed JSON should produce a parse error that includes the file path.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	if err := os.WriteFile(path, []byte(`{"default": BROKEN`), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(corrupt JSON) should return an error")
	}
	// Error should mention the file path for diagnostics.
	if got := err.Error(); !containsSubstring(got, path) {
		t.Fatalf("error %q should contain file path %q", got, path)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	// An empty file (zero bytes) should return an empty registry or a clear error.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	reg, err := Load(path)
	if err != nil {
		// An error is acceptable as long as it mentions the file path.
		if got := err.Error(); !containsSubstring(got, path) {
			t.Fatalf("error %q should contain file path %q", got, path)
		}
		return
	}
	// If no error, should be an empty registry.
	if len(reg.Accounts) != 0 {
		t.Fatalf("Load(empty file) returned %d accounts, want 0", len(reg.Accounts))
	}
}

func TestSave_Atomic(t *testing.T) {
	// After Save, the file should exist with correct contents.
	// No partial writes should be observable.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	reg := Registry{
		Default:  "a1",
		Accounts: []Account{{Handle: "a1", Email: "x@y.com", Description: "test", ConfigDir: "/tmp/a1"}},
	}

	if err := Save(path, reg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// File must exist.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not found after Save: %v", err)
	}

	// Contents must be valid JSON that decodes to the same registry.
	var got Registry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, reg) {
		t.Fatalf("saved contents mismatch:\n  got:  %+v\n  want: %+v", got, reg)
	}

	// No temp files should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "accounts.json" {
			t.Errorf("leftover temp file after Save: %s", e.Name())
		}
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	// Save should create the parent directory if it does not exist.
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", ".gc", "accounts.json")

	reg := Registry{
		Default:  "w",
		Accounts: []Account{{Handle: "w", ConfigDir: "/tmp/w"}},
	}

	if err := Save(path, reg); err != nil {
		t.Fatalf("Save failed when parent dir missing: %v", err)
	}

	// Verify file was created.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not found after Save with missing parent: %v", err)
	}
}

func TestSave_PreservesAllFields(t *testing.T) {
	// All four Account fields must round-trip correctly through Save/Load.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	want := Registry{
		Accounts: []Account{
			{
				Handle:      "myhandle",
				Email:       "user@example.com",
				Description: "A detailed description with special chars: é, ñ",
				ConfigDir:   "/home/user/.config/provider",
			},
		},
	}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(got.Accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got.Accounts))
	}
	a := got.Accounts[0]
	if a.Handle != want.Accounts[0].Handle {
		t.Errorf("Handle = %q, want %q", a.Handle, want.Accounts[0].Handle)
	}
	if a.Email != want.Accounts[0].Email {
		t.Errorf("Email = %q, want %q", a.Email, want.Accounts[0].Email)
	}
	if a.Description != want.Accounts[0].Description {
		t.Errorf("Description = %q, want %q", a.Description, want.Accounts[0].Description)
	}
	if a.ConfigDir != want.Accounts[0].ConfigDir {
		t.Errorf("ConfigDir = %q, want %q", a.ConfigDir, want.Accounts[0].ConfigDir)
	}
}

func TestSave_DefaultField(t *testing.T) {
	// The "default" field in registry JSON must be persisted and loaded.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	want := Registry{
		Default: "my-default-account",
		Accounts: []Account{
			{Handle: "my-default-account", ConfigDir: "/tmp/d"},
		},
	}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if got.Default != want.Default {
		t.Fatalf("Default = %q, want %q", got.Default, want.Default)
	}
}

// containsSubstring reports whether s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
