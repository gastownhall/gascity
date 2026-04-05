//go:build acceptance_a

// Session command acceptance tests.
//
// These exercise gc session subcommands as a black box. Session
// management is fundamental to the agent lifecycle. Most mutating
// operations need a running controller, so Tier A tests focus on
// list, prune, and error paths for each subcommand.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSessionErrors(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("NoSubcommand", func(t *testing.T) {
		out, err := c.GC("session")
		if err == nil {
			t.Fatal("expected error for bare 'gc session', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})

	t.Run("UnknownSubcommand", func(t *testing.T) {
		out, err := c.GC("session", "explode")
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got success")
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("expected 'unknown subcommand' message, got:\n%s", out)
		}
	})

	t.Run("New_NonexistentTemplate", func(t *testing.T) {
		_, err := c.GC("session", "new", "nonexistent-template-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent template, got success")
		}
	})

	t.Run("New_MissingTemplate", func(t *testing.T) {
		// cobra.ExactArgs(1) or custom validation handles this.
		_, err := c.GC("session", "new")
		if err == nil {
			t.Fatal("expected error for missing template, got success")
		}
	})

	t.Run("Close_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "close", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Kill_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "kill", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Wake_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "wake", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Peek_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "peek", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Rename_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "rename", "nonexistent-session", "new-title")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})
}

func TestSessionEmptyCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("List_Empty", func(t *testing.T) {
		out, err := c.GC("session", "list")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No sessions found") {
			t.Errorf("expected 'No sessions found' on fresh city, got:\n%s", out)
		}
	})

	t.Run("List_JSON", func(t *testing.T) {
		out, err := c.GC("session", "list", "--json")
		if err != nil {
			t.Fatalf("gc session list --json: %v\n%s", err, out)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed != "[]" && trimmed != "null" && !strings.HasPrefix(trimmed, "[") {
			t.Errorf("expected JSON array on fresh city, got:\n%s", out)
		}
	})

	t.Run("Prune_Empty", func(t *testing.T) {
		out, err := c.GC("session", "prune")
		if err != nil {
			t.Fatalf("gc session prune: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No sessions to prune") {
			t.Errorf("expected 'No sessions to prune' on fresh city, got:\n%s", out)
		}
	})
}
