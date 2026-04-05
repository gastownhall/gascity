//go:build acceptance_a

// Formula and events acceptance tests.
//
// These exercise gc formula (list, show) and gc events / gc event emit
// as a black box. Formula tests use a gastown city which has formulas
// from its packs. Event tests verify emit+query round-trip against the
// file-backed event log.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// --- gc formula ---

func TestFormulaCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("List_GastownCity_ShowsFormulas", func(t *testing.T) {
		out, err := c.GC("formula", "list")
		if err != nil {
			t.Fatalf("gc formula list failed: %v\n%s", err, out)
		}

		// Gastown ships many formulas — verify at least one is discovered.
		if !strings.Contains(out, "mol-") {
			t.Errorf("expected gastown formulas (mol-*) in output, got:\n%s", out)
		}
	})

	t.Run("Show_GastownFormula_DisplaysSteps", func(t *testing.T) {
		// List formulas first to get a real name.
		listOut, err := c.GC("formula", "list")
		if err != nil {
			t.Fatalf("gc formula list failed: %v\n%s", err, listOut)
		}

		// Pick the first formula.
		lines := strings.Split(strings.TrimSpace(listOut), "\n")
		if len(lines) == 0 || lines[0] == "" || strings.Contains(lines[0], "No formula") {
			t.Skip("no formulas available to show")
		}
		formulaName := strings.TrimSpace(lines[0])

		out, err := c.GC("formula", "show", formulaName)
		if err != nil {
			t.Fatalf("gc formula show %s failed: %v\n%s", formulaName, err, out)
		}

		if !strings.Contains(out, "Formula:") {
			t.Errorf("expected 'Formula:' header in output, got:\n%s", out)
		}
		if !strings.Contains(out, "Steps") {
			t.Errorf("expected 'Steps' section in output, got:\n%s", out)
		}
	})

	t.Run("Show_NonexistentFormula_ReturnsError", func(t *testing.T) {
		_, err := c.GC("formula", "show", "mol-nonexistent-formula-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent formula, got success")
		}
	})

	t.Run("List_TutorialCity", func(t *testing.T) {
		tc := helpers.NewCity(t, testEnv)
		tc.Init("claude")

		out, err := tc.GC("formula", "list")
		if err != nil {
			t.Fatalf("gc formula list failed: %v\n%s", err, out)
		}
		// Tutorial city may have system formulas. The command should not crash.
		_ = out
	})
}

// --- gc events ---

func TestEventCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("Emit_ThenList_ShowsEvent", func(t *testing.T) {
		// Emit a custom event.
		out, err := c.GC("event", "emit", "test.acceptance",
			"--subject", "test-bead-123",
			"--message", "acceptance test event",
			"--actor", "quinn")
		if err != nil {
			t.Fatalf("gc event emit failed: %v\n%s", err, out)
		}

		// Query the event log.
		out, err = c.GC("events")
		if err != nil {
			t.Fatalf("gc events failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "test.acceptance") {
			t.Errorf("event log should contain emitted event type 'test.acceptance', got:\n%s", out)
		}
	})

	t.Run("Emit_AlwaysExitsZero", func(t *testing.T) {
		out, err := c.GC("event", "emit", "test.bestEffort")
		if err != nil {
			t.Fatalf("gc event emit should always exit 0: %v\n%s", err, out)
		}
	})

	t.Run("TypeFilter_FiltersResults", func(t *testing.T) {
		// Emit two different event types.
		c.GC("event", "emit", "test.alpha", "--message", "alpha event")
		c.GC("event", "emit", "test.beta", "--message", "beta event")

		// Filter to just alpha.
		out, err := c.GC("events", "--type", "test.alpha")
		if err != nil {
			t.Fatalf("gc events --type filter failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "test.alpha") {
			t.Errorf("filtered output should contain test.alpha, got:\n%s", out)
		}
		if strings.Contains(out, "test.beta") {
			t.Errorf("filtered output should NOT contain test.beta, got:\n%s", out)
		}
	})

	t.Run("Seq_PrintsNumber", func(t *testing.T) {
		out, err := c.GC("events", "--seq")
		if err != nil {
			t.Fatalf("gc events --seq failed: %v\n%s", err, out)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			t.Error("gc events --seq output is empty")
		}
	})

	t.Run("JSONFormat_OutputsParseable", func(t *testing.T) {
		// Emit an event so there's something to show.
		c.GC("event", "emit", "test.json", "--message", "json test")

		out, err := c.GC("events", "--json")
		if err != nil {
			t.Fatalf("gc events --json failed: %v\n%s", err, out)
		}
		// JSON output should start with [ (array) or contain {.
		trimmed := strings.TrimSpace(out)
		if trimmed != "" && !strings.Contains(trimmed, "{") {
			t.Errorf("expected JSON-like output, got:\n%s", out)
		}
	})

	t.Run("NoSubcommand_ReturnsError", func(t *testing.T) {
		out, err := c.GC("event")
		if err == nil {
			t.Fatal("expected error for bare 'gc event', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})
}
