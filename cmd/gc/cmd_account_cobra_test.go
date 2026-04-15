package main

import (
	"bytes"
	"testing"
)

// TestNewAccountCmd_SubcommandRegistration verifies that the "gc account"
// parent command has all 5 expected subcommands: add, remove, list, default,
// and status.
func TestNewAccountCmd_SubcommandRegistration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAccountCmd(&stdout, &stderr)

	expected := map[string]bool{
		"add":     false,
		"remove":  false,
		"list":    false,
		"default": false,
		"status":  false,
	}

	for _, sub := range cmd.Commands() {
		name := sub.Name()
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("expected subcommand %q not found on account command", name)
		}
	}
}

// TestNewAccountStatusCmd_Flags verifies that the "gc account status" command
// has the expected Use and Short fields set (structural wiring check).
func TestNewAccountStatusCmd_Flags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAccountStatusCmd(&stdout, &stderr)

	if cmd.Use != "status" {
		t.Errorf("expected Use=%q, got %q", "status", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("expected non-empty Short description for account status command")
	}
	if cmd.Long == "" {
		t.Error("expected non-empty Long description for account status command")
	}
}

// TestNewAccountRemoveCmd_Flags verifies that the "gc account remove" command
// has ExactArgs(1) validation and expected Use field.
func TestNewAccountRemoveCmd_Flags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newAccountRemoveCmd(&stdout, &stderr)

	if cmd.Use != "remove <handle>" {
		t.Errorf("expected Use=%q, got %q", "remove <handle>", cmd.Use)
	}
	if cmd.Short == "" {
		t.Error("expected non-empty Short description for account remove command")
	}

	// Verify Args validation rejects no-args by executing with no args.
	// ExactArgs(1) should cause an error before RunE.
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when executing account remove with no arguments")
	}
}
