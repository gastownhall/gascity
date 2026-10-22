package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// coreWorktreeScriptRel is where the core pack ships worktree-setup.sh;
// worktree mode's injected pre_start points at it (absolute, because
// implicit agents have no SourceDir for {{.ConfigDir}} to resolve
// against — the promptTemplate precedent in InjectImplicitAgents).
const coreWorktreeScriptRel = "assets/scripts/worktree-setup.sh"

// composeFakeCorePack creates a pack directory that PackDirByName("core")
// resolves: a pack.toml naming the pack "core", optionally carrying the
// worktree-setup.sh script.
func composeFakeCorePack(t *testing.T, withScript bool) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, dir, "pack.toml", "[pack]\nname = \"core\"\nschema = 2\n")
	if withScript {
		writeTestFile(t, dir, coreWorktreeScriptRel, "#!/bin/sh\nexit 0\n")
	}
	return dir
}

// TestImplicitAgentIsolationParse covers decode of the new [workspace] key
// on a single layer. Enum enforcement lives in validateImplicitAgentIsolation
// and is covered by its own tests; this pins the field plumbing itself.
func TestImplicitAgentIsolationParse(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"shared", "shared"},
		{"worktree", "worktree"},
	} {
		out, err := Parse([]byte("[workspace]\nname = \"t\"\nimplicit_agent_isolation = \"" + tc.raw + "\"\n"))
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.raw, err)
		}
		if got := out.Workspace.ImplicitAgentIsolation; got != tc.want {
			t.Fatalf("decoded implicit_agent_isolation = %q, want %q", got, tc.want)
		}
	}
	// Absent stays empty — "shared" is the default without writing it.
	out, err := Parse([]byte("[workspace]\nname = \"t\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := out.Workspace.ImplicitAgentIsolation; got != "" {
		t.Fatalf("absent implicit_agent_isolation = %q, want empty", got)
	}
}

// TestImplicitAgentIsolationFragmentOverride pins mergeWorkspace pickup:
// a fragment that defines the key overrides the root layer (last-wins, same
// as the other workspace string fields), and a fragment that does not
// define it leaves the root's value alone. The composed value stays
// "shared" in both cases so composed-root core-pack validation stays out
// of the way; that validation has its own test.
func TestImplicitAgentIsolationFragmentOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
implicit_agent_isolation = "worktree"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[workspace]
implicit_agent_isolation = "shared"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Workspace.ImplicitAgentIsolation; got != "shared" {
		t.Fatalf("composed implicit_agent_isolation = %q, want the fragment's shared", got)
	}

	fs2 := fsys.NewFake()
	fs2.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
implicit_agent_isolation = "shared"
`)
	fs2.Files["/city/fragment.toml"] = []byte(`
[workspace]
timezone = "UTC"
`)
	cfg2, _, err := LoadWithIncludes(fs2, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg2.Workspace.ImplicitAgentIsolation; got != "shared" {
		t.Fatalf("composed implicit_agent_isolation = %q, want the root's shared to survive an unrelated fragment", got)
	}
}

// TestImplicitAgentIsolationRejectsOutOfEnum proves a typo fails config
// load rather than silently meaning "shared": an operator who writes
// "worktrees" must not believe isolation is on while every pool still
// shares the rig root.
func TestImplicitAgentIsolationRejectsOutOfEnum(t *testing.T) {
	for _, raw := range []string{"bogus", "true", "1", "Worktree", "none"} {
		t.Run(raw, func(t *testing.T) {
			_, err := Parse([]byte("[workspace]\nname = \"t\"\nimplicit_agent_isolation = \"" + raw + "\"\n"))
			if err == nil {
				t.Fatalf("expected an error for an out-of-enum implicit_agent_isolation value %q", raw)
			}
			for _, want := range []string{"workspace.implicit_agent_isolation", raw, "shared", "worktree"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
	// Whitespace is tolerated; the value itself is not folded.
	out, err := Parse([]byte("[workspace]\nname = \"t\"\nimplicit_agent_isolation = \" worktree \"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := out.Workspace.ImplicitAgentIsolation; got != " worktree " {
		t.Fatalf("decoded value = %q, want it preserved for composed-root validation", got)
	}
}

// TestImplicitAgentIsolationWorktreeRequiresCore proves the composed-root
// load path (the one every real gc invocation uses) rejects worktree mode
// when the core pack is not composed, and again when its
// worktree-setup.sh is missing on disk — without it the injected pre_start
// would point at a script that does not exist and sessions would start in
// an empty non-repo directory.
func TestImplicitAgentIsolationWorktreeRequiresCore(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
implicit_agent_isolation = "worktree"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes: want an error for worktree mode without the core pack, got nil")
	}
	if !strings.Contains(err.Error(), `workspace.implicit_agent_isolation = "worktree"`) || !strings.Contains(err.Error(), "core") {
		t.Fatalf("error = %q, want it to name the key and the missing core pack", err.Error())
	}

	// A composed core pack whose worktree-setup.sh is missing is the same
	// refusal, naming the script path. Import a real local "core" pack on
	// disk that does not ship the script.
	dir := t.TempDir()
	coreDir := filepath.Join(dir, "corepack")
	cityDir := filepath.Join(dir, "city")
	writeTestFile(t, coreDir, "pack.toml", "[pack]\nname = \"core\"\nschema = 2\n")
	rigRoot := filepath.Join(dir, "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
implicit_agent_isolation = "worktree"

[imports.core]
source = "../corepack"

[[rigs]]
name = "demo"
path = "`+rigRoot+`"
`)
	_, _, err = LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("LoadWithIncludes: want an error for a core pack without worktree-setup.sh, got nil")
	}
	wantScript := filepath.Join(coreDir, "assets", "scripts", "worktree-setup.sh")
	if !strings.Contains(err.Error(), wantScript) {
		t.Fatalf("error = %q, want it to name the missing script %q", err.Error(), wantScript)
	}

	// The same city with the script present loads and the injected implicit
	// pool carries the wired pre_start end to end — the core pack path
	// resolving through the composed import.
	writeTestFile(t, coreDir, "assets/scripts/worktree-setup.sh", "#!/bin/sh\nexit 0\n")
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes with the script present: %v", err)
	}
	cfg.Providers = map[string]ProviderSpec{"claude": {}}
	InjectImplicitAgents(cfg)
	var rigImplicit *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Implicit && cfg.Agents[i].Dir == "demo" {
			rigImplicit = &cfg.Agents[i]
			break
		}
	}
	if rigImplicit == nil {
		t.Fatal("no rig-scoped implicit agent after load")
	}
	wantPreStart := wantScript + " {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"
	if len(rigImplicit.PreStart) != 1 || rigImplicit.PreStart[0] != wantPreStart {
		t.Fatalf("PreStart = %v, want [%q]", rigImplicit.PreStart, wantPreStart)
	}
	if rigImplicit.WorkDir != ".gc/worktrees/{{.Rig}}/{{.AgentBase}}" {
		t.Fatalf("WorkDir = %q", rigImplicit.WorkDir)
	}
}

// TestInjectImplicitAgentsWorktreeMode covers the #1181 fix at injection
// time: in worktree mode each rig-scoped implicit agent gets the per-slot
// work_dir template and a pre_start invoking the core pack's
// worktree-setup.sh, while city-scoped implicit agents and explicit agents
// stay untouched.
func TestInjectImplicitAgentsWorktreeMode(t *testing.T) {
	coreDir := composeFakeCorePack(t, true)
	cfg := &City{
		Workspace: Workspace{ImplicitAgentIsolation: "worktree"},
		Providers: map[string]ProviderSpec{"claude": {}},
		PackDirs:  []string{coreDir},
		Rigs:      []Rig{{Name: "demo", Path: "/tmp/demo"}},
		Agents: []Agent{{
			Name: "claude",
			Dir:  "demo",
		}},
	}
	InjectImplicitAgents(cfg)

	var cityImplicit *Agent
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if !a.Implicit {
			continue
		}
		if a.Dir == "" {
			if cityImplicit != nil {
				t.Fatal("more than one city-scoped implicit agent injected")
			}
			cityImplicit = a
			continue
		}
		if a.Dir != "demo" {
			t.Fatalf("unexpected rig-scoped implicit agent %q", a.QualifiedName())
		}
		wantWorkDir := ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"
		if a.WorkDir != wantWorkDir {
			t.Errorf("rig implicit %q: WorkDir = %q, want %q", a.QualifiedName(), a.WorkDir, wantWorkDir)
		}
		wantPreStart := filepath.Join(coreDir, coreWorktreeScriptRel) + " {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"
		if len(a.PreStart) != 1 || a.PreStart[0] != wantPreStart {
			t.Errorf("rig implicit %q: PreStart = %v, want [%q]", a.QualifiedName(), a.PreStart, wantPreStart)
		}
	}
	if cityImplicit == nil {
		t.Fatal("no city-scoped implicit agent injected")
	}
	if cityImplicit.WorkDir != "" || len(cityImplicit.PreStart) != 0 {
		t.Fatalf("city-scoped implicit agent must stay unchanged in worktree mode: WorkDir=%q PreStart=%v", cityImplicit.WorkDir, cityImplicit.PreStart)
	}
	// Explicit agent untouched: no duplicate was injected and its fields
	// are exactly what the author wrote.
	for _, a := range cfg.Agents {
		if a.Implicit {
			continue
		}
		if a.Dir == "demo" && a.Name == "claude" {
			if a.WorkDir != "" || len(a.PreStart) != 0 {
				t.Fatalf("explicit agent mutated by worktree mode: WorkDir=%q PreStart=%v", a.WorkDir, a.PreStart)
			}
			return
		}
	}
	t.Fatal("explicit agent demo/claude lost after injection")
}

// TestInjectImplicitAgentsWorktreeModeWithoutCore verifies the injected
// pre_start still points at the core script path even when the core pack is
// not composed (empty PackDirs): rejection is the composed-root validation's
// job, and injection must not silently invent a different path.
func TestInjectImplicitAgentsWorktreeModeWithoutCore(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{ImplicitAgentIsolation: "worktree"},
		Providers: map[string]ProviderSpec{"claude": {}},
		Rigs:      []Rig{{Name: "demo", Path: "/tmp/demo"}},
	}
	InjectImplicitAgents(cfg)

	var rigImplicit *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Implicit && cfg.Agents[i].Dir == "demo" {
			rigImplicit = &cfg.Agents[i]
		}
	}
	if rigImplicit == nil {
		t.Fatal("no rig-scoped implicit agent injected")
	}
	wantPreStart := filepath.Join("", coreWorktreeScriptRel) + " {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"
	if len(rigImplicit.PreStart) != 1 || rigImplicit.PreStart[0] != wantPreStart {
		t.Fatalf("PreStart = %v, want [%q] even without the core pack composed", rigImplicit.PreStart, wantPreStart)
	}
	if _, err := os.Stat(strings.Fields(rigImplicit.PreStart[0])[0]); !os.IsNotExist(err) {
		t.Fatalf("expected the un-composed script path %q to not exist on disk", strings.Fields(rigImplicit.PreStart[0])[0])
	}
}

// TestInjectImplicitAgentsSharedDefaultIsByteCompatible pins #1181's
// compatibility floor: with the key absent or "shared", injected implicit
// agents stay byte-identical to the pre-key output — no work_dir, no
// pre_start — so the shared-cwd default (and the config fingerprint, which
// hashes pre_start but not work_dir) is unchanged.
func TestInjectImplicitAgentsSharedDefaultIsByteCompatible(t *testing.T) {
	for _, mode := range []string{"", "shared"} {
		cfg := &City{
			Workspace: Workspace{ImplicitAgentIsolation: mode},
			Providers: map[string]ProviderSpec{"claude": {}},
			Rigs:      []Rig{{Name: "demo", Path: "/tmp/demo"}},
			Agents: []Agent{{
				// An explicit agent with the same (dir, name) key suppresses
				// injection; it must stay untouched too.
				Name: "claude",
				Dir:  "demo",
			}},
		}
		InjectImplicitAgents(cfg)

		var cityImplicit, rigImplicit *Agent
		for i := range cfg.Agents {
			a := &cfg.Agents[i]
			if !a.Implicit {
				continue
			}
			if a.Dir == "" && cityImplicit == nil {
				cityImplicit = a
			}
			if a.Dir == "demo" && rigImplicit == nil {
				rigImplicit = a
			}
		}
		if cityImplicit == nil {
			t.Fatalf("mode %q: no city-scoped implicit agent injected", mode)
		}
		if rigImplicit != nil {
			t.Fatalf("mode %q: explicit agent for demo/claude was shadowed by an injected implicit agent", mode)
		}
		if cityImplicit.WorkDir != "" || len(cityImplicit.PreStart) != 0 {
			t.Fatalf("mode %q: city-scoped implicit agent mutated: WorkDir=%q PreStart=%v", mode, cityImplicit.WorkDir, cityImplicit.PreStart)
		}

		// A rig with no explicit agent still gets the plain implicit pool.
		cfg2 := &City{
			Workspace: Workspace{ImplicitAgentIsolation: mode},
			Providers: map[string]ProviderSpec{"claude": {}},
			Rigs:      []Rig{{Name: "demo", Path: "/tmp/demo"}},
		}
		InjectImplicitAgents(cfg2)
		var injected *Agent
		for i := range cfg2.Agents {
			if cfg2.Agents[i].Implicit && cfg2.Agents[i].Dir == "demo" {
				injected = &cfg2.Agents[i]
			}
		}
		if injected == nil {
			t.Fatalf("mode %q: no rig-scoped implicit agent injected", mode)
		}
		if injected.WorkDir != "" || len(injected.PreStart) != 0 {
			t.Fatalf("mode %q: rig-scoped implicit agent mutated: WorkDir=%q PreStart=%v", mode, injected.WorkDir, injected.PreStart)
		}
		if !strings.HasPrefix(injected.QualifiedName(), "demo/") {
			t.Fatalf("mode %q: injected agent identity = %q, want demo/<name>", mode, injected.QualifiedName())
		}
	}
}
