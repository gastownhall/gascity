//go:build integration

package integration

// TestCleanInstallTutorialPath is a regression test for GitHub issue #1670:
// "Tutorial 01: gc sling fails on clean install — issue_prefix not seeded into
// Dolt DB by gc init / gc rig add."
//
// After gc init + gc rig add the rig's Dolt database must be seeded with
// issue_prefix so that `bd config get issue_prefix` returns the derived prefix
// and rig-scoped bead creation works without a "database not initialized" error.
//
// This test passes against current main (post-#1477) and guards against future regressions of #1670.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// bdDoltInRig runs the bd binary in rigDir using the managed Dolt endpoint
// from cityDir. The rig and the city share the same Dolt server; the database
// name is read from the rig's .beads/metadata.json by bd.
func bdDoltInRig(cityDir, rigDir string, args ...string) (string, error) {
	env := commandEnvForDir(cityDir, true)
	if port, ok := ensureManagedDoltPortForTest(cityDir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
	}
	return runCommand(rigDir, env, integrationBDCommandTimeout, bdBinary, args...)
}

func TestCleanInstallTutorialPath(t *testing.T) {
	requireDoltIntegration(t)

	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	registerIntegrationDoltSQLServerCleanup(t, cityDir)

	// --- Step 1: gc init (clean install, no --file) ---
	out, err := runGCDoltWithEnv(env, "", "init",
		"--skip-provider-readiness",
		"--providers", "codex",
		"--default-provider", "codex",
		cityDir,
	)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	// --- Step 2: gc rig add (simulate `gc rig add ~/tutorial-rig-alpha`) ---
	// Three-part name so DeriveBeadsPrefix produces a 3-char prefix ("tra") that
	// can never equal the 2-char prefix derived from the random city name.
	rigName := "tutorial-rig-alpha"
	rigDir := filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	out, err = gcDolt(cityDir, "rig", "add", rigDir)
	if err != nil {
		t.Fatalf("gc rig add failed: %v\noutput: %s", err, out)
	}

	wantPrefix := config.DeriveBeadsPrefix(rigName) // "tra"

	// --- Assertion 1: bd config get issue_prefix returns the derived prefix ---
	// Regression: rig Dolt DB was never seeded so this returned "" before the fix.
	prefixOut, err := bdDoltInRig(cityDir, rigDir, "config", "get", "issue_prefix")
	if err != nil {
		t.Fatalf("bd config get issue_prefix in rig failed: %v\noutput: %s\n(regression: issue #1670 — rig Dolt DB not seeded during gc rig add)", err, prefixOut)
	}
	gotPrefix := strings.TrimSpace(prefixOut)
	if gotPrefix != wantPrefix {
		t.Errorf("bd config get issue_prefix = %q, want %q\n(regression: issue #1670 — rig Dolt DB not seeded during gc rig add)", gotPrefix, wantPrefix)
	}

	// --- Assertion 2: rig-scoped bead creation succeeds ---
	// If the DB is not initialized, bd create returns a "database not initialized"
	// (or equivalent) error instead of a bead ID.
	beadTitle := fmt.Sprintf("tutorial regression test bead (%s)", wantPrefix)
	beadOut, err := bdDoltInRig(cityDir, rigDir, "create", beadTitle)
	if err != nil {
		if strings.Contains(strings.ToLower(beadOut+err.Error()), "not initialized") {
			t.Fatalf("bd create in rig failed with database-not-initialized (regression issue #1670): %v\noutput: %s", err, beadOut)
		}
		t.Fatalf("bd create in rig failed: %v\noutput: %s", err, beadOut)
	}
	// Bead ID should carry the rig prefix, e.g. "mp-abc".
	if !strings.Contains(beadOut, wantPrefix+"-") {
		t.Errorf("bd create output = %q, want bead ID containing prefix %q", strings.TrimSpace(beadOut), wantPrefix+"-")
	}
}
