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

// TestArgosWiredIntoCity is the .5 city-wiring regression: the example city
// must import the argos pack and expand it into a runnable, always-resident
// city agent, so the watchdog actually comes up when the city does. It pins
// the whole edge — the [imports.argos] line in the city's root pack.toml, the
// always-resident named session, the agent it resolves to, and the prompt file
// on disk — so a future refactor cannot silently drop the watchdog from the
// roster.
func TestArgosWiredIntoCity(t *testing.T) {
	dir := exampleDir()

	// 1. The city's root pack.toml imports the argos pack — the single line
	//    that wires the watchdog into the deployment.
	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("reading city pack.toml: %v", err)
	}
	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing city pack.toml: %v", err)
	}
	argosImp, ok := tc.Imports["argos"]
	if !ok {
		t.Fatalf("city pack.toml imports = %v, want an entry for \"argos\" so the watchdog is wired in", tc.Imports)
	}
	if argosImp.Source != "packs/argos" {
		t.Errorf("city pack.toml imports[\"argos\"].Source = %q, want %q", argosImp.Source, "packs/argos")
	}

	// 2. The expanded city keeps argos resident and resolves it to a real
	//    agent whose prompt template exists on disk — i.e. it would actually
	//    run, not just parse.
	cfg := loadExpanded(t)
	session := config.FindNamedSession(cfg, "argos")
	if session == nil {
		t.Fatal("expanded city has no argos named_session; the import did not bring the watchdog up")
	}
	if got := session.ModeOrDefault(); got != "always" {
		t.Errorf("argos named_session mode = %q, want %q so the reconciler re-wakes it each patrol tick", got, "always")
	}
	agent := config.FindAgent(cfg, session.TemplateQualifiedName())
	if agent == nil {
		t.Fatalf("argos named_session resolves to no agent (%q)", session.TemplateQualifiedName())
	}
	if agent.Scope != "city" {
		t.Errorf("argos agent scope = %q, want %q", agent.Scope, "city")
	}
	if agent.PromptTemplate == "" {
		t.Fatal("argos agent has no prompt_template")
	}
	if _, err := os.Stat(resolveExamplePath(dir, agent.PromptTemplate)); err != nil {
		t.Errorf("argos prompt_template %q not on disk: %v", agent.PromptTemplate, err)
	}
}

// TestArgosPatrolScenarioContract is the .5 integration/regression test. It
// pins the canonical patrol scenarios from the #2194 watchdog design — the
// four cases the bead enumerates plus their two rate-limit shapes — to the
// verdict the prompt assigns and the action it takes, read straight off the
// pack/CLI surface (the rendered prompt). Each case is anchored by a live-state
// description, never a role name: ZFC keeps the judgment in the prompt, and
// this Go only checks that the prompt's contract table binds each observable
// situation to the right verdict and the right gc command. If a later edit
// re-wires a verdict to the wrong action, drops a leave-it-alone case, or
// forgets the wake-before-nudge order for a suspended holder, this matrix
// breaks loudly.
func TestArgosPatrolScenarioContract(t *testing.T) {
	body := renderArgosPrompt(t)
	table := sectionBetween(t, body, "## Patrol scenarios (the contract)", "\n## ")

	cases := []struct {
		name string
		// anchor uniquely identifies one scenario row by the live state it
		// describes (no role name).
		anchor string
		// verdict and actions must all appear on that same row, so the
		// situation→verdict→action binding cannot drift apart.
		verdict string
		actions []string
	}{
		{
			name:    "active rate-limit wall over claimed work recovers with continue",
			anchor:  "**active**",
			verdict: "`rate-limit-stalled`",
			actions: []string{"continue"},
		},
		{
			name:    "suspended holder is woken before the continue nudge",
			anchor:  "**suspended**",
			verdict: "`rate-limit-stalled`",
			actions: []string{"gc session wake", "continue"},
		},
		{
			name:    "healthy advancing pane is left alone",
			anchor:  "advancing",
			verdict: "`healthy`",
			actions: []string{"leave it alone"},
		},
		{
			name:    "idle holder with no claimed work is left alone",
			anchor:  "**no** claimed",
			verdict: "`idle-no-work`",
			actions: []string{"leave it alone"},
		},
		{
			name:    "frozen context over claimed work recovers with compact",
			anchor:  "wedged context",
			verdict: "`context-frozen`",
			actions: []string{"/compact"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := scenarioRow(t, table, c.anchor)
			if !strings.Contains(row, c.verdict) {
				t.Errorf("scenario %q row does not assign verdict %q:\n%s", c.anchor, c.verdict, row)
			}
			for _, action := range c.actions {
				if !strings.Contains(row, action) {
					t.Errorf("scenario %q row (verdict %q) does not bind action %q:\n%s", c.anchor, c.verdict, action, row)
				}
			}
		})
	}

	// The two leave-it-alone verdicts must never carry a nudge/wake verb on
	// their row, so a future edit cannot quietly start poking a healthy or
	// idle session.
	for _, leave := range []string{"advancing", "**no** claimed"} {
		row := scenarioRow(t, table, leave)
		for _, forbidden := range []string{"nudge", "gc session wake", "continue", "/compact"} {
			if strings.Contains(row, forbidden) {
				t.Errorf("leave-it-alone scenario %q row must not act, found %q:\n%s", leave, forbidden, row)
			}
		}
	}
}

// scenarioRow returns the single line of the patrol-scenario table that
// contains anchor, failing if anchor does not identify exactly one row.
func scenarioRow(t *testing.T, table, anchor string) string {
	t.Helper()
	var matches []string
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, anchor) {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("anchor %q matched %d scenario rows, want exactly 1:\n%s", anchor, len(matches), table)
	}
	return matches[0]
}

// renderArgosPrompt renders the argos prompt template with the same funcs
// and context the gastown agent prompts use.
func renderArgosPrompt(t *testing.T) string {
	t.Helper()
	return renderGastownPromptForPack(t,
		filepath.Join("packs", "argos", "agents", "argos", "prompt.template.md"),
		"argos", "argos", "", "", "")
}
