package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// implicitSlotReadonlyCity builds a city whose rig "app" is a real git
// repository and whose configured claude provider yields a marked
// rig-scoped implicit agent (config.InjectImplicitAgents with the default
// worktree isolation mode), plus a city-level MCP catalog every agent
// inherits. None of the slot's instances exist yet — exactly the shape the
// read-only consumers face between pool sessions.
func implicitSlotReadonlyCity(t *testing.T) (string, *config.City, *config.Agent) {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	implicitSlotGitOut(t, rigRoot, "init", "-b", "main")
	implicitSlotGitOut(t, rigRoot, "config", "user.email", "test@example.com")
	implicitSlotGitOut(t, rigRoot, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(rigRoot, "hello.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing rig file: %v", err)
	}
	implicitSlotGitOut(t, rigRoot, "add", "hello.txt")
	implicitSlotGitOut(t, rigRoot, "commit", "-m", "init")

	if err := os.MkdirAll(filepath.Join(cityPath, "mcp"), 0o755); err != nil {
		t.Fatalf("mkdir mcp catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "mcp", "notes.toml"), []byte("name = \"notes\"\ncommand = \"npx\"\n"), 0o644); err != nil {
		t.Fatalf("writing mcp catalog: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{"claude": config.BuiltinProviderAlias("claude")},
		Rigs:      []config.Rig{{Name: "app", Path: rigRoot}},
	}
	config.InjectImplicitAgents(cfg)
	var agent *config.Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Dir == "app" && cfg.Agents[i].Name == "claude" {
			agent = &cfg.Agents[i]
		}
	}
	if agent == nil {
		t.Fatal("no marked implicit app/claude agent after InjectImplicitAgents")
	}
	if !agent.PoolSlotWorktrees {
		t.Fatal("implicit app/claude agent is not marked for slot worktrees")
	}
	// The agent's own mcp_dir makes the catalog load for this agent alone,
	// independent of the pack-import sources a composed city would carry.
	agent.MCPDir = filepath.Join(cityPath, "mcp")
	return cityPath, cfg, agent
}

// slotWorktreePath is the slot checkout the marked implicit agent's
// instance claude-1 runs in.
func slotWorktreePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")
}

// assertNoSlotProvisioned fails the test when the given read-only consumer
// went beyond resolving a slot path and actually created it.
func assertNoSlotProvisioned(t *testing.T, slot string) {
	t.Helper()
	if _, err := os.Stat(slot); !os.IsNotExist(err) {
		t.Errorf("the read-only path provisioned slot worktree %s: %v", slot, err)
	}
}

// TestSessionLogsContextDoesNotProvisionASlotWorktree is AC-6: `gc session
// logs` resolves where a session's logs live — it must not create the
// session's working tree to find them. A named session bound to a marked
// pool agent's instance resolves the slot path and leaves it unprovisioned.
func TestSessionLogsContextDoesNotProvisionASlotWorktree(t *testing.T) {
	cityPath, cfg, _ := implicitSlotReadonlyCity(t)
	cfg.NamedSessions = []config.NamedSession{{Name: "claude-1", Template: "claude", Dir: "app"}}
	slot := slotWorktreePath(cityPath)

	got, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "app/claude-1")
	if !ok {
		t.Fatal("resolveConfiguredSessionLogContext() = not found, want the named session's slot")
	}
	if got != slot {
		t.Fatalf("resolveConfiguredSessionLogContext() workDir = %q, want slot %q", got, slot)
	}
	assertNoSlotProvisioned(t, slot)
}

// TestReadOnlyTemplateResolutionDoesNotProvisionASlotWorktree is AC-6: the
// read-only desired-state build (the allocator's named-session create
// decision) resolves an instance's template — its slot path — without
// creating the worktree; the create step itself provisions when the plan is
// accepted.
func TestReadOnlyTemplateResolutionDoesNotProvisionASlotWorktree(t *testing.T) {
	cityPath, cfg, agent := implicitSlotReadonlyCity(t)
	slot := slotWorktreePath(cityPath)

	bp := newReadOnlyAgentBuildParams("test-city", cityPath, cfg, nil, time.Now().UTC(), io.Discard)
	tp, err := resolveTemplate(bp, agent, "app/claude-1", nil)
	if err != nil {
		t.Fatalf("read-only resolveTemplate: %v", err)
	}
	if tp.WorkDir != slot {
		t.Fatalf("read-only resolveTemplate WorkDir = %q, want slot %q", tp.WorkDir, slot)
	}
	assertNoSlotProvisioned(t, slot)
}

// TestMCPSessionProjectionDoesNotProvisionASlotWorktree is AC-6: projecting
// a session's MCP surface resolves the session's work dir — for a slot
// instance that must be a pure resolution, never the provisioning path,
// or `gc mcp list --session` would build a git worktree as a side effect.
func TestMCPSessionProjectionDoesNotProvisionASlotWorktree(t *testing.T) {
	cityPath, cfg, _ := implicitSlotReadonlyCity(t)
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "claude-1 session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":      "app/claude",
			"session_name":  "s-claude-1",
			"agent_name":    "app/claude-1",
			"provider":      "claude",
			"provider_kind": "claude",
			"state":         "asleep",
		},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}

	slot := slotWorktreePath(cityPath)
	_, err = resolveSessionMCPProjection(cityPath, cfg, sessionFrontDoor(store), bead.ID, nil)
	if err != nil {
		t.Fatalf("resolveSessionMCPProjection: %v", err)
	}
	assertNoSlotProvisioned(t, slot)
}

// TestAgentLevelMCPRequiresSessionForSlotIsolatedPool is AC-6: a marked
// pool agent's sessions run in per-instance worktrees, so its MCP
// projection is genuinely session-specific — the agent-level view must
// refuse with the same "use --session" error an explicit multi-session pool
// gets, and the refusal itself must create nothing.
func TestAgentLevelMCPRequiresSessionForSlotIsolatedPool(t *testing.T) {
	cityPath, cfg, agent := implicitSlotReadonlyCity(t)

	_, err := resolveDeterministicAgentMCPProjection(cityPath, cfg, agent, exec.LookPath)
	if err == nil || !strings.Contains(err.Error(), "use --session") {
		t.Fatalf("agent-level projection err = %v, want the use --session refusal", err)
	}
	assertNoSlotProvisioned(t, slotWorktreePath(cityPath))
}
