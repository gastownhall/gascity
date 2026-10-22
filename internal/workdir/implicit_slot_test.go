package workdir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// implicitSlotTestCity builds a city with one rig "app" (git-backed unless
// withGit is false) and runs InjectImplicitAgents over a configured provider
// catalog, returning the city path, rigs, and the injected agents.
func implicitSlotTestCity(t *testing.T, withGit bool) (string, []config.Rig, []config.Agent) {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	if withGit {
		if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir rig .git: %v", err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city", Provider: "claude"},
		Providers: map[string]config.ProviderSpec{"claude": config.BuiltinProviderAlias("claude")},
		Rigs:      []config.Rig{{Name: "app", Path: rigRoot}},
	}
	config.InjectImplicitAgents(cfg)
	return cityPath, cfg.Rigs, cfg.Agents
}

// implicitSlotTestAgent returns the injected claude agent scoped to dir.
func implicitSlotTestAgent(t *testing.T, agents []config.Agent, dir string) config.Agent {
	t.Helper()
	for _, a := range agents {
		if a.Dir == dir && a.Name == "claude" {
			return a
		}
	}
	t.Fatalf("claude agent scoped to %q not found among %d injected agents", dir, len(agents))
	return config.Agent{}
}

// TestImplicitRigPoolInstancesGetDistinctWorkDirs pins the defect fix itself:
// numbered pool instances of a rig-scoped implicit agent must each resolve to
// their own slot worktree under <city>/.gc/worktrees/<rig>/, while the
// canonical template identity keeps resolving to the rig root.
func TestImplicitRigPoolInstancesGetDistinctWorkDirs(t *testing.T) {
	cityPath, rigs, agents := implicitSlotTestCity(t, true)
	a := implicitSlotTestAgent(t, agents, "app")

	if !a.SupportsMultipleSessions() {
		t.Fatal("rig-scoped implicit agent must support multiple concurrent sessions")
	}

	first, err := ResolveWorkDirPathStrict(cityPath, "city", "app/claude-1", a, rigs)
	if err != nil {
		t.Fatalf("resolving app/claude-1: %v", err)
	}
	second, err := ResolveWorkDirPathStrict(cityPath, "city", "app/claude-2", a, rigs)
	if err != nil {
		t.Fatalf("resolving app/claude-2: %v", err)
	}
	if first == second {
		t.Fatalf("pool instances app/claude-1 and app/claude-2 share one working directory %q", first)
	}
	wantFirst := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")
	wantSecond := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-2")
	if first != wantFirst {
		t.Errorf("app/claude-1 work dir = %q, want %q", first, wantFirst)
	}
	if second != wantSecond {
		t.Errorf("app/claude-2 work dir = %q, want %q", second, wantSecond)
	}

	// The canonical template identity — the name without an instance
	// suffix — keeps today's behavior and resolves to the rig root.
	canonical, err := ResolveWorkDirPathStrict(cityPath, "city", "app/claude", a, rigs)
	if err != nil {
		t.Fatalf("resolving app/claude: %v", err)
	}
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if canonical != rigRoot {
		t.Errorf("app/claude work dir = %q, want rig root %q", canonical, rigRoot)
	}
}

// TestImplicitSlotForMatchRules walks the matcher table: only a marked
// rig-scoped implicit agent resolving a numbered instance of its own template
// (or of its pool name, for copies) over a git-backed rig qualifies for a
// slot worktree. Everything else — canonical names, ad-hoc sessions, MCP alt
// identities, malformed numbers, foreign rigs, unmarked or overridden
// agents, non-git rigs — does not.
func TestImplicitSlotForMatchRules(t *testing.T) {
	cityPath, rigs, agents := implicitSlotTestCity(t, true)
	a := implicitSlotTestAgent(t, agents, "app")
	cityAgent := implicitSlotTestAgent(t, agents, "")

	slot1 := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")
	slot2 := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-2")
	slot12 := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-12")

	// A pool copy carries the template identity in PoolName and a concrete
	// instance name; the slot match keys off the PoolName base.
	poolCopy := a
	poolCopy.Name = "claude-1"
	poolCopy.PoolName = a.QualifiedName()

	// An explicit work_dir is an agent-level opt-out.
	withWorkDir := a
	withWorkDir.WorkDir = "{{.CityRoot}}/.gc/agents/claude"

	// An unmarked agent resolves exactly as today.
	unmarked := a
	unmarked.PoolSlotWorktrees = false

	tests := []struct {
		name        string
		qualified   string
		agent       config.Agent
		wantSlot    string
		wantRigRoot string
	}{
		{name: "first instance", qualified: "app/claude-1", agent: a, wantSlot: slot1, wantRigRoot: filepath.Join(cityPath, "rigs", "app")},
		{name: "second instance", qualified: "app/claude-2", agent: a, wantSlot: slot2, wantRigRoot: filepath.Join(cityPath, "rigs", "app")},
		{name: "double-digit instance", qualified: "app/claude-12", agent: a, wantSlot: slot12, wantRigRoot: filepath.Join(cityPath, "rigs", "app")},
		{name: "pool copy resolves via pool name base", qualified: "app/claude-1", agent: poolCopy, wantSlot: slot1, wantRigRoot: filepath.Join(cityPath, "rigs", "app")},
		{name: "canonical template identity", qualified: "app/claude", agent: a},
		{name: "instance zero", qualified: "app/claude-0", agent: a},
		{name: "zero-padded instance", qualified: "app/claude-01", agent: a},
		{name: "non-numeric suffix", qualified: "app/claude-1x", agent: a},
		{name: "suffix starting with digit then junk", qualified: "app/claude-1-alt", agent: a},
		{name: "ad-hoc session identity", qualified: "app/claude-adhoc-s-1", agent: a},
		{name: "foreign rig prefix", qualified: "other/claude-1", agent: a},
		{name: "city-scoped implicit agent", qualified: "claude-1", agent: cityAgent},
		{name: "unmarked agent", qualified: "app/claude-1", agent: unmarked},
		{name: "explicit work_dir opt-out", qualified: "app/claude-1", agent: withWorkDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot, ok := ImplicitSlotFor(cityPath, tt.qualified, tt.agent, rigs)
			if tt.wantSlot == "" {
				if ok {
					t.Fatalf("ImplicitSlotFor(%q) = %+v, want no match", tt.qualified, slot)
				}
				return
			}
			if !ok {
				t.Fatalf("ImplicitSlotFor(%q) did not match, want slot %q", tt.qualified, tt.wantSlot)
			}
			if slot.Path != tt.wantSlot {
				t.Errorf("slot path = %q, want %q", slot.Path, tt.wantSlot)
			}
			if slot.RigRoot != tt.wantRigRoot {
				t.Errorf("slot rig root = %q, want %q", slot.RigRoot, tt.wantRigRoot)
			}
		})
	}
}

// TestImplicitSlotForRequiresGitRig pins the Lstat(<rigRoot>/.git) gate: a
// rig without .git gets no slot worktrees (today's rig-root sharing is
// unchanged for it), while a linked-worktree rig root — whose .git is a
// regular pointer FILE — does qualify, proving the check is a plain Lstat,
// not a git subprocess.
func TestImplicitSlotForRequiresGitRig(t *testing.T) {
	cityPath, rigs, agents := implicitSlotTestCity(t, false)
	a := implicitSlotTestAgent(t, agents, "app")

	if slot, ok := ImplicitSlotFor(cityPath, "app/claude-1", a, rigs); ok {
		t.Fatalf("non-git rig matched slot %q; want no match", slot.Path)
	}

	// A linked-worktree rig root carries a regular .git pointer file.
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.WriteFile(filepath.Join(rigRoot, ".git"), []byte("gitdir: /tmp/somewhere.git/worktrees/app\n"), 0o644); err != nil {
		t.Fatalf("writing .git pointer: %v", err)
	}
	slot, ok := ImplicitSlotFor(cityPath, "app/claude-1", a, rigs)
	if !ok {
		t.Fatal("linked-worktree rig root (regular .git file) must match")
	}
	if want := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1"); slot.Path != want {
		t.Errorf("slot path = %q, want %q", slot.Path, want)
	}
}

// TestImplicitSlotForResolvesRigRootAgainstCity pins that a relative rig path
// resolves against the city root before the .git Lstat, so inline configs
// pointing rigs at relative paths behave like absolute ones.
func TestImplicitSlotForResolvesRigRootAgainstCity(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir rig .git: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city", Provider: "claude"},
		Providers: map[string]config.ProviderSpec{"claude": config.BuiltinProviderAlias("claude")},
		Rigs:      []config.Rig{{Name: "app", Path: "rigs/app"}},
	}
	config.InjectImplicitAgents(cfg)
	a := implicitSlotTestAgent(t, cfg.Agents, "app")

	slot, ok := ImplicitSlotFor(cityPath, "app/claude-1", a, cfg.Rigs)
	if !ok {
		t.Fatal("relative rig path with .git must match")
	}
	if want := filepath.Join(cityPath, "rigs", "app"); slot.RigRoot != want {
		t.Errorf("rig root = %q, want %q", slot.RigRoot, want)
	}
	if want := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1"); slot.Path != want {
		t.Errorf("slot path = %q, want %q", slot.Path, want)
	}
}
