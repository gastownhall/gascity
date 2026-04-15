package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestNewQuotaCmd_SubcommandRegistration verifies that the "gc quota" parent
// command has all 4 expected subcommands: scan, status, rotate, and clear.
func TestNewQuotaCmd_SubcommandRegistration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newQuotaCmd(&stdout, &stderr)

	expected := map[string]bool{
		"scan":   false,
		"status": false,
		"rotate": false,
		"clear":  false,
	}

	for _, sub := range cmd.Commands() {
		name := sub.Name()
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("expected subcommand %q not found on quota command", name)
		}
	}
}

// TestNewQuotaScanCmd_Flags verifies that the "gc quota scan" command has the
// expected Use and description fields set.
func TestNewQuotaScanCmd_Flags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newQuotaScanCmd(&stdout, &stderr)

	if cmd.Use != "scan" {
		t.Errorf("expected Use=%q, got %q", "scan", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("expected non-empty Short description for quota scan command")
	}
	if cmd.Long == "" {
		t.Error("expected non-empty Long description for quota scan command")
	}
}

// TestNewQuotaRotateCmd_Flags verifies that the "gc quota rotate" command has
// the expected Use and description fields set.
func TestNewQuotaRotateCmd_Flags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newQuotaRotateCmd(&stdout, &stderr)

	if cmd.Use != "rotate" {
		t.Errorf("expected Use=%q, got %q", "rotate", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("expected non-empty Short description for quota rotate command")
	}
	if cmd.Long == "" {
		t.Error("expected non-empty Long description for quota rotate command")
	}
}

// TestNewQuotaClearCmd_Flags verifies that the "gc quota clear" command has
// the --all and --force flags registered.
func TestNewQuotaClearCmd_Flags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newQuotaClearCmd(&stdout, &stderr)

	if cmd.Use != "clear [handle]" {
		t.Errorf("expected Use=%q, got %q", "clear [handle]", cmd.Use)
	}

	// Verify --all flag exists.
	allFlag := cmd.Flags().Lookup("all")
	if allFlag == nil {
		t.Fatal("expected --all flag to be registered on quota clear command")
	}
	if allFlag.DefValue != "false" {
		t.Errorf("expected --all default to be %q, got %q", "false", allFlag.DefValue)
	}

	// Verify --force flag exists.
	forceFlag := cmd.Flags().Lookup("force")
	if forceFlag == nil {
		t.Fatal("expected --force flag to be registered on quota clear command")
	}
	if forceFlag.DefValue != "false" {
		t.Errorf("expected --force default to be %q, got %q", "false", forceFlag.DefValue)
	}
}

// TestLoadRateLimitPatterns verifies that loadRateLimitPatterns loads patterns
// from a valid city config with providers configured. When the city config
// cannot be loaded (no city.toml), it falls back to default patterns.
func TestLoadRateLimitPatterns(t *testing.T) {
	// Test fallback when city config does not exist.
	t.Run("fallback_no_config", func(t *testing.T) {
		tmpDir := t.TempDir()
		var stderr bytes.Buffer
		patterns := loadRateLimitPatterns(tmpDir, &stderr)

		// Should fall back to default patterns.
		defaultPatterns, ok := patterns["default"]
		if !ok {
			t.Fatal("expected 'default' key in fallback patterns")
		}
		if len(defaultPatterns) == 0 {
			t.Error("expected non-empty default patterns")
		}
	})

	// Test with a valid city config that has providers with rate-limit patterns.
	t.Run("with_city_config", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create a minimal city.toml with a custom provider.
		cityToml := `
name = "test-city"

[providers.custom1]
command = "echo"
rate_limit_patterns = ["custom rate limit", "custom 429"]
`
		if err := os.WriteFile(filepath.Join(tmpDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
			t.Fatal(err)
		}

		var stderr bytes.Buffer
		patterns := loadRateLimitPatterns(tmpDir, &stderr)

		// Should have the custom provider patterns.
		custom, ok := patterns["custom1"]
		if !ok {
			t.Fatal("expected 'custom1' key in patterns from city config")
		}
		if len(custom) != 2 {
			t.Errorf("expected 2 custom patterns, got %d", len(custom))
		}

		// Should also have builtin providers (claude).
		if _, ok := patterns["claude"]; !ok {
			t.Error("expected builtin 'claude' provider patterns to be included")
		}
	})
}

// TestDefaultRateLimitPatterns verifies that defaultRateLimitPatterns returns
// a non-empty set of sensible patterns.
func TestDefaultRateLimitPatterns(t *testing.T) {
	patterns := defaultRateLimitPatterns()

	if len(patterns) == 0 {
		t.Fatal("expected non-empty default rate-limit patterns")
	}

	// Verify the patterns include common rate-limit indicators.
	expected := map[string]bool{
		"rate limit":     false,
		"429":            false,
		"quota exceeded": false,
	}

	for _, p := range patterns {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for pattern, found := range expected {
		if !found {
			t.Errorf("expected default pattern %q not found", pattern)
		}
	}
}
