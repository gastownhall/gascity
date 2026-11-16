package api

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCanAttributeSessionUsesResolvedWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown", Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", WorkDir: ".gc/worktrees/{{.Rig}}/refinery", MaxActiveSessions: intPtr(1)},
			{Name: "witness", Dir: "demo", WorkDir: ".gc/agents/{{.Rig}}/witness", MaxActiveSessions: intPtr(1)},
		},
	}

	if !canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = false, want true for distinct workdirs")
	}
}

func TestCanAttributeSessionRejectsSharedRigRootWhenClaudePoolExists(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo-repo")
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "demo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when Claude pool shares rig root")
	}
}

func TestCanAttributeSessionRejectsSharedPoolTemplateEvenWhenItMentionsAgentIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", WorkDir: ".gc/shared", MaxActiveSessions: intPtr(1)},
			{
				Name:              "polecat",
				Dir:               "demo",
				WorkDir:           `{{if .AgentBase}}.gc/shared{{end}}`,
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
			},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when pooled template still resolves to a shared path")
	}
}

func TestCanAttributeSessionRejectsSharedSingleSlotPoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "observer", Dir: "demo", WorkDir: ".gc/shared/polecat", MaxActiveSessions: intPtr(1)},
			{
				Name:              "polecat",
				Dir:               "demo",
				WorkDir:           ".gc/shared/{{.AgentBase}}",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
			},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/observer", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when a single-slot pool shares the observed workdir")
	}
}

// wrappedProviders declares one provider per family outcome the attribution
// count must tell apart: a claude wrapper, a codex wrapper, and a wrapper whose
// base names no provider (its family is undetermined, which reads as not
// claude).
func wrappedProviders() map[string]config.ProviderSpec {
	claudeBase, codexBase, missingBase := "builtin:claude", "builtin:codex", "builtin:no-such-provider"
	return map[string]config.ProviderSpec{
		"claude-wrap": {Base: &claudeBase},
		"codex-wrap":  {Base: &codexBase},
		"broken-wrap": {Base: &missingBase},
	}
}

func TestCanAttributeSessionCountsClaudeFamilyProviders(t *testing.T) {
	pool := func(provider string) config.Agent {
		return config.Agent{
			Name: "polecat", Dir: "demo", Provider: provider,
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
		}
	}
	named := func(name, provider string) config.Agent {
		return config.Agent{Name: name, Dir: "demo", Provider: provider, MaxActiveSessions: intPtr(1)}
	}
	cases := []struct {
		name              string
		workspaceProvider string
		agents            []config.Agent
		// want[i] is canAttributeSession for agents[i]; every agent shares
		// the rig root unless it is a pool template, which is never
		// attributed itself (want is ignored for it).
		want []bool
	}{
		{
			name:   "wrapped claude next to raw claude on one workdir: neither is attributable",
			agents: []config.Agent{named("refinery", "claude"), named("observer", "claude-wrap")},
			want:   []bool{false, false},
		},
		{
			name:   "two wrapped claude agents on one workdir: neither is attributable",
			agents: []config.Agent{named("refinery", "claude-wrap"), named("observer", "claude-wrap")},
			want:   []bool{false, false},
		},
		{
			name:              "workspace default wrapped claude next to raw claude: neither is attributable",
			workspaceProvider: "claude-wrap",
			agents:            []config.Agent{named("refinery", ""), named("observer", "claude")},
			want:              []bool{false, false},
		},
		{
			name:   "wrapped claude pool template shares the rig root with a raw claude agent",
			agents: []config.Agent{named("refinery", "claude"), pool("claude-wrap")},
			want:   []bool{false, false},
		},
		{
			name:   "raw claude pool template shares the rig root with a wrapped claude agent",
			agents: []config.Agent{named("refinery", "claude-wrap"), pool("claude")},
			want:   []bool{false, false},
		},
		{
			name:   "wrapped claude pool template shares the rig root with a wrapped claude agent",
			agents: []config.Agent{named("refinery", "claude-wrap"), pool("claude-wrap")},
			want:   []bool{false, false},
		},
		{
			name:   "wrapped claude agent alone on its workdir is attributable",
			agents: []config.Agent{named("refinery", "claude-wrap")},
			want:   []bool{true},
		},
		{
			name:   "codex wrapper next to claude on one workdir does not count: claude stays attributable",
			agents: []config.Agent{named("refinery", "claude"), named("observer", "codex-wrap")},
			want:   []bool{true, true},
		},
		{
			name:   "wrapper with an unresolvable base reads as not claude: claude stays attributable",
			agents: []config.Agent{named("refinery", "claude"), named("observer", "broken-wrap")},
			want:   []bool{true, true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Provider: tc.workspaceProvider},
				Providers: wrappedProviders(),
				Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(t.TempDir(), "demo-repo")}},
				Agents:    tc.agents,
			}
			for i, a := range tc.agents {
				if isMultiSessionAgent(a) {
					continue
				}
				if got := canAttributeSession(a, "demo/"+a.Name, cfg, cityPath); got != tc.want[i] {
					t.Errorf("canAttributeSession(%s) = %v, want %v", a.Name, got, tc.want[i])
				}
			}
		})
	}
}

func TestResolveSessionTemplateUsesConfiguredWorkDir(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "worktrees", "myrig", "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}

func TestResolveSessionTemplateUsesCityNameFallbackForWorkDirTemplates(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Name = ""
	state.cfg.Agents[0].WorkDir = ".gc/agents/{{.CityName}}/{{.AgentBase}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "agents", filepath.Base(state.cityPath), "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}

func TestResolveSessionTemplateUsesQualifiedNameForWorkDirTemplates(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.Agent}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "worktrees", "myrig", "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}
