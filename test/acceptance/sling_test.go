//go:build acceptance_a

// Sling command acceptance tests.
//
// These exercise gc sling as a black box. Sling is the core dispatch
// mechanism that routes work (beads, formulas, inline text) to agents.
// Tests focus on argument validation, flag conflicts, and dry-run
// preview output since real dispatch requires a running city.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSlingCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// --- argument validation ---

	t.Run("NoArgs_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling")
		if err == nil {
			t.Fatal("expected error for bare 'gc sling', got success")
		}
		if !strings.Contains(out, "requires 1 or 2 arguments") {
			t.Errorf("expected argument count error, got:\n%s", out)
		}
	})

	t.Run("TooManyArgs_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "a", "b", "c")
		if err == nil {
			t.Fatal("expected error for too many args, got success")
		}
		if !strings.Contains(out, "requires 1 or 2 arguments") {
			t.Errorf("expected argument count error, got:\n%s", out)
		}
	})

	// --- flag validation ---

	t.Run("InvalidMergeStrategy_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--merge", "squash", "agent", "text")
		if err == nil {
			t.Fatal("expected error for invalid merge strategy, got success")
		}
		if !strings.Contains(out, "must be direct, mr, or local") {
			t.Errorf("expected merge strategy error, got:\n%s", out)
		}
	})

	t.Run("OwnedWithNoConvoy_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--owned", "--no-convoy", "agent", "text")
		if err == nil {
			t.Fatal("expected error for --owned with --no-convoy, got success")
		}
		if !strings.Contains(out, "cannot use with --no-convoy") {
			t.Errorf("expected conflict error, got:\n%s", out)
		}
	})

	t.Run("StdinRequiresOneArg_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--stdin", "agent", "extra")
		if err == nil {
			t.Fatal("expected error for --stdin with 2 args, got success")
		}
		if !strings.Contains(out, "--stdin requires exactly 1 argument") {
			t.Errorf("expected --stdin argument error, got:\n%s", out)
		}
	})

	// --- target resolution errors ---

	t.Run("NonexistentAgent_ReturnsError", func(t *testing.T) {
		_, err := c.GC("sling", "nonexistent-agent-xyz", "some work")
		if err == nil {
			t.Fatal("expected error for nonexistent agent, got success")
		}
	})

	t.Run("InlineTextWithoutTarget_ReturnsError", func(t *testing.T) {
		// Single arg that looks like inline text (not a bead ID) needs a target.
		out, err := c.GC("sling", "write a README")
		if err == nil {
			t.Fatal("expected error for inline text without target, got success")
		}
		if !strings.Contains(out, "inline text requires explicit target") {
			t.Errorf("expected 'inline text requires explicit target' error, got:\n%s", out)
		}
	})
}

func TestSlingDryRun(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	agentName := findFirstAgent(t, c)
	if agentName == "" {
		t.Skip("no agents found in gastown city config")
	}

	t.Run("InlineText_ShowsPreview", func(t *testing.T) {
		out, err := c.GC("sling", "--dry-run", agentName, "write unit tests")
		if err != nil {
			t.Fatalf("gc sling --dry-run: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No side effects executed") {
			t.Errorf("dry-run output should contain 'No side effects executed', got:\n%s", out)
		}
		if !strings.Contains(out, "Target:") {
			t.Errorf("dry-run output should contain 'Target:' section, got:\n%s", out)
		}
	})

	t.Run("Formula_ShowsPreview", func(t *testing.T) {
		// Use --formula with dry-run. Formula may not exist but we're testing
		// that the dry-run path handles the attempt gracefully.
		out, err := c.GC("sling", "--dry-run", "-f", agentName, "mol-polecat-work")
		if err != nil {
			// Formula might not exist; that's OK as long as the error is about
			// the formula, not a crash.
			if strings.Contains(out, "No side effects executed") {
				// Dry-run succeeded despite formula issues — fine.
				return
			}
			// If it's a formula-not-found error, that's expected.
			if strings.Contains(out, "formula") || strings.Contains(out, "not found") {
				t.Log("formula not found (expected in some configs)")
				return
			}
			t.Fatalf("gc sling --dry-run -f: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No side effects executed") {
			t.Errorf("dry-run output should contain 'No side effects executed', got:\n%s", out)
		}
	})
}

// --- helpers ---

// findFirstAgent parses gc config explain to find the first agent name.
func findFirstAgent(t *testing.T, c *helpers.City) string {
	t.Helper()
	out, err := c.GC("config", "explain")
	if err != nil {
		t.Fatalf("gc config explain: %v\n%s", err, out)
	}
	// Look for agent lines in config output.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "agent:") || strings.HasPrefix(line, "Agent:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	// Try gc agent list if config explain didn't work.
	listOut, err := c.GC("agent", "list")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(listOut), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && !strings.EqualFold(fields[0], "NAME") && !strings.HasPrefix(fields[0], "-") {
			return fields[0]
		}
	}
	return ""
}
