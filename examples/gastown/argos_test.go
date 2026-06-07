// Package gastown_test also validates the argos watchdog pack, which the
// gastown example city composes alongside its domain roster.
//
// Argos is the rate-limit-recovery watchdog: a city-scoped, always-resident
// named session modeled on the boot agent that wakes each patrol tick, runs a
// single pass of triage with a fresh provider context, then drain-acks and
// exits. At this (.3) stage the triage is read-only detection — it enumerates
// every session, cross-references claimed work, peeks the pane, and classifies
// each session, but takes no recovery action. These tests pin the agent's
// structure (so the lifecycle is what the prompt documents) and its read-only
// boundary (so recovery arrives as a deliberate, reviewed change in .4, not by
// accident).
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

// TestArgosPromptIsReadOnlyDetection locks the .3 boundary: the prompt now
// DETECTS and CLASSIFIES (read-only triage), but still takes no recovery
// action. Recovery (.4) — nudges, wakes, unclaims, warrants — must therefore
// be a deliberate edit here, not a silent regression.
func TestArgosPromptIsReadOnlyDetection(t *testing.T) {
	body := renderArgosPrompt(t)
	lower := strings.ToLower(body)

	// Observation surface: enumerate, cross-reference claimed work, confirm
	// by reading the pane, close the single-pass wake.
	for _, want := range []string{
		"gc session list --json", // enumerate the field of view
		"--status=in_progress",   // list claimed, in-progress work
		"assignee",               // the session_name == assignee join
		"gc session peek",        // confirm by reading the pane
		"gc runtime drain-ack",   // single-pass lifecycle close
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos detection prompt missing observation step %q", want)
		}
	}

	// The four verdicts the read-only triage classifies into.
	for _, want := range []string{
		"healthy",
		"idle-no-work",
		"rate-limit-stalled",
		"context-frozen",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos detection prompt missing classification %q", want)
		}
	}

	// The fire gate is a conjunction: a claimed in-progress bead AND the
	// rate-limit marker visible on the pane. Both clauses must be named.
	for _, want := range []string{
		"fire gate",     // named explicitly so the conjunction is unmissable
		"claimed",       // the work-in-flight clause
		"session limit", // the marker clause (the b7691626 inline wall)
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("argos detection prompt missing fire-gate element %q", want)
		}
	}

	// Read-only boundary: the prompt declares itself read-only and issues NO
	// mutating or recovery command. Detection classifies; .4 acts.
	if !strings.Contains(lower, "read-only") {
		t.Error("argos detection prompt must declare itself read-only")
	}
	for _, forbidden := range []string{
		"gc session nudge",
		"gc session wake",
		"gc bd update",
		"gc mail send",
		"--label=warrant",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("argos .3 is read-only; prompt must not issue recovery command %q", forbidden)
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
