package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// isolationCity describes a minimal on-disk city for the
// workspace.implicit_agent_isolation load tests.
type isolationCity struct {
	// isolation is written as [workspace] implicit_agent_isolation when set.
	isolation string
	// withCore imports a local pack named "core".
	withCore bool
	// withScript ships assets/scripts/worktree-setup.sh in that core pack.
	withScript bool
	// extra is appended to city.toml verbatim.
	extra string
}

// writeIsolationCity writes the city under a fresh temp dir and returns the
// city.toml path and the core pack directory. GC_HOME points at an empty dir
// so no bootstrap implicit imports compose.
func writeIsolationCity(t *testing.T, c isolationCity) (cityToml, coreDir string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	dir := t.TempDir()
	coreDir = filepath.Join(dir, "packs", "core")
	if c.withCore {
		if err := os.MkdirAll(filepath.Join(coreDir, "assets", "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(coreDir, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c.withScript {
			if err := os.WriteFile(filepath.Join(coreDir, "assets", "scripts", "worktree-setup.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test\"\n")
	if c.isolation != "" {
		b.WriteString("implicit_agent_isolation = \"" + c.isolation + "\"\n")
	}
	b.WriteString("\n[providers.claude]\n\n[[rigs]]\nname = \"demo\"\npath = \"" + filepath.Join(dir, "repos", "demo") + "\"\n")
	if c.withCore {
		b.WriteString("\n[imports.core]\nsource = \"./packs/core\"\n")
	}
	b.WriteString(c.extra)
	cityToml = filepath.Join(dir, "city.toml")
	if err := os.WriteFile(cityToml, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityToml, coreDir
}

func loadIsolationCity(t *testing.T, c isolationCity) (*City, string) {
	t.Helper()
	cityToml, coreDir := writeIsolationCity(t, c)
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityToml)
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	return cfg, coreDir
}

func implicitAgentsOf(cfg *City) []Agent {
	var out []Agent
	for _, a := range cfg.Agents {
		if a.Implicit {
			out = append(out, a)
		}
	}
	return out
}

// AC-3: absent and "shared" isolation inject exactly today's implicit agents.
func TestImplicitAgentIsolationSharedKeepsTodaysAgents(t *testing.T) {
	absent, _ := loadIsolationCity(t, isolationCity{withCore: true, withScript: true})
	shared, _ := loadIsolationCity(t, isolationCity{isolation: "shared", withCore: true, withScript: true})

	if got := shared.Workspace.ImplicitAgentIsolation; got != ImplicitAgentIsolationShared {
		t.Fatalf("workspace.implicit_agent_isolation = %q, want %q", got, ImplicitAgentIsolationShared)
	}
	if shared.Workspace.ImplicitAgentWorktreeIsolation() {
		t.Fatal(`"shared" must not select worktree isolation`)
	}

	absentAgents := implicitAgentsOf(absent)
	sharedAgents := implicitAgentsOf(shared)
	if len(absentAgents) == 0 {
		t.Fatal("no implicit agents injected")
	}
	if got := filepath.Base(absentAgents[0].PromptTemplate); got != "pool-worker.template.md" {
		t.Fatalf("implicit prompt_template = %q; the core pack did not compose", absentAgents[0].PromptTemplate)
	}
	for _, agents := range [][]Agent{absentAgents, sharedAgents} {
		for _, a := range agents {
			if a.WorkDir != "" || len(a.PreStart) != 0 {
				t.Errorf("implicit agent %s: work_dir=%q pre_start=%v, want both empty", a.QualifiedName(), a.WorkDir, a.PreStart)
			}
		}
	}
	// PromptTemplate is rooted at each city's own core dir; the rest of the
	// agent, and so its config fingerprint, must match field for field.
	normalize := func(agents []Agent) []Agent {
		out := make([]Agent, len(agents))
		for i, a := range agents {
			a.PromptTemplate = filepath.Base(a.PromptTemplate)
			out[i] = a
		}
		return out
	}
	if !reflect.DeepEqual(normalize(absentAgents), normalize(sharedAgents)) {
		t.Fatalf("shared implicit agents differ from absent:\nabsent=%+v\nshared=%+v", absentAgents, sharedAgents)
	}
}

// AC-3: the key merges from a fragment like the other workspace strings.
func TestImplicitAgentIsolationMergesFromFragment(t *testing.T) {
	base := &City{Workspace: Workspace{Name: "test"}}
	fragment := &City{Workspace: Workspace{ImplicitAgentIsolation: "shared"}}
	md, err := toml.Decode("[workspace]\nimplicit_agent_isolation = \"shared\"\n", &City{})
	if err != nil {
		t.Fatal(err)
	}
	prov := &Provenance{Workspace: map[string]string{}}
	mergeWorkspace(base, fragment, md, "frag.toml", prov)
	if base.Workspace.ImplicitAgentIsolation != "shared" {
		t.Fatalf("merged implicit_agent_isolation = %q, want shared", base.Workspace.ImplicitAgentIsolation)
	}
	if prov.Workspace["implicit_agent_isolation"] != "frag.toml" {
		t.Fatalf("provenance = %v, want implicit_agent_isolation from frag.toml", prov.Workspace)
	}
}

func findAgent(t *testing.T, cfg *City, dir, name string) Agent {
	t.Helper()
	for _, a := range cfg.Agents {
		if a.Dir == dir && a.Name == name {
			return a
		}
	}
	t.Fatalf("agent %s/%s not found in %d agents", dir, name, len(cfg.Agents))
	return Agent{}
}

// AC-4: worktree mode isolates rig-scoped implicit agents only.
func TestImplicitAgentIsolationWorktreeInjectsWorkDirAndPreStart(t *testing.T) {
	explicit := "\n[[agent]]\nname = \"builder\"\ndir = \"demo\"\n"
	shared, _ := loadIsolationCity(t, isolationCity{withCore: true, withScript: true, extra: explicit})
	cfg, coreDir := loadIsolationCity(t, isolationCity{isolation: "worktree", withCore: true, withScript: true, extra: explicit})

	rigAgent := findAgent(t, cfg, "demo", "claude")
	if rigAgent.WorkDir != ".gc/worktrees/{{.Rig}}/{{.AgentBase}}" {
		t.Fatalf("rig implicit work_dir = %q, want .gc/worktrees/{{.Rig}}/{{.AgentBase}}", rigAgent.WorkDir)
	}
	wantPreStart := []string{filepath.Join(coreDir, "assets", "scripts", "worktree-setup.sh") + " {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"}
	if !reflect.DeepEqual(rigAgent.PreStart, wantPreStart) {
		t.Fatalf("rig implicit pre_start = %q, want %q", rigAgent.PreStart, wantPreStart)
	}

	// City-scoped implicit agents and explicit agents are untouched.
	cityAgent := findAgent(t, cfg, "", "claude")
	sharedCityAgent := findAgent(t, shared, "", "claude")
	if cityAgent.WorkDir != sharedCityAgent.WorkDir || !reflect.DeepEqual(cityAgent.PreStart, sharedCityAgent.PreStart) {
		t.Fatalf("city implicit agent changed: work_dir=%q pre_start=%q, want work_dir=%q pre_start=%q",
			cityAgent.WorkDir, cityAgent.PreStart, sharedCityAgent.WorkDir, sharedCityAgent.PreStart)
	}
	builder := findAgent(t, cfg, "demo", "builder")
	if builder.WorkDir != "" || len(builder.PreStart) != 0 {
		t.Fatalf("explicit agent changed: work_dir=%q pre_start=%q", builder.WorkDir, builder.PreStart)
	}
}

// AC-4: a deferred [[patches.agent]] still overrides one isolated agent.
func TestImplicitAgentIsolationWorktreePatchOverridesWorkDir(t *testing.T) {
	patch := "\n[[patches.agent]]\ndir = \"demo\"\nname = \"claude\"\nwork_dir = \".gc/custom/{{.AgentBase}}\"\n"
	cfg, _ := loadIsolationCity(t, isolationCity{isolation: "worktree", withCore: true, withScript: true, extra: patch})
	if got := findAgent(t, cfg, "demo", "claude").WorkDir; got != ".gc/custom/{{.AgentBase}}" {
		t.Fatalf("patched work_dir = %q, want .gc/custom/{{.AgentBase}}", got)
	}
}
