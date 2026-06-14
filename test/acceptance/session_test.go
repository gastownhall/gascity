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

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSessionErrors(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")

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
		var mayorSeen bool
		for _, sess := range got.Sessions {
			if sess.Template == "mayor" {
				mayorSeen = true
			}
		}
		if !mayorSeen {
			t.Fatalf("default mayor named session missing\n%s", out)
		}
		for _, sess := range got.Sessions {
			switch sess.State {
			case session.StateCreating, session.StateActive, session.StateAwake:
			default:
				t.Errorf("session %q state = %q, want creating or running\n%s", sess.Template, sess.State, out)
			}
		}
	})

	t.Run("Config_JSON_DefaultOnDemandControlDispatcher", func(t *testing.T) {
		out, err := c.GC("config", "show", "--json")
		if err != nil {
			t.Fatalf("gc config show --json: %v\n%s", err, out)
		}
		var got struct {
			Config struct {
				NamedSessions []struct {
					Template string
					Mode     string
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("gc config show --json output is not a config envelope: %v\n%s", err, out)
		}
		for _, sess := range got.Config.NamedSessions {
			if sess.Template == config.ControlDispatcherAgentName {
				if sess.Mode != "on_demand" {
					t.Fatalf("default control-dispatcher mode = %q, want on_demand\n%s", sess.Mode, out)
				}
				return
			}
		}
		t.Fatalf("default control-dispatcher named session missing from config\n%s", out)
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
