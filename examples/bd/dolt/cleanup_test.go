// cleanup_test.go pins the dry-run and --force behavior of
// commands/cleanup/run.sh when the rig registry query fails. See ga-a5mi0k:
// metadata_files() and compute_allowlist_file() both call
// `gc rig list --json`, but only the allowlist call is fail-closed —
// metadata_files() degrades to a local-only filesystem scan that cannot see
// external rigs, so a failed registry query previously produced a dry-run
// row with no annotation at all for a live external rig's database,
// indistinguishable from a confirmed orphan.
package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleanupScript is the on-disk path to the cleanup command script.
const cleanupScript = "commands/cleanup/run.sh"

// writeFailingGCStub writes a `gc` stub to binDir that fails every
// invocation, simulating `gc rig list --json` erroring out. Cleanup's
// run.sh only ever shells out to `gc rig list --json` (in metadata_files()
// and compute_allowlist_file()), so an unconditional failure is sufficient
// to exercise the "registry query failed" path without branching on args.
func writeFailingGCStub(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 1\n")
}

// TestCleanupDryRunAnnotatesUnverifiedOnRegistryFailure pins exit_contract
// item 4 on ga-a5mi0k: when `gc rig list --json` fails, dry-run must mark
// every row "unverified" instead of leaving the STATUS column blank. Today,
// metadata_files() silently falls back to a local find() scan rooted at
// $GC_CITY_PATH/rigs, which cannot discover an external rig's database (one
// registered outside the city path, like the HQ-colocated live stores
// gascity, my_db, mcdclient, etc.) — so its row prints with no annotation,
// indistinguishable from a confirmed orphan.
func TestCleanupDryRunAnnotatesUnverifiedOnRegistryFailure(t *testing.T) {
	cityPath := t.TempDir()

	// dataDir holds one "orphan": a live external rig's database. Its rig
	// registration would only be visible via `gc rig list --json` — the
	// local-only find() fallback is rooted at $GC_CITY_PATH/rigs and
	// structurally cannot see it, mirroring the HQ-colocated case where the
	// shared Dolt data dir shares no path prefix with the rig's registered
	// path.
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "extdb", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir extdb: %v", err)
	}

	binDir := t.TempDir()
	writeFailingGCStub(t, binDir)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, cleanupScript))
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=3306",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cleanup dry-run failed: %v\n%s", err, out)
	}

	var extdbLine string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "extdb") {
			extdbLine = line
			break
		}
	}
	if extdbLine == "" {
		t.Fatalf("extdb not listed as an orphan in dry-run output:\n%s", out)
	}
	if !strings.Contains(extdbLine, "unverified") {
		t.Fatalf("extdb row does not carry an \"unverified\" status when the rig registry query failed — "+
			"an operator cannot distinguish this live external rig's database from a confirmed orphan:\n%s", out)
	}
}

// TestCleanupForceRefusesOnRegistryFailure characterizes exit_contract item
// 3 on ga-a5mi0k, which was already correct before this bead's fix:
// compute_allowlist_file() aborts --force before any rm -rf / DROP DATABASE
// when the registry query fails. This guards against a regression while
// item 4's dry-run annotation fix lands alongside it.
func TestCleanupForceRefusesOnRegistryFailure(t *testing.T) {
	cityPath := t.TempDir()

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "extdb")
	if err := os.MkdirAll(filepath.Join(dbPath, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir extdb: %v", err)
	}

	binDir := t.TempDir()
	writeFailingGCStub(t, binDir)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, cleanupScript), "--force")
	cmd.Env = append(filteredEnv("PATH"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=3306",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("cleanup --force succeeded despite a failed rig registry query; it must abort before removing anything:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to run overlap allowlist unverified") {
		t.Fatalf("cleanup --force failed, but not for the expected reason (registry query unverified):\n%s", out)
	}

	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("extdb was removed despite the registry query failing: %v", statErr)
	}
}
