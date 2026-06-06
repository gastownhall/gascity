package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCrossStoreClaimDirRedirectsCityAgentToBeadOwningRig(t *testing.T) {
	// vp-kvp stage iii (write half): a city-scoped singleton discovers rig-store
	// work through the federated gc-hook read, so its bd update --claim must land
	// in that rig store rather than its own (city) store.
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	dir, ok := crossStoreClaimDir(cfg, cityAgent, "vw-123")
	if !ok {
		t.Fatal("city-scoped agent claiming a rig-store bead must redirect to the rig store")
	}
	if dir != rigPath {
		t.Fatalf("redirect dir = %q, want %q", dir, rigPath)
	}
}

func TestCrossStoreClaimDirLeavesRigAgentUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	rigAgent := &config.Agent{Name: "reviewer", Dir: "voxist-web"}

	if _, ok := crossStoreClaimDir(cfg, rigAgent, "vw-123"); ok {
		t.Fatal("rig-scoped agent must be byte-for-byte unchanged (no claim redirect)")
	}
}

func TestCrossStoreClaimDirNoRedirectForCityOwnedBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	// A bead whose prefix does not resolve to a configured rig (HQ/city-owned)
	// stays in the agent's own store: no redirect.
	if _, ok := crossStoreClaimDir(cfg, cityAgent, "hq-9"); ok {
		t.Fatal("city-owned bead must not redirect")
	}
}

func TestRunBDForBeadRedirectsCrossStoreWrite(t *testing.T) {
	var inStoreDir string
	var inStoreArgs []string
	var fellBack bool
	exec := agentScriptExecutor{
		runCommand: func(_ string, _ ...string) error {
			fellBack = true
			return nil
		},
		runCommandInStore: func(dir string, _ []string, name string, args ...string) error {
			inStoreDir = dir
			inStoreArgs = append([]string{name}, args...)
			return nil
		},
		resolveBeadStore: func(beadID string) (string, []string, bool) {
			if beadID == "vw-1" {
				return "/rigs/voxist-web", []string{"BEADS_DIR=/rigs/voxist-web/.beads"}, true
			}
			return "", nil, false
		},
	}

	if err := exec.runBDForBead("vw-1", "update", "vw-1", "--claim"); err != nil {
		t.Fatalf("runBDForBead cross-store: %v", err)
	}
	if fellBack {
		t.Fatal("cross-store bead must route through runCommandInStore, not the default runCommand")
	}
	if inStoreDir != "/rigs/voxist-web" {
		t.Fatalf("in-store dir = %q, want /rigs/voxist-web", inStoreDir)
	}
	if len(inStoreArgs) == 0 || inStoreArgs[0] != "bd" {
		t.Fatalf("in-store args = %v, want bd ...", inStoreArgs)
	}

	// A bead the resolver does not redirect falls back to the default runner.
	fellBack = false
	if err := exec.runBDForBead("hq-9", "update", "hq-9", "--claim"); err != nil {
		t.Fatalf("runBDForBead fallback: %v", err)
	}
	if !fellBack {
		t.Fatal("non-redirected bead must fall back to the default runCommand")
	}
}

func TestCrossStoreClaimDirNilCfgOrAgent(t *testing.T) {
	rigAgent := &config.Agent{Name: "reviewer", Dir: "voxist-web"}
	if _, ok := crossStoreClaimDir(nil, rigAgent, "vw-1"); ok {
		t.Fatal("nil cfg must not redirect")
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web", Path: "/x", Prefix: "vw"}}}
	if _, ok := crossStoreClaimDir(cfg, nil, "vw-1"); ok {
		t.Fatal("nil agent must not redirect")
	}
}
