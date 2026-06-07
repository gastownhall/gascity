// Package gastown_test also validates the argos watchdog pack, which the
// gastown example city composes alongside its domain roster.
//
// Argos is the rate-limit-recovery watchdog: a city-scoped, always-resident
// named session modeled on the boot agent that wakes each patrol tick, runs a
// single pass of triage with a fresh provider context, then drain-acks and
// exits. At this (.4) stage the triage detects AND recovers — it enumerates
// every session, cross-references claimed work, peeks the pane, classifies each
// session, and then acts on a recoverable stall: it nudges a rate-limit wall
// with "continue" (waking a suspended holder first), "/compact"s a frozen
// context, times the nudge to the wall's reset window, and backs off via the
// observable last_nudge_delivered_at so consecutive ticks never storm. These
// tests pin the agent's structure (so the lifecycle is what the prompt
// documents) and its recovery contract (the actions it takes, the timing and
// anti-storm it obeys, and the actions it still must NOT take — unclaim,
// mayor-mail escalation, warrants, kills).
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

// TestArgosPromptRecovers locks the .4 boundary: detection graduates to
// recovery. The prompt must (1) keep the detection surface intact as the input
// contract, (2) issue the recovery actions — "continue" for a rate-limit wall,
// wake-then-nudge for a suspended holder, "/compact" for a frozen context —
// (3) time the nudge to the wall's reset window with a blind tiered-backoff
// fallback, (4) derive its anti-storm backoff from the observable
// last_nudge_delivered_at rather than a status file, and (5) still NOT unclaim
// work, escalate by mayor-mail, file warrants, or kill sessions.
func TestArgosPromptRecovers(t *testing.T) {
	body := renderArgosPrompt(t)
	lower := strings.ToLower(body)

	// Input contract: the detection observation surface is unchanged — it is
	// what recovery consumes.
	for _, want := range []string{
		"gc session list --json", // enumerate the field of view
		"--status=in_progress",   // list claimed, in-progress work
		"assignee",               // the session_name == assignee join
		"gc session peek",        // confirm by reading the pane
		"gc runtime drain-ack",   // single-pass lifecycle close
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos recovery prompt dropped detection step %q", want)
		}
	}

	// The four verdicts still drive the decision.
	for _, want := range []string{
		"healthy",
		"idle-no-work",
		"rate-limit-stalled",
		"context-frozen",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos recovery prompt missing classification %q", want)
		}
	}

	// The fire gate conjunction is still named: a claimed in-progress bead AND
	// the rate-limit marker visible on the pane.
	for _, want := range []string{
		"fire gate",     // named explicitly so the conjunction is unmissable
		"claimed",       // the work-in-flight clause
		"session limit", // the marker clause (the b7691626 inline wall)
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("argos recovery prompt missing fire-gate element %q", want)
		}
	}

	// Recovery actions: the verbs the act step issues.
	for _, want := range []string{
		"gc session nudge", // the core recovery
		"gc session wake",  // suspended -> wake, then nudge
		"continue",         // the rate-limit nudge payload
		"/compact",         // the context-frozen recovery
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argos recovery prompt missing recovery action %q", want)
		}
	}

	// Reset-window timing (primary) with a blind tiered-backoff fallback.
	if !strings.Contains(lower, "resets") {
		t.Error("argos recovery prompt must time the nudge to the wall's reset window")
	}
	for _, tier := range []string{"15m", "30m", "1h", "2h", "4h"} {
		if !strings.Contains(lower, tier) {
			t.Errorf("argos recovery prompt missing backoff tier %q", tier)
		}
	}

	// Anti-storm derives from observable state, not a status file.
	if !strings.Contains(lower, "last_nudge_delivered_at") {
		t.Error("argos recovery prompt must derive backoff from the observable last_nudge_delivered_at, not a status file")
	}

	// No-unclaim contract: the orphan sweep, not Argos, reclaims dead-owner
	// work, so the prompt must say so and must not mutate work beads. It also
	// must not escalate by mail, file warrants, or kill sessions.
	if !strings.Contains(lower, "orphan") {
		t.Error("argos recovery prompt must explain that the orphan sweep, not Argos, owns unclaim")
	}
	for _, forbidden := range []string{
		"gc bd update",    // no unclaim / no work-bead mutation
		"gc mail send",    // no mayor escalation (cut by design)
		"gc session kill", // no kill/restart
		"--label=warrant", // no warrants (boot/dog own those)
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("argos recovery prompt must not issue %q", forbidden)
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
