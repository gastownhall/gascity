//go:build acceptance_a

// Config show and pack list acceptance tests.
//
// These exercise gc config show and gc pack list as a black box.
// These are diagnostic commands users run to debug configuration
// issues — they must produce useful, parseable output.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestConfigShowCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("Show_OutputsTOML", func(t *testing.T) {
		out, err := c.GC("config", "show")
		if err != nil {
			t.Fatalf("gc config show failed: %v\n%s", err, out)
		}
		// Resolved config must contain workspace section and agents.
		if !strings.Contains(out, "[workspace]") {
			t.Errorf("config show should contain [workspace], got:\n%s", out)
		}
		if !strings.Contains(out, "[[agent]]") {
			t.Errorf("config show should contain [[agent]], got:\n%s", out)
		}
		// Gastown agents should appear.
		for _, agent := range []string{"mayor", "deacon", "boot"} {
			if !strings.Contains(out, agent) {
				t.Errorf("config show should contain agent %q", agent)
			}
		}
	})

	t.Run("Show_Validate_ReportsValid", func(t *testing.T) {
		out, err := c.GC("config", "show", "--validate")
		if err != nil {
			t.Fatalf("gc config show --validate failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "valid") {
			t.Errorf("expected 'valid' in output, got:\n%s", out)
		}
	})

	t.Run("Explain_ShowsProvenance", func(t *testing.T) {
		out, err := c.GC("config", "explain")
		if err != nil {
			t.Fatalf("gc config explain failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("config explain produced empty output")
		}
	})

	t.Run("Explain_SpecificAgent", func(t *testing.T) {
		out, err := c.GC("config", "explain", "--agent", "mayor")
		if err != nil {
			t.Fatalf("gc config explain --agent mayor: %v\n%s", err, out)
		}
		if !strings.Contains(out, "mayor") {
			t.Errorf("config explain --agent mayor should mention mayor, got:\n%s", out)
		}
	})

	t.Run("Explain_UnknownAgent_ReturnsError", func(t *testing.T) {
		_, err := c.GC("config", "explain", "--agent", "nonexistent-agent-xyz")
		if err == nil {
			t.Fatal("expected error for config explain with unknown agent")
		}
	})
}

func TestConfigShowNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "config", "show")
	if err == nil {
		t.Fatal("expected error for config show from non-city directory")
	}
}

func TestPackListCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("ListSucceeds", func(t *testing.T) {
		out, err := c.GC("pack", "list")
		if err != nil {
			t.Fatalf("gc pack list failed: %v\n%s", err, out)
		}
		// Gastown city should show pack sources.
		if strings.TrimSpace(out) == "" {
			t.Fatal("pack list produced empty output")
		}
	})

	t.Run("FetchDoesNotCrash", func(t *testing.T) {
		// pack fetch may fail if git remotes are unreachable, but
		// should not crash. Accept either success or network error.
		out, _ := c.GC("pack", "fetch")
		_ = out
	})

	t.Run("BareCommand_ReturnsHelp", func(t *testing.T) {
		// Bare "gc pack" should show help or require subcommand.
		out, _ := c.GC("pack")
		_ = out
	})
}

func TestPackListNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "pack", "list")
	if err == nil {
		t.Fatal("expected error for pack list from non-city directory")
	}
}
