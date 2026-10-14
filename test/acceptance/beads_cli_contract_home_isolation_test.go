//go:build acceptance_bd_contract

// HOME-isolation regression coverage for runBD. Kept in its own file rather
// than beads_cli_contract_test.go so that file's test list stays exactly
// the focused external contract manifest pinned by
// TestAcceptanceTargetsSeparateTierAFromExternalBdContracts
// (scripts/ci_critical_path_test.go). Same build tag and package as
// beads_cli_contract_test.go, so this file shares runBD/requireBD/createBead
// directly instead of duplicating them.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestRunBDIsolatesHOMEFromSharedServerConfig proves runBD re-homes bd
// (helpers.ToolCommand gives it a test-owned HOME), so a shared-server
// config.yaml sitting in the ambient real HOME (ga-1037rg; first surfaced on
// a gate host as ga-yoxtux, where it took down the whole bd CLI contract at
// 1 PASS / 36 FAIL) cannot make bd route the contract's own commands through
// that shared server instead of dir's own BEADS_DIR-scoped store. Run by
// test-bd-cli-contract-home-
// isolation, a separate Makefile target from test-bd-cli-contract itself,
// wherever CI installs a bd version and runs the focused contract.
func TestRunBDIsolatesHOMEFromSharedServerConfig(t *testing.T) {
	helpers.RequireBD(t)

	pollutedHome := t.TempDir()
	beadsDir := filepath.Join(pollutedHome, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating polluted HOME .beads dir: %v", err)
	}
	cfg := "no-db: true\ndolt:\n    shared-server: true\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing polluted HOME config.yaml: %v", err)
	}
	t.Setenv("HOME", pollutedHome)

	dir := beadstest.GuardedTempDir(t)
	requireBD(t, dir, "init", "-p", "ct", "--skip-hooks", "-q")
	id := createBead(t, dir, "home-isolation probe")

	out := requireBD(t, dir, "list", "--json")
	if !strings.Contains(out, id) {
		t.Fatalf("bd list under a shared-server HOME did not see bead %s created in dir's own BEADS_DIR-scoped store:\n%s", id, out)
	}
}
