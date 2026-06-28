package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestCrossStorePipeline_ReadThenClaim verifies the composition of the
// cross-store read half (firstStoreWithWork) and the write half
// (crossStoreClaimDir): a bead ID surfaced by federation from a rig store
// must be correctly redirected to that same rig store by the claim path.
//
// Uses the same injectable-runner approach as TestFirstStoreWithWork so no
// real Dolt or gc subprocess is needed.
func TestCrossStorePipeline_ReadThenClaim(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	rigName := "voxist-api"

	cfg := &config.City{
		Rigs: []config.Rig{{Name: rigName, Path: rigPath, Prefix: "va"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	// Step 1: READ — city store is empty; rig store has one ready bead.
	stores := []hookStore{
		{dir: cityPath, env: nil},
		{dir: rigPath, env: nil},
	}
	run := func(_, dir string, _ []string) (string, error) {
		if filepath.Clean(dir) == filepath.Clean(rigPath) {
			return `[{"id":"va-1","status":"open","issue_type":"task"}]`, nil
		}
		return `[]`, nil
	}

	out, gotStore, err := firstStoreWithWork("fake-query", stores, run)
	if err != nil {
		t.Fatalf("firstStoreWithWork: %v", err)
	}
	if filepath.Clean(gotStore.dir) != filepath.Clean(rigPath) {
		t.Fatalf("selected store = %q, want %q (rig store)", gotStore.dir, rigPath)
	}

	// Parse the bead ID from the output (same parse the agent/host would do).
	var beads []struct {
		ID string `json:"id"`
	}
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &beads); jsonErr != nil || len(beads) == 0 {
		t.Fatalf("could not parse bead from output %q: %v", out, jsonErr)
	}
	beadID := beads[0].ID
	if beadID != "va-1" {
		t.Fatalf("parsed bead ID = %q, want va-1", beadID)
	}

	// Step 2: CLAIM — the bead ID from the read output must redirect to the rig store.
	claimDir, ok := crossStoreClaimDir(cfg, cityAgent, beadID)
	if !ok {
		t.Fatalf("crossStoreClaimDir(%q): no redirect — city-scoped agent claiming a rig-store bead must redirect", beadID)
	}
	if filepath.Clean(claimDir) != filepath.Clean(rigPath) {
		t.Fatalf("claim dir = %q, want %q (rig store)", claimDir, rigPath)
	}
}

// TestCrossStorePipeline_CityBeadNoRedirect confirms that city-owned work
// (HQ store bead, unrecognized prefix) does not get redirected — the claim
// stays in the agent's own store.
func TestCrossStorePipeline_CityBeadNoRedirect(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-api", Path: t.TempDir(), Prefix: "va"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	// An HQ/city-owned bead (vc-* or unknown prefix) must not redirect.
	if _, ok := crossStoreClaimDir(cfg, cityAgent, "vc-123"); ok {
		t.Fatal("city-owned (vc-*) bead must not redirect to any rig store")
	}
}

// TestCrossStorePipeline_RigAgentByteForByteUnchanged confirms the rig-agent
// isolation guarantee: a rig-scoped agent is NEVER redirected regardless of
// bead prefix.
func TestCrossStorePipeline_RigAgentByteForByteUnchanged(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-api", Path: t.TempDir(), Prefix: "va"}},
	}
	rigAgent := &config.Agent{Name: "executor", Dir: "voxist-api"}

	if _, ok := crossStoreClaimDir(cfg, rigAgent, "va-1"); ok {
		t.Fatal("rig-scoped agent must be byte-for-byte unchanged (no claim redirect)")
	}
}
