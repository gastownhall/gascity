//go:build acceptance_a

// Session command acceptance tests.
//
// These exercise gc session subcommands as a black box. Session
// management is fundamental to the agent lifecycle. Most mutating
// operations need a running controller, so Tier A tests focus on
// list, prune, and error paths for each subcommand.
package acceptance_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
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

func TestSessionDefaultNamedSession(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("List_DefaultNamedSession", func(t *testing.T) {
		out, err := c.GC("session", "list")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		if strings.Contains(out, "No sessions found") {
			t.Errorf("expected default named session on fresh city, got:\n%s", out)
		}
		if !strings.Contains(out, "mayor") {
			t.Errorf("expected default named session in list, got:\n%s", out)
		}
		if !strings.Contains(out, string(session.StateCreating)) &&
			!strings.Contains(out, string(session.StateActive)) &&
			!strings.Contains(out, string(session.StateAwake)) {
			t.Errorf("expected creating or running state in default named session list, got:\n%s", out)
		}
	})

	t.Run("List_JSON_DefaultNamedSession", func(t *testing.T) {
		out, err := c.GC("session", "list", "--json")
		if err != nil {
			t.Fatalf("gc session list --json: %v\n%s", err, out)
		}
		var got struct {
			Sessions []struct {
				Template string        `json:"template"`
				State    session.State `json:"state"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("gc session list --json output is not a session list envelope: %v\n%s", err, out)
		}
		if len(got.Sessions) != 1 {
			t.Fatalf("session count = %d, want 1 default named session\n%s", len(got.Sessions), out)
		}
		if got.Sessions[0].Template != "mayor" {
			t.Errorf("template = %q, want mayor\n%s", got.Sessions[0].Template, out)
		}
		switch got.Sessions[0].State {
		case session.StateCreating, session.StateActive, session.StateAwake:
		default:
			t.Errorf("state = %q, want creating or running\n%s", got.Sessions[0].State, out)
		}
	})

	t.Run("Prune_NoClosedSessions", func(t *testing.T) {
		out, err := c.GC("session", "prune")
		if err != nil {
			t.Fatalf("gc session prune: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No sessions to prune") {
			t.Errorf("expected 'No sessions to prune' with only default named session, got:\n%s", out)
		}
	})
}
