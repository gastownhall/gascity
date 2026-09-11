package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGCNonTestFilesStayOnRigProvisionBoundary enforces that rig-add
// provisioning has exactly one orchestration path: internal/rig.Provision,
// reached only from the two sanctioned delegates (the CLI wrapper in cmd_rig.go
// and the controller's StateMutator in api_state.go). It mirrors
// TestGCNonTestFilesStayOnWorkerBoundary, extended with a per-needle allowlist
// because this boundary has sanctioned call sites the worker boundary lacks.
//
// The forbidden needles guard the primitives Decision 7 consolidated: the rig
// config writer lives only inside internal/rig, the retired parallel writer
// (configedit.Editor.CreateRig) must not be re-called, and the two deleted
// controller helpers (initializeRigStoreForCreate, detectRigDefaultBranch) must
// not be reconstructed. The site-binding writer
// config.WriteCityAndRigSiteBindingsForEdit is a shared leaf with legitimate
// non-rig-add callers (gc agent, gc init, gc doctor), so it is guarded with a
// per-needle allowlist rather than banned outright: a NEW rig-add caller must
// not reach for the raw writer instead of internal/rig.Provision, while the
// sanctioned config editors keep using it. Only the raw config.* call is guarded
// (not the writeCityConfigForEditFS wrapper, whose sole definition lives in the
// allowlisted cmd_agent.go). Other leaf helpers shared with gc init and the
// controller lifecycle (initDirIfReady, normalizeCanonicalBdScopeFiles) stay
// unguarded: the boundary is the orchestration, not those leaves.
func TestGCNonTestFilesStayOnRigProvisionBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	// allowed maps a needle to the base filenames permitted to contain it.
	allowed := map[string]map[string]bool{
		"rig.Provision(":   {"cmd_rig.go": true, "api_state.go": true},
		"rig.StatRigPath(": {"cmd_rig.go": true}, // ordering-only CLI preflight; Provision re-runs it.
		// Sanctioned non-rig-add callers of the shared site-binding writer. The
		// rig-add delegates (cmd_rig.go, api_state.go) are deliberately absent:
		// they must route through internal/rig.Provision, not the raw writer.
		"config.WriteCityAndRigSiteBindingsForEdit(": {
			"cmd_agent.go":        true, // wrapper writeCityConfigForEditFS + suspend/resume edits.
			"cmd_init.go":         true, // city bootstrap.
			"doctor_v2_checks.go": true, // doctor --fix binding repair.
		},
	}
	forbidden := []string{
		"rig.Provision(",   // only the two delegates orchestrate a rig add.
		"rig.StatRigPath(", // preflight belongs to the CLI wrapper alone.
		"config.AppendRigAndWriteSiteBindingsForEdit(", // the rig-add config writer lives only in internal/rig.
		"config.WriteCityAndRigSiteBindingsForEdit(",   // the raw site-binding writer: rig-add must not call it directly.
		"editor.CreateRig(",                            // the retired parallel writer.
		"initializeRigStoreForCreate",                  // resurrection guards for the two deleted controller helpers.
		"detectRigDefaultBranch(",
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbidden {
			if !strings.Contains(content, needle) {
				continue
			}
			if allowed[needle][name] {
				continue
			}
			t.Fatalf("%s calls rig-provisioning primitive %q outside the sanctioned boundary", path, needle)
		}
	}
}
