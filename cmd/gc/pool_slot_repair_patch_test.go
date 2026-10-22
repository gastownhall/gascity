package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestConcretePoolWorkDirRepairPatchRewritesPreUpgradeImplicitSlot is AC-8:
// a pre-upgrade slot session bead still pointing work_dir/gc.work_dir at the
// rig root — where every instance of the implicit pool used to run — gets
// both rewritten to the instance's slot worktree by the next start's repair
// patch.
func TestConcretePoolWorkDirRepairPatchRewritesPreUpgradeImplicitSlot(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir rig .git: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "app", Path: rigRoot}},
		Agents: []config.Agent{{
			Name: "claude", Dir: "app", Implicit: true, PoolSlotWorktrees: true,
		}},
	}
	slot := filepath.Join(cityPath, ".gc", "worktrees", "app", "claude-1")

	candidate := startCandidate{
		info: sessionpkg.Info{
			ID:               "s-1",
			Template:         "app/claude",
			PoolManaged:      true,
			WorkDir:          rigRoot,
			WorkDirCanonical: rigRoot,
		},
		tp: TemplateParams{TemplateName: "app/claude", InstanceName: "app/claude-1"},
	}

	patch := concretePoolWorkDirRepairPatch(candidate, cityPath, cfg)
	if len(patch) == 0 {
		t.Fatal("no repair patch for a pre-upgrade slot session pointing at the rig root")
	}
	if got := patch[beadmeta.WorkDirMetadataKey]; got != slot {
		t.Errorf("gc.work_dir = %q, want slot %q", got, slot)
	}
	if got := patch[beadmeta.LegacyWorkDirMetadataKey]; got != slot {
		t.Errorf("work_dir = %q, want slot %q", got, slot)
	}

	// A session already on its slot is left alone: the repair only fires for
	// rows still naming the template's directory.
	already := candidate
	already.info.WorkDir = slot
	already.info.WorkDirCanonical = slot
	if patch := concretePoolWorkDirRepairPatch(already, cityPath, cfg); len(patch) != 0 {
		t.Errorf("repaired session got patch %v, want none", patch)
	}
}
