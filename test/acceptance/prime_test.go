//go:build acceptance_a

// Prime command acceptance tests.
//
// These exercise gc prime as a black box: agent name resolution,
// prompt template rendering, GC_AGENT env fallback, hook mode,
// and the default prompt fallback when no agent or city matches.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestPrimeGastownCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("NamedAgent_OutputsRenderedPrompt", func(t *testing.T) {
		out, err := c.GC("prime", "mayor")
		if err != nil {
			t.Fatalf("gc prime mayor failed: %v\n%s", err, out)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			t.Fatal("gc prime mayor produced empty output")
		}
		// A rendered template should NOT be the default fallback.
		if strings.Contains(out, "# Gas City Agent") {
			t.Error("gc prime mayor returned the default prompt instead of the mayor template")
		}
	})

	t.Run("DifferentAgent_OutputsDifferentPrompt", func(t *testing.T) {
		mayorOut, err := c.GC("prime", "mayor")
		if err != nil {
			t.Fatalf("gc prime mayor: %v\n%s", err, mayorOut)
		}
		bootOut, err := c.GC("prime", "boot")
		if err != nil {
			t.Fatalf("gc prime boot: %v\n%s", err, bootOut)
		}
		if strings.TrimSpace(bootOut) == "" {
			t.Fatal("gc prime boot produced empty output")
		}
		if mayorOut == bootOut {
			t.Error("mayor and boot should have different prompts")
		}
	})

	t.Run("UnknownAgent_ReturnsDefaultPrompt", func(t *testing.T) {
		out, err := c.GC("prime", "nonexistent-agent-xyz")
		if err != nil {
			t.Fatalf("gc prime nonexistent-agent-xyz failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "# Gas City Agent") {
			t.Errorf("expected default prompt for unknown agent, got:\n%s", out)
		}
	})

	t.Run("GC_AGENT_Fallback", func(t *testing.T) {
		testEnv.With("GC_AGENT", "mayor")
		defer testEnv.Without("GC_AGENT")

		out, err := c.GC("prime")
		if err != nil {
			t.Fatalf("gc prime (GC_AGENT=mayor) failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc prime with GC_AGENT=mayor produced empty output")
		}
		if strings.Contains(out, "# Gas City Agent") {
			t.Error("gc prime with GC_AGENT=mayor returned default prompt instead of mayor template")
		}
	})

	t.Run("NoArgsNoEnv_ReturnsDefaultPrompt", func(t *testing.T) {
		// Ensure GC_AGENT is not set.
		testEnv.Without("GC_AGENT")

		out, err := c.GC("prime")
		if err != nil {
			t.Fatalf("gc prime (no args, no GC_AGENT) failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "# Gas City Agent") {
			t.Errorf("expected default prompt when no agent specified, got:\n%s", out)
		}
	})

	t.Run("HookMode_DoesNotCrash", func(t *testing.T) {
		out, err := c.GC("prime", "--hook", "mayor")
		if err != nil {
			t.Fatalf("gc prime --hook mayor failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc prime --hook mayor produced empty output")
		}
	})
}

func TestPrimeTutorialCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("NoArgs_ReturnsPrompt", func(t *testing.T) {
		out, err := c.GC("prime")
		if err != nil {
			t.Fatalf("gc prime failed: %v\n%s", err, out)
		}
		// Tutorial city may not have agents with templates, so default prompt is fine.
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc prime produced empty output")
		}
	})
}

func TestPrimeNoCityContext(t *testing.T) {
	emptyDir := t.TempDir()
	out, err := helpers.RunGC(testEnv, emptyDir, "prime")
	if err != nil {
		t.Fatalf("gc prime from non-city dir failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "# Gas City Agent") {
		t.Errorf("expected default prompt from non-city dir, got:\n%s", out)
	}
}

func TestPrimeDefaultPromptContent(t *testing.T) {
	emptyDir := t.TempDir()
	out, err := helpers.RunGC(testEnv, emptyDir, "prime")
	if err != nil {
		t.Fatalf("gc prime failed: %v\n%s", err, out)
	}
	// The default prompt should mention the key bd commands.
	for _, expected := range []string{"bd ready", "bd show", "bd close"} {
		if !strings.Contains(out, expected) {
			t.Errorf("default prompt should mention %q, got:\n%s", expected, out)
		}
	}
}
