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
	"time"

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

	// The default named session (mayor) is created asynchronously by the
	// reconciler. Poll until it appears before running subtests to avoid a
	// race on slow or CPU-saturated runners.
	if !c.WaitForCondition(func() bool {
		out, err := c.GC("session", "list")
		return err == nil && strings.Contains(out, "mayor")
	}, 30*time.Second) {
		out, _ := c.GC("session", "list")
		t.Fatalf("timed out waiting for default named session (mayor) to appear:\n%s", out)
	}

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
			!strings.Contains(out, string(session.StateAwake)) &&
			!strings.Contains(out, string(session.StateAsleep)) {
			t.Errorf("expected materialized default named session state in list, got:\n%s", out)
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
			case session.StateCreating, session.StateActive, session.StateAwake, session.StateAsleep:
			default:
				t.Errorf("session %q state = %q, want materialized lifecycle state\n%s", sess.Template, sess.State, out)
			}
		}
	})

	t.Run("Config_JSON_NoDefaultControlDispatcherNamedSession", func(t *testing.T) {
		out, err := c.GC("config", "show", "--json")
		if err != nil {
			t.Fatalf("gc config show --json: %v\n%s", err, out)
		}
		var got struct {
			Config struct {
				NamedSessions []struct {
					Name     string
					Template string
					Mode     string
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("gc config show --json output is not a config envelope: %v\n%s", err, out)
		}
		// The control dispatcher serves via demand-scaling of the core-pack
		// agent template (openControlDispatcherDemand), so gc init no longer
		// injects a redundant on_demand named session for it -- that only
		// produced a confusing "backing template not found ... disabled"
		// warning on upgraded cities. Assert it is NOT auto-created.
		for _, sess := range got.Config.NamedSessions {
			if sess.Name == config.ControlDispatcherAgentName {
				t.Fatalf("gc init should not create a control-dispatcher named session (redundant with demand-scaling); found %+v\n%s", sess, out)
			}
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
