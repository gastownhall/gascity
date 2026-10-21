package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveSessionTemplate_MissingDeclaredPromptIsNotSilent drives the
// session boot path (resolveTemplate, cmd/gc/template_resolve.go — the
// renderPrompt call at Step 9) for an agent whose declared prompt_template
// file does not exist. On stock the render returns "" without a word,
// resolveTemplate reads "" as "this agent has no prompt"
// (includePrimeInstruction := !hasHooks && prompt == ""), ships the bare
// beacon plus the `gc prime` instruction, and the seat ends up on
// defaultPrimePrompt. The contract pinned here: the boot path reports the
// missing path and the agent on agentBuildParams.stderr, returns no error,
// and ships exactly the no-template shape (the beacon plus the gc prime
// instruction) — the fallback is unchanged, only the silence goes.
func TestResolveSessionTemplate_MissingDeclaredPromptIsNotSilent(t *testing.T) {
	newParams := func(cityPath string, stderr io.Writer) *agentBuildParams {
		return &agentBuildParams{
			fs:              fsys.OSFS{},
			cityName:        "bright-lights",
			cityPath:        cityPath,
			workspace:       &config.Workspace{Name: "bright-lights", Provider: "opencode"},
			providers:       config.BuiltinProviders(),
			lookPath:        func(string) (string, error) { return "/usr/bin/opencode", nil },
			beaconTime:      testBeaconTime,
			sessionTemplate: "",
			beadNames:       make(map[string]string),
			stderr:          stderr,
		}
	}

	t.Run("declared but missing", func(t *testing.T) {
		cityPath := t.TempDir()
		const templatePath = "prompts/crew.template.md"
		if _, err := os.Stat(filepath.Join(cityPath, templatePath)); !os.IsNotExist(err) {
			t.Fatalf("fixture precondition: %s must not exist under %s (stat err=%v)", templatePath, cityPath, err)
		}
		var stderr strings.Builder
		agent := &config.Agent{
			Name:           "navani",
			PromptTemplate: templatePath,
			Provider:       "opencode",
		}

		tp, err := resolveTemplate(newParams(cityPath, &stderr), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v (the fallback stays; the diagnostic belongs on stderr)", err)
		}
		diagnostics := stderr.String()
		if strings.TrimSpace(diagnostics) == "" {
			t.Fatalf("agent %q declares prompt_template %q, the file is missing, and the boot path said nothing; the session will ship prompt=%q and the seat will boot on the built-in default prompt with no signal", agent.QualifiedName(), templatePath, tp.Prompt)
		}
		if !strings.Contains(diagnostics, templatePath) {
			t.Fatalf("boot-path stderr = %q, want a line naming the missing template path %q", diagnostics, templatePath)
		}
		if !strings.Contains(diagnostics, agent.QualifiedName()) {
			t.Errorf("boot-path stderr = %q, want a line naming the agent %q", diagnostics, agent.QualifiedName())
		}

		// The fallback is unchanged: the session ships exactly what an agent
		// without a prompt_template ships — the beacon plus the gc prime
		// instruction — and no template text.
		control := &config.Agent{Name: "navani", Provider: "opencode"}
		var controlStderr strings.Builder
		want, err := resolveTemplate(newParams(cityPath, &controlStderr), control, control.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("control resolveTemplate: %v", err)
		}
		if tp.Prompt != want.Prompt {
			t.Fatalf("prompt with a missing template = %q, want the no-template shape %q", tp.Prompt, want.Prompt)
		}
		if !strings.Contains(tp.Prompt, "Run `gc prime`") {
			t.Fatalf("prompt = %q, want the gc prime instruction (the beacon ships alone)", tp.Prompt)
		}
	})

	t.Run("control: no prompt_template declared", func(t *testing.T) {
		cityPath := t.TempDir()
		var stderr strings.Builder
		agent := &config.Agent{
			Name:     "navani",
			Provider: "opencode",
		}

		if _, err := resolveTemplate(newParams(cityPath, &stderr), agent, agent.QualifiedName(), nil); err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("an agent that declares no prompt_template must resolve silently; stderr = %q", stderr.String())
		}
	})
}
