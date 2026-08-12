package dolt_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/workdir"
)

// TestDogAgentWorkDirDoesNotResolveToCityRoot is a regression guard for
// gascity#4077: the bundled dog agent is scope=city with no configured
// rig, so an omitted work_dir/dir falls back to workdir.ResolveDirPath's
// dir=="" case, which returns the city root itself. Every per-run artifact
// the dog session creates relative to its cwd then leaks into the
// operator's city root instead of a scratch/worktree location.
func TestDogAgentWorkDirDoesNotResolveToCityRoot(t *testing.T) {
	agents, err := config.DiscoverPackAgents(fsys.OSFS{}, repoRoot(t), "dolt", nil)
	if err != nil {
		t.Fatalf("DiscoverPackAgents() error = %v", err)
	}

	var dog *config.Agent
	for i := range agents {
		if agents[i].Name == "dog" {
			dog = &agents[i]
		}
	}
	if dog == nil {
		t.Fatalf("dog agent not found under %s/agents", repoRoot(t))
	}
	if dog.Scope != "city" {
		t.Fatalf("dog agent scope = %q, want %q (test assumes the city-scoped, rig-less case)", dog.Scope, "city")
	}

	cityPath := t.TempDir()
	got := workdir.ResolveWorkDirPath(cityPath, "city", "dog", *dog, nil)
	if got == cityPath {
		t.Fatalf("dog agent work_dir resolved to the city root %q; want a scratch subdirectory", cityPath)
	}
}
