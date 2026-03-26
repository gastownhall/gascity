package account

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	cityPath := dir

	// .gc/ must exist for RuntimePath.
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	reg, err := Load(cityPath)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(reg.Accounts) != 0 {
		t.Errorf("expected empty accounts, got %d", len(reg.Accounts))
	}
}

func TestWithRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cityPath := dir

	// Add an account.
	err := WithRegistry(cityPath, func(reg *Registry) error {
		return Add(reg, Account{Handle: "test", ConfigDir: "/test/.claude", Provider: "claude"})
	})
	if err != nil {
		t.Fatalf("WithRegistry(add): %v", err)
	}

	// Read it back.
	reg, err := Load(cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reg.Accounts) != 1 {
		t.Fatalf("len = %d, want 1", len(reg.Accounts))
	}
	if reg.Accounts[0].Handle != "test" {
		t.Errorf("Handle = %q, want test", reg.Accounts[0].Handle)
	}
	if reg.Accounts[0].ConfigDir != "/test/.claude" {
		t.Errorf("ConfigDir = %q, want /test/.claude", reg.Accounts[0].ConfigDir)
	}

	// Set default and verify.
	err = WithRegistry(cityPath, func(reg *Registry) error {
		return SetDefault(reg, "test")
	})
	if err != nil {
		t.Fatalf("WithRegistry(default): %v", err)
	}

	reg, err = Load(cityPath)
	if err != nil {
		t.Fatalf("Load after default: %v", err)
	}
	if reg.Default != "test" {
		t.Errorf("Default = %q, want test", reg.Default)
	}
}
