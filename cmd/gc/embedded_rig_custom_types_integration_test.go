//go:build integration

package main

// The embedded init door's type registration against bd itself. It is tagged
// because only a store bd created can show that a gc type is accepted
// afterwards; the stubbed proofs in storage_mode_rewrite_test.go pin the bd
// calls and the environment they run under.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestInitializingARealEmbeddedRigLetsItCreateGcTypedBeads is the claim the
// registration exists for: bd init leaves a fresh embedded store rejecting
// gc's types (convoy here), and after initAndHookDir the store accepts them
// while git sees no change to the rig's tracked .beads.
func TestInitializingARealEmbeddedRigLetsItCreateGcTypedBeads(t *testing.T) {
	bd := bazeltest.DataPath(t, "GC_TEST_PINNED_BD_BIN")
	if bd == "" {
		found, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
		if err != nil {
			t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
		}
		bd = found
	}
	configureTestDoltIdentityEnv(t)
	// The door runs bd by name; put the bd under test ahead of this package's
	// testscript shim.
	binDir := t.TempDir()
	if err := os.Symlink(bd, filepath.Join(binDir, "bd")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cityPath, rigPath, _ := legacyManagedCityWithEmbeddedRig(t, cleanEmbeddedRigConfig)
	recordingBdProviderScript(t, cityPath)
	captureStorageModeChanges(t)
	// Swap the fixture's stand-in store for one bd itself creates.
	if err := os.RemoveAll(filepath.Join(rigPath, ".beads")); err != nil {
		t.Fatal(err)
	}
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"BD_BIN":             bd,
		"BEADS_DIR":          filepath.Join(rigPath, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	must := func(name string, args ...string) string {
		t.Helper()
		out, err := run(rigPath, name, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	must("git", "init", "-q", ".")
	must("bd", "init", "-p", "fr", "--quiet", "--skip-agents", "--skip-hooks")
	if mode := readScopeDoltMode(t, rigPath); mode != "embedded" {
		t.Fatalf("bd init made a %q store, want embedded", mode)
	}
	must("git", "add", "-A", ".")
	must("git", "commit", "-q", "--allow-empty", "-m", "rig with bd's store")
	if out, err := run(rigPath, "bd", "create", "--json", "--type", "convoy", "before"); err == nil {
		t.Fatalf("precondition: a fresh embedded store already accepts gc's convoy type:\n%s", out)
	}

	if err := initAndHookDir(cityPath, rigPath, "fr"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}

	must("bd", "create", "--json", "--type", "convoy", "after")
	if status := must("git", "status", "--porcelain", "--", ".beads"); status != "" {
		t.Fatalf("initializing the embedded rig changed its git-tracked .beads:\n%s", status)
	}
}
