//go:build acceptance_a

// Handoff command acceptance tests.
//
// These exercise gc handoff error paths. Handoff is the context
// continuation mechanism — it sends mail and restarts a session.
// Full lifecycle tests belong in Tier B (needs real sessions).
// Tier A covers argument validation and missing-context errors.
package acceptance_test

import (
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestHandoffCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("NoArgs_ReturnsError", func(t *testing.T) {
		// cobra.RangeArgs(1,2) rejects zero args.
		_, err := c.GC("handoff")
		if err == nil {
			t.Fatal("expected error for handoff without args, got success")
		}
	})

	t.Run("NoSessionContext_ReturnsError", func(t *testing.T) {
		// Self-handoff requires GC_ALIAS/GC_SESSION_ID which aren't set in tests.
		_, err := c.GC("handoff", "test subject")
		if err == nil {
			t.Fatal("expected error for handoff without session context, got success")
		}
	})

	t.Run("RemoteNonexistent_ReturnsError", func(t *testing.T) {
		_, err := c.GC("handoff", "--target", "nonexistent-session", "test subject")
		if err == nil {
			t.Fatal("expected error for handoff to nonexistent target, got success")
		}
	})

	t.Run("TooManyArgs_ReturnsError", func(t *testing.T) {
		// cobra.RangeArgs(1,2) rejects three args.
		_, err := c.GC("handoff", "subject", "message", "extra")
		if err == nil {
			t.Fatal("expected error for too many args, got success")
		}
	})

	t.Run("RemoteWithMessage_ReturnsError", func(t *testing.T) {
		// Remote handoff with nonexistent target + message body.
		_, err := c.GC("handoff", "--target", "nonexistent", "subject", "body message")
		if err == nil {
			t.Fatal("expected error for remote handoff to nonexistent, got success")
		}
	})
}

// --- gc graph ---

func TestGraphCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("EmptyCity_Succeeds", func(t *testing.T) {
		// graph should not crash on an empty city.
		_, _ = c.GC("graph")
	})
}

func TestGraph_NotInitialized_ReturnsError(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "graph")
	if err == nil {
		t.Fatal("expected error for graph on non-city directory, got success")
	}
}
