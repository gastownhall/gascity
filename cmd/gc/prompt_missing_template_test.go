package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestRenderPrompt_DeclaredButMissingTemplateWarnsToStderr pins the renderer's
// half of the missing-prompt_template defect. An agent that DECLARES a
// prompt_template whose file does not exist must be distinguishable from an
// agent that declares none. renderPromptWithMeta already warns on a malformed
// template (TestRenderPromptParseErrorFallback) and on a missing inject
// fragment (TestRenderPromptInjectMissing); the missing-file branch is the
// one read path that returns an empty PromptRenderResult in silence. The
// session boot path then treats "" as "this agent has no prompt", ships a
// bare beacon, and the seat's non-strict `gc prime` falls through to
// defaultPrimePrompt ("# Gas City Agent") with no signal anywhere.
//
// The empty return value is kept: the fallback may stay, the silence is the
// defect.
func TestRenderPrompt_DeclaredButMissingTemplateWarnsToStderr(t *testing.T) {
	cityPath := t.TempDir()
	// The agent name is deliberately not a substring of the template path, so
	// the agent-name assertion below cannot pass by accident.
	const templatePath = "prompts/crew.template.md"
	ctx := PromptContext{CityRoot: cityPath, AgentName: "qcore/navani", TemplateName: "crew"}

	t.Run("declared but missing", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(cityPath, templatePath)); !os.IsNotExist(err) {
			t.Fatalf("fixture precondition: %s must not exist under %s (stat err=%v)", templatePath, cityPath, err)
		}
		var stderr strings.Builder
		res := renderPromptWithMeta(fsys.OSFS{}, cityPath, "test-city", templatePath, ctx, "", &stderr, nil, nil, nil)
		if res.Text != "" {
			t.Fatalf("renderPromptWithMeta(missing).Text = %q, want empty (the fallback stays; only the silence is the defect)", res.Text)
		}
		if stderr.Len() == 0 {
			t.Fatalf("declared prompt_template %q is missing and the renderer said nothing: stderr=%q; the seat will boot on the built-in default prompt with no signal", templatePath, stderr.String())
		}
		if !strings.Contains(stderr.String(), templatePath) {
			t.Errorf("stderr = %q, want a line naming the missing template path %q", stderr.String(), templatePath)
		}
		if !strings.Contains(stderr.String(), ctx.AgentName) {
			t.Errorf("stderr = %q, want a line naming the agent %q (PromptContext.AgentName is available to the renderer)", stderr.String(), ctx.AgentName)
		}
	})

	t.Run("control: no prompt_template declared", func(t *testing.T) {
		var stderr strings.Builder
		res := renderPromptWithMeta(fsys.OSFS{}, cityPath, "test-city", "", ctx, "", &stderr, nil, nil, nil)
		if res.Text != "" {
			t.Fatalf("renderPromptWithMeta(empty path).Text = %q, want empty", res.Text)
		}
		if stderr.Len() != 0 {
			t.Fatalf("an agent that declares no prompt_template must render silently; stderr = %q", stderr.String())
		}
	})
}
