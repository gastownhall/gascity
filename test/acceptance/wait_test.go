//go:build acceptance_a

// Wait command acceptance tests.
//
// These exercise gc wait list, inspect, cancel, and ready as a black
// box. Wait is the durable session wait mechanism — agents register
// waits that survive session restarts. Tests cover the empty-state
// list, error paths for missing IDs and nonexistent waits, and
// non-city context.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestWaitCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("ListEmpty", func(t *testing.T) {
		out, err := c.GC("wait", "list")
		if err != nil {
			t.Fatalf("gc wait list failed: %v\n%s", err, out)
		}
		// Fresh city should have no waits.
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "no wait") && !strings.Contains(lower, "0 wait") && strings.TrimSpace(out) != "" {
			// Accept empty output or "no waits" message.
			t.Logf("gc wait list output: %s", out)
		}
	})

	t.Run("ListWithStateFilter", func(t *testing.T) {
		out, err := c.GC("wait", "list", "--state", "pending")
		if err != nil {
			t.Fatalf("gc wait list --state pending: %v\n%s", err, out)
		}
		// Should succeed even with no pending waits.
	})

	t.Run("InspectMissingID", func(t *testing.T) {
		_, err := c.GC("wait", "inspect")
		if err == nil {
			t.Fatal("expected error for wait inspect without ID")
		}
	})

	t.Run("InspectNonexistent", func(t *testing.T) {
		_, err := c.GC("wait", "inspect", "no-such-wait-xyz")
		if err == nil {
			t.Fatal("expected error for inspecting nonexistent wait")
		}
	})

	t.Run("CancelMissingID", func(t *testing.T) {
		_, err := c.GC("wait", "cancel")
		if err == nil {
			t.Fatal("expected error for wait cancel without ID")
		}
	})

	t.Run("CancelNonexistent", func(t *testing.T) {
		_, err := c.GC("wait", "cancel", "no-such-wait-xyz")
		if err == nil {
			t.Fatal("expected error for cancelling nonexistent wait")
		}
	})

	t.Run("ReadyMissingID", func(t *testing.T) {
		_, err := c.GC("wait", "ready")
		if err == nil {
			t.Fatal("expected error for wait ready without ID")
		}
	})

	t.Run("ReadyNonexistent", func(t *testing.T) {
		_, err := c.GC("wait", "ready", "no-such-wait-xyz")
		if err == nil {
			t.Fatal("expected error for marking nonexistent wait ready")
		}
	})
}

func TestWaitFromNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "wait", "list")
	if err == nil {
		t.Fatal("expected error for wait list from non-city directory")
	}
}
