package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

// TestResolveTemplateSkillsIntegration is the end-to-end regression for
// Phase 3 pass-1 Claude finding #3. It exercises step 11b of
// resolveTemplate end-to-end and asserts that:
//
//  1. Stage-2 eligible agent (tmux session, non-ACP) with
//     WorkDir == scope root → FPExtra contains skills:<name>; no
//     materialize-skills PreStart entry.
//  2. Stage-2 eligible agent with WorkDir != scope root →
//     FPExtra contains skills:<name>; PreStart ends with the
//     materialize-skills command.
//  3. ACP agent → FPExtra has no skills:*; no PreStart materialize-skills.
//  4. K8s session → FPExtra has no skills:*; no PreStart materialize-skills.
//
// Without this test, a refactor could drop or invert step 11b and the
// helper-level tests would still pass.
func TestResolveTemplateSkillsIntegration(t *testing.T) {
	cityPath := t.TempDir()
	// Minimal city.toml + pack.toml so PackSkillsDir populates and
	// the shared catalog discovery picks up skills/.
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"skills-test\"\nversion = \"0.1.0\"\nschema = 2\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	// Write a skill source that the materializer will enumerate.
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-load the city catalog by calling the real discovery path
	// against cityPath/skills.
	sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedCat.Entries) != 1 || sharedCat.Entries[0].Name != "plan" {
		t.Fatalf("unexpected catalog: %+v", sharedCat)
	}

	makeParams := func(sessionProvider string) *agentBuildParams {
		return &agentBuildParams{
			cityName:  "city",
			cityPath:  cityPath,
			workspace: &config.Workspace{Provider: "claude"},
			providers: map[string]config.ProviderSpec{
				"claude": {Command: "echo", PromptMode: "none", SupportsACP: true},
			},
			lookPath:        func(string) (string, error) { return "/bin/echo", nil },
			fs:              fsys.OSFS{},
			rigs:            []config.Rig{},
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			skillCatalog:    &sharedCat,
			sessionProvider: sessionProvider,
		}
	}

	cases := []struct {
		name               string
		sessionProvider    string
		agent              *config.Agent
		wantSkillsKey      bool // expect FPExtra["skills:plan"] populated
		wantMaterializeCmd bool // expect PreStart ends with materialize-skills invocation
	}{
		{
			name:               "tmux + workdir == scope root",
			sessionProvider:    "tmux",
			agent:              &config.Agent{Name: "mayor", Scope: "city", Provider: "claude"},
			wantSkillsKey:      true,
			wantMaterializeCmd: false,
		},
		{
			name:            "tmux + workdir != scope root",
			sessionProvider: "tmux",
			agent: &config.Agent{
				Name:     "polecat",
				Scope:    "city",
				Provider: "claude",
				WorkDir:  ".gc/worktrees/polecat-1",
			},
			wantSkillsKey:      true,
			wantMaterializeCmd: true,
		},
		{
			name:            "acp session ineligible",
			sessionProvider: "tmux",
			agent: &config.Agent{
				Name:     "witness",
				Scope:    "city",
				Provider: "claude",
				Session:  "acp",
			},
			wantSkillsKey:      false,
			wantMaterializeCmd: false,
		},
		{
			name:               "k8s city session ineligible",
			sessionProvider:    "k8s",
			agent:              &config.Agent{Name: "pod-worker", Scope: "city", Provider: "claude"},
			wantSkillsKey:      false,
			wantMaterializeCmd: false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			params := makeParams(c.sessionProvider)
			tp, err := resolveTemplate(params, c.agent, c.agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}

			_, haveKey := tp.FPExtra["skills:plan"]
			if haveKey != c.wantSkillsKey {
				t.Errorf("FPExtra[skills:plan] present = %v, want %v; FPExtra=%+v",
					haveKey, c.wantSkillsKey, tp.FPExtra)
			}
			if haveKey {
				if tp.FPExtra["skills:plan"] == "" {
					t.Errorf("FPExtra[skills:plan] empty; want non-empty hash")
				}
			}

			foundCmd := false
			for _, entry := range tp.Hints.PreStart {
				if strings.Contains(entry, "internal materialize-skills") {
					foundCmd = true
					break
				}
			}
			if foundCmd != c.wantMaterializeCmd {
				t.Errorf("PreStart materialize-skills present = %v, want %v; PreStart=%v",
					foundCmd, c.wantMaterializeCmd, tp.Hints.PreStart)
			}
		})
	}
}

// TestResolveTemplateAppendsAssignedSkillsPrompt verifies that the
// assigned-skills appendix lands at the tail of the rendered prompt
// for every stage-1-eligible agent with a vendor sink (by default).
// Opt-out via InjectAssignedSkills = &false is honored.
func TestResolveTemplateAppendsAssignedSkillsPrompt(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"s\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: Plan the work\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}

	buildParams := func() *agentBuildParams {
		return &agentBuildParams{
			cityName:  "city",
			cityPath:  cityPath,
			workspace: &config.Workspace{Provider: "claude"},
			providers: map[string]config.ProviderSpec{
				"claude": {Command: "echo", PromptMode: "none"},
			},
			lookPath:        func(string) (string, error) { return "/bin/echo", nil },
			fs:              fsys.OSFS{},
			rigs:            []config.Rig{},
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			skillCatalog:    &sharedCat,
			sessionProvider: "tmux",
		}
	}

	t.Run("default inject", func(t *testing.T) {
		a := &config.Agent{Name: "mayor", Scope: "city", Provider: "claude"}
		tp, err := resolveTemplate(buildParams(), a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{
			"## Skills available to this session",
			"`plan` — Plan the work",
		} {
			if !strings.Contains(tp.Prompt, needle) {
				t.Errorf("prompt missing %q:\n%s", needle, tp.Prompt)
			}
		}
	})

	t.Run("opt out", func(t *testing.T) {
		no := false
		a := &config.Agent{Name: "quiet", Scope: "city", Provider: "claude", InjectAssignedSkills: &no}
		tp, err := resolveTemplate(buildParams(), a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("opt-out agent got the appendix anyway:\n%s", tp.Prompt)
		}
	})

	t.Run("no sink skipped", func(t *testing.T) {
		// provider=copilot has no vendor sink → no appendix
		params := buildParams()
		params.providers["copilot"] = config.ProviderSpec{Command: "echo", PromptMode: "none"}
		a := &config.Agent{Name: "sinkless", Scope: "city", Provider: "copilot"}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("sinkless agent got the appendix:\n%s", tp.Prompt)
		}
	})

	// Runtime gating — pass-1 Codex review regression. The appendix
	// must NOT fire for agents whose runtime can't deliver the skills,
	// otherwise the prompt lies ("skills are materialized and load
	// automatically") to an agent whose sink is never populated.
	t.Run("k8s city session skipped", func(t *testing.T) {
		params := buildParams()
		params.sessionProvider = "k8s"
		a := &config.Agent{Name: "pod-worker", Scope: "city", Provider: "claude"}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("k8s session got the appendix despite materialization not reaching it:\n%s", tp.Prompt)
		}
	})

	t.Run("acp agent skipped", func(t *testing.T) {
		// Give the provider ACP support so resolveTemplate accepts
		// session = "acp"; the materialization gate is what should
		// reject it.
		params := buildParams()
		params.providers["claude"] = config.ProviderSpec{Command: "echo", PromptMode: "none", SupportsACP: true}
		a := &config.Agent{Name: "witness", Scope: "city", Provider: "claude", Session: "acp"}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("acp agent got the appendix despite stage-1/stage-2 ineligibility:\n%s", tp.Prompt)
		}
	})

	t.Run("subprocess workdir differs skipped", func(t *testing.T) {
		// subprocess is stage-1-eligible (host scope root is
		// reachable) but NOT stage-2-eligible (no PreStart execution).
		// When WorkDir != scope root, stage 1 delivers to the scope
		// root but not the workdir, and stage 2 doesn't run — so the
		// agent's workdir sink stays empty. No appendix.
		params := buildParams()
		params.sessionProvider = "subprocess"
		a := &config.Agent{
			Name:     "sub",
			Scope:    "city",
			Provider: "claude",
			WorkDir:  ".gc/worktrees/sub-1",
		}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("subprocess worktree session got the appendix despite stage-2 ineligibility:\n%s", tp.Prompt)
		}
	})

	t.Run("subprocess workdir at scope root gets appendix", func(t *testing.T) {
		// Same subprocess runtime, but WorkDir is the scope root —
		// stage 1 delivers directly to the workdir-equivalent sink.
		params := buildParams()
		params.sessionProvider = "subprocess"
		a := &config.Agent{Name: "sub", Scope: "city", Provider: "claude"}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tp.Prompt, "Skills available to this session") {
			t.Errorf("subprocess@scope-root should get the appendix:\n%s", tp.Prompt)
		}
	})
}

// TestResolveTemplatePoolInstanceMaterializeUsesTemplateName is the
// v0.15.1-rc1 → rc2 regression. Pool instances (especially namepool-
// themed ones like polecat → furiosa) must route the stage-2 PreStart
// `gc internal materialize-skills --agent` flag at the TEMPLATE's
// qualified name, not the instance's. The materialize-skills command
// resolves the agent via resolveAgentIdentity, which cannot map a
// namepool member (`rig/furiosa`) back to its template (`rig/polecat`)
// — it treats `rig/furiosa` as an unknown agent and exits with code 1,
// failing pre_start[1] on every polecat start in tier C. See
// TestGastown_PolecatImplementsRefineryMerges.
func TestResolveTemplatePoolInstanceMaterializeUsesTemplateName(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"s\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: test\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}

	// Namepool-themed instance: template is "rig/polecat" (PoolName),
	// concrete instance name is "furiosa", Dir="rig".
	// WorkDir != scope root so stage-2 PreStart injection fires.
	instance := &config.Agent{
		Name:     "furiosa",
		Dir:      "rig",
		Scope:    "rig",
		Provider: "claude",
		WorkDir:  ".gc/worktrees/rig/polecats/furiosa",
		PoolName: "rig/polecat",
	}

	params := &agentBuildParams{
		cityName:  "city",
		cityPath:  cityPath,
		workspace: &config.Workspace{Provider: "claude"},
		providers: map[string]config.ProviderSpec{
			"claude": {Command: "echo", PromptMode: "none"},
		},
		lookPath:        func(string) (string, error) { return "/bin/echo", nil },
		fs:              fsys.OSFS{},
		rigs:            []config.Rig{{Name: "rig", Path: filepath.Join(cityPath, "rig")}},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		skillCatalog:    &sharedCat,
		sessionProvider: "tmux",
	}

	tp, err := resolveTemplate(params, instance, instance.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	var materializeCmd string
	for _, entry := range tp.Hints.PreStart {
		if strings.Contains(entry, "internal materialize-skills") {
			materializeCmd = entry
			break
		}
	}
	if materializeCmd == "" {
		t.Fatalf("expected stage-2 materialize-skills PreStart entry; PreStart=%v", tp.Hints.PreStart)
	}

	// The --agent flag must carry the TEMPLATE qualified name, not the
	// instance. `gc internal materialize-skills --agent rig/furiosa`
	// exits 1 with "unknown agent" because resolveAgentIdentity can't
	// walk a namepool member back to its pool template.
	// shellquote.Join emits bare (unquoted) tokens when no escaping is
	// needed, so match on the raw substring after --agent.
	if !strings.Contains(materializeCmd, "--agent rig/polecat") {
		t.Errorf("materialize-skills --agent flag should carry template name rig/polecat; got: %q", materializeCmd)
	}
	if strings.Contains(materializeCmd, "--agent rig/furiosa") {
		t.Errorf("materialize-skills --agent flag must NOT carry namepool instance name rig/furiosa; got: %q", materializeCmd)
	}

	// Non-pool singleton: cfgAgent.PoolName is empty, so the cmd carries
	// the agent's own qualified name. Guards against over-correction
	// where templateNameFor's fallback breaks non-pool cases.
	singleton := &config.Agent{
		Name:     "mayor",
		Scope:    "city",
		Provider: "claude",
		WorkDir:  ".gc/agents/mayor",
	}
	tp2, err := resolveTemplate(params, singleton, singleton.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate singleton: %v", err)
	}
	var singletonCmd string
	for _, entry := range tp2.Hints.PreStart {
		if strings.Contains(entry, "internal materialize-skills") {
			singletonCmd = entry
			break
		}
	}
	if singletonCmd != "" && !strings.Contains(singletonCmd, "--agent mayor") {
		t.Errorf("singleton materialize-skills should carry own qualified name mayor; got: %q", singletonCmd)
	}
}
