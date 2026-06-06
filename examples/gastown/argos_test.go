// Package gastown_test also validates the argos watchdog pack, which the
// gastown example city composes alongside its domain roster.
//
// Argos is the scaffold stage of the rate-limit-recovery watchdog: a
// city-scoped, always-resident named session modeled on the boot agent
// that wakes each patrol tick, logs a one-line session summary, drain-acks,
// and exits. These tests pin the scaffold's structure (so the lifecycle is
// what the prompt documents) and its no-op boundary (so detection and
// recovery arrive as a deliberate, reviewed change, not by accident).
package gastown_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

// TestArgosPackParses verifies the argos pack identity is well-formed.
func TestArgosPackParses(t *testing.T) {
	dir := exampleDir()
	data, err := os.ReadFile(filepath.Join(dir, "packs", "argos", "pack.toml"))
	if err != nil {
		t.Fatalf("reading argos pack.toml: %v", err)
	}
	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing argos pack.toml: %v", err)
	}
	if tc.Pack.Name != "argos" {
		t.Errorf("[pack] name = %q, want %q", tc.Pack.Name, "argos")
	}
	if tc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", tc.Pack.Schema)
	}
}

// TestArgosAgentScaffoldConfig pins the boot-modeled agent.toml fields:
// city scope, fresh wake, isolated work dir, single resident session.
func TestArgosAgentScaffoldConfig(t *testing.T) {
	agents := discoverPackAgents(t, filepath.Join("packs", "argos"))
	if len(agents) != 1 {
		t.Fatalf("argos pack has %d discovered agents, want 1", len(agents))
	}
	a := agents[0]
	if a.Name != "argos" {
		t.Errorf("agent name = %q, want %q", a.Name, "argos")
	}
	if a.Scope != "city" {
		t.Errorf("argos scope = %q, want %q", a.Scope, "city")
	}
	if got := a.EffectiveWakeMode(); got != "fresh" {
		t.Errorf("argos wake_mode = %q, want %q", got, "fresh")
	}
	if a.WorkDir != ".gc/agents/argos" {
		t.Errorf("argos work_dir = %q, want %q", a.WorkDir, ".gc/agents/argos")
	}
	if a.MaxActiveSessions == nil || *a.MaxActiveSessions != 1 {
		t.Errorf("argos max_active_sessions = %v, want 1", a.MaxActiveSessions)
	}
	if !strings.HasSuffix(a.PromptTemplate, filepath.Join("agents", "argos", "prompt.template.md")) {
		t.Errorf("argos prompt_template = %q, want suffix agents/argos/prompt.template.md", a.PromptTemplate)
	}
}

// TestArgosPromptMatchesNamedSessionLifecycle mirrors the boot lifecycle
// check: the always-resident named session and fresh wake mode must match
// what the prompt tells the agent about its own lifecycle.
func TestArgosPromptMatchesNamedSessionLifecycle(t *testing.T) {
	cfg := loadExpanded(t)
	argosSession := config.FindNamedSession(cfg, "argos")
	if argosSession == nil {
		t.Fatal("argos named_session missing; the city must keep the watchdog resident")
	}
	if got := argosSession.ModeOrDefault(); got != "always" {
		t.Fatalf("argos named_session mode = %q, want %q so the reconciler re-wakes it each patrol tick", got, "always")
	}
	argosAgent := config.FindAgent(cfg, argosSession.TemplateQualifiedName())
	if argosAgent == nil {
		t.Fatalf("argos agent template %q missing; named_session and prompt must refer to a real agent", argosSession.TemplateQualifiedName())
	}
	if got := argosAgent.EffectiveWakeMode(); got != "fresh" {
		t.Fatalf("argos agent wake_mode = %q, want %q because the prompt documents fresh, single-pass triage", got, "fresh")
	}
	if got := argosAgent.Scope; got != "city" {
		t.Fatalf("argos agent scope = %q, want %q", got, "city")
	}

	body := renderArgosPrompt(t)
	for _, want := range []string{
		`mode = "always"`,
		`wake_mode = "fresh"`,
		"single-pass triage",
		"Drain-ack and exit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos prompt missing lifecycle guidance %q", want)
		}
	}
}

// TestArgosPromptIsScaffoldOnly locks the no-op boundary: the scaffold
// observes and logs, and explicitly takes no detection or recovery action.
// Detection (.3) and recovery (.4) must therefore be a deliberate edit
// here, not a silent regression.
func TestArgosPromptIsScaffoldOnly(t *testing.T) {
	body := renderArgosPrompt(t)

	// The three actions a scaffold wake performs, fully rendered.
	for _, want := range []string{
		"gc session list --json",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos prompt missing required scaffold command %q", want)
		}
	}

	// Explicit no-op disclaimers — the scaffold must announce that it does
	// not yet detect or recover.
	for _, want := range []string{
		"No detection, no nudges, no warrants.",
		"What Argos does NOT do (yet)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos prompt missing scaffold-boundary disclaimer %q", want)
		}
	}
}

// renderArgosPrompt renders the argos prompt template with the same funcs
// and context the gastown agent prompts use.
func renderArgosPrompt(t *testing.T) string {
	t.Helper()
	return renderGastownPromptForPack(t,
		filepath.Join("packs", "argos", "agents", "argos", "prompt.template.md"),
		"argos", "argos", "", "", "")
}
