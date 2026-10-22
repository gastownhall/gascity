package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

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
// define it leaves the root's value alone.
func TestImplicitAgentIsolationFragmentOverride(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
implicit_agent_isolation = "shared"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[workspace]
implicit_agent_isolation = "worktree"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Workspace.ImplicitAgentIsolation; got != "worktree" {
		t.Fatalf("composed implicit_agent_isolation = %q, want the fragment's worktree", got)
	}

	fs2 := fsys.NewFake()
	fs2.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
implicit_agent_isolation = "worktree"
`)
	fs2.Files["/city/fragment.toml"] = []byte(`
[workspace]
timezone = "UTC"
`)
	cfg2, _, err := LoadWithIncludes(fs2, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg2.Workspace.ImplicitAgentIsolation; got != "worktree" {
		t.Fatalf("composed implicit_agent_isolation = %q, want the root's worktree to survive an unrelated fragment", got)
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
