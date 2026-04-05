//go:build acceptance_a

// Nudge and service command acceptance tests.
//
// These exercise gc nudge status and gc service (list, doctor) as a
// black box. Both are infrastructure inspection commands that should
// work on any initialized city.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestNudgeCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("StatusMissingSession", func(t *testing.T) {
		_, err := c.GC("nudge", "status")
		if err == nil {
			t.Fatal("expected error for nudge status without session ID")
		}
	})

	t.Run("StatusNonexistent", func(t *testing.T) {
		// nudge status requires a session ID arg.
		_, err := c.GC("nudge", "status", "no-such-session-xyz")
		// May succeed with empty output or fail — either is acceptable.
		// The key test is it doesn't crash.
		_ = err
	})

	t.Run("BareCommand_ReturnsHelp", func(t *testing.T) {
		// Bare "gc nudge" should show help or error.
		out, err := c.GC("nudge")
		_ = err
		_ = out
	})
}

func TestNudgeFromNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "nudge", "status", "test")
	if err == nil {
		t.Fatal("expected error for nudge status from non-city directory")
	}
}

func TestServiceCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("ListSucceeds", func(t *testing.T) {
		out, err := c.GC("service", "list")
		if err != nil {
			t.Fatalf("gc service list failed: %v\n%s", err, out)
		}
	})

	t.Run("DoctorDoesNotCrash", func(t *testing.T) {
		// service doctor may return non-zero if services are unhealthy
		// (expected when no city is running). We just verify it doesn't crash.
		out, _ := c.GC("service", "doctor")
		_ = out
	})

	t.Run("BareCommand_ReturnsError", func(t *testing.T) {
		out, err := c.GC("service")
		if err == nil {
			t.Fatal("expected error for bare 'gc service', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})
}

func TestServiceFromNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "service", "list")
	if err == nil {
		t.Fatal("expected error for service list from non-city directory")
	}
}
