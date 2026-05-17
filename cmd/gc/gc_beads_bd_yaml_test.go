package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGcBeadsBdEnsureTypesCustomInYaml_ReplacesStaleValue pins the #2154
// gascity-side fix: ensure_types_custom_in_yaml must overwrite an existing
// stale types.custom line rather than skipping it.
//
// Pre-fix the function returned early on any `^types.custom:` match — so a
// city whose .beads/config.yaml had a STALE types list (subset of the SDK's
// required set, or an older format) silently kept the stale value and the
// required types never landed. bd reads this YAML key as a fallback when the
// database config table is unset, so a stale value here masks the types we
// actually need to register.
//
// The fix changed the early-return guard to compare the FULL desired line
// (grep -qxF) and to strip+re-append on mismatch.
func TestGcBeadsBdEnsureTypesCustomInYaml_ReplacesStaleValue(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yamlPath := filepath.Join(cityDir, ".beads", "config.yaml")
	initial := "issue_prefix: gc\ntypes.custom: stale,old,values\n"
	if err := os.WriteFile(yamlPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile(initial): %v", err)
	}

	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityDir)

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
	want := "issue_prefix: gc\ntypes.custom: " + desiredTypes + "\n"
	if string(data) != want {
		t.Fatalf("config.yaml after stale-replace:\n%q\nwant:\n%q", string(data), want)
	}
}

// TestGcBeadsBdEnsureTypesCustomInYaml_IdempotentWhenMatching pins the
// other half of the contract: when the existing line matches the desired
// value exactly, the function must be a no-op (no rewrite, no mtime change
// noise that downstream watchers would interpret as a change).
func TestGcBeadsBdEnsureTypesCustomInYaml_IdempotentWhenMatching(t *testing.T) {
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

	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityDir)

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
