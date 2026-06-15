package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcBeadsBdEnsureTypesCustomInYaml_MergesWithExistingValues pins the
// gascity-side #2154 fix and the PR #2315 review followup: when the
// existing types.custom line is a different set than the baseline being
// installed, the function must MERGE the two sets (preserving existing
// entries that may be pack/user-defined custom types) rather than overwrite.
// The required baseline types must end up present after the call; the
// existing entries must also remain.
func TestGcBeadsBdEnsureTypesCustomInYaml_MergesWithExistingValues(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yamlPath := filepath.Join(cityDir, ".beads", "config.yaml")
	// Existing values represent extensions the operator/pack added beyond
	// the SDK baseline — they must be preserved through the merge.
	initial := "issue_prefix: gc\ntypes.custom: legacy_a,legacy_b,legacy_c\n"
	if err := os.WriteFile(yamlPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}

	materializeBuiltinPacksForTest(t, cityDir)
	script := bundledGcBeadsBdScriptForTest(t)

	desiredTypes := "alpha,beta,gamma"
	// Source just the function definition out of the script and call it.
	// We extract via awk rather than sourcing the whole file because the
	// script's main block at the bottom runs unconditionally.
	bashCmd := fmt.Sprintf(`
set -e
eval "$(awk '/^ensure_types_custom_in_yaml\(\)/,/^}/' %q)"
ensure_types_custom_in_yaml %q %q
`, script, cityDir, desiredTypes)

	out, err := exec.Command("bash", "-c", bashCmd).CombinedOutput()
	if err != nil {
		t.Fatalf("ensure_types_custom_in_yaml: %v\n%s", err, out)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("ReadFile(after): %v", err)
	}
	got := string(data)
	// All baseline types must land.
	for _, must := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml missing required baseline type %q after merge:\n%s", must, got)
		}
	}
	// All existing entries must be preserved.
	for _, must := range []string{"legacy_a", "legacy_b", "legacy_c"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml lost existing type %q after merge:\n%s", must, got)
		}
	}
}

// TestGcBeadsBdEnsureTypesCustomInYaml_IdempotentWhenMatching pins the
// other half of the contract: when the existing line matches the desired
// value exactly, the function must be a no-op (no rewrite, no mtime change
// noise that downstream watchers would interpret as a change).
func TestGcBeadsBdEnsureTypesCustomInYaml_IdempotentWhenMatching(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yamlPath := filepath.Join(cityDir, ".beads", "config.yaml")
	desiredTypes := "alpha,beta,gamma"
	initial := "issue_prefix: gc\ntypes.custom: " + desiredTypes + "\n"
	if err := os.WriteFile(yamlPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}
	infoBefore, err := os.Stat(yamlPath)
	if err != nil {
		t.Fatalf("Stat(before): %v", err)
	}

	materializeBuiltinPacksForTest(t, cityDir)
	script := bundledGcBeadsBdScriptForTest(t)

	bashCmd := fmt.Sprintf(`
set -e
eval "$(awk '/^ensure_types_custom_in_yaml\(\)/,/^}/' %q)"
ensure_types_custom_in_yaml %q %q
`, script, cityDir, desiredTypes)

	out, err := exec.Command("bash", "-c", bashCmd).CombinedOutput()
	if err != nil {
		t.Fatalf("ensure_types_custom_in_yaml: %v\n%s", err, out)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("ReadFile(after): %v", err)
	}
	if string(data) != initial {
		t.Fatalf("config.yaml after idempotent call changed:\nbefore: %q\nafter:  %q", initial, string(data))
	}
	infoAfter, err := os.Stat(yamlPath)
	if err != nil {
		t.Fatalf("Stat(after): %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatalf("config.yaml mtime changed on idempotent call (before=%v after=%v) — function should short-circuit when value matches",
			infoBefore.ModTime(), infoAfter.ModTime())
	}
}

// TestGcBeadsBdEnsureTypesCustomInYaml_PreservesCustomExtensions pins the
// PR #2315 review fix: when the existing types.custom line contains
// pack/user-defined types beyond the GC baseline (the desiredTypes the
// caller passes), the function must MERGE — preserving the extensions —
// not narrow the set to just the baseline. The previous behavior treated
// any non-exact match as stale and rewrote with $types alone, silently
// dropping pack/user types and breaking later bead creation for those
// types. Mirrors mergeCustomTypes in
// internal/doctor/checks_custom_types.go.
func TestGcBeadsBdEnsureTypesCustomInYaml_PreservesCustomExtensions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yamlPath := filepath.Join(cityDir, ".beads", "config.yaml")
	// Existing line: GC baseline + 2 pack-defined extensions.
	initial := "issue_prefix: gc\ntypes.custom: alpha,beta,pack_custom_a,pack_custom_b\n"
	if err := os.WriteFile(yamlPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}

	materializeBuiltinPacksForTest(t, cityDir)
	script := bundledGcBeadsBdScriptForTest(t)

	// Caller passes only the baseline. The merge must keep pack_custom_a
	// and pack_custom_b — narrowing the set would defeat the doctor-merge
	// contract internal/doctor/checks_custom_types.go encodes.
	desiredTypes := "alpha,beta"
	bashCmd := fmt.Sprintf(`
set -e
eval "$(awk '/^ensure_types_custom_in_yaml\(\)/,/^}/' %q)"
ensure_types_custom_in_yaml %q %q
`, script, cityDir, desiredTypes)

	out, err := exec.Command("bash", "-c", bashCmd).CombinedOutput()
	if err != nil {
		t.Fatalf("ensure_types_custom_in_yaml: %v\n%s", err, out)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("ReadFile(after): %v", err)
	}
	got := string(data)
	for _, must := range []string{"alpha", "beta", "pack_custom_a", "pack_custom_b"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml lost custom type %q after merge:\n%s", must, got)
		}
	}
}

// TestGcBeadsBdEnsureTypesCustomInYaml_AddsMissingBaselineToCustomSet
// pins the other half of the merge: a YAML containing ONLY pack/user
// extensions (no overlap with the baseline) must end up with both the
// extensions AND the baseline after a call with the GC types.
func TestGcBeadsBdEnsureTypesCustomInYaml_AddsMissingBaselineToCustomSet(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yamlPath := filepath.Join(cityDir, ".beads", "config.yaml")
	initial := "issue_prefix: gc\ntypes.custom: pack_only_a,pack_only_b\n"
	if err := os.WriteFile(yamlPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}

	materializeBuiltinPacksForTest(t, cityDir)
	script := bundledGcBeadsBdScriptForTest(t)

	desiredTypes := "alpha,beta,gamma"
	bashCmd := fmt.Sprintf(`
set -e
eval "$(awk '/^ensure_types_custom_in_yaml\(\)/,/^}/' %q)"
ensure_types_custom_in_yaml %q %q
`, script, cityDir, desiredTypes)

	out, err := exec.Command("bash", "-c", bashCmd).CombinedOutput()
	if err != nil {
		t.Fatalf("ensure_types_custom_in_yaml: %v\n%s", err, out)
	}

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("ReadFile(after): %v", err)
	}
	got := string(data)
	for _, must := range []string{"alpha", "beta", "gamma", "pack_only_a", "pack_only_b"} {
		if !strings.Contains(got, must) {
			t.Errorf("config.yaml missing expected type %q after merge:\n%s", must, got)
		}
	}
}
