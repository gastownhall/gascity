package convergence

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
)

func environKeys(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		out[name] = value
	}
	return out
}

// TestEveryEnvironKeyIsReserved pins checkScriptEnvWhitelistKeys to what
// Environ actually writes: a key Environ emits that the operator could also
// supply would let config replace an identity or store variable. Every
// optional field is set and every passthrough variable is present so the
// emitted set is the full one.
func TestEveryEnvironKeyIsReserved(t *testing.T) {
	for _, key := range []string{
		"BEADS_DOLT_AUTO_START", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER", "BEADS_DOLT_PASSWORD", "GC_DOLT", "GC_DOLT_HOST",
		"GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_INTEGRATION_REAL_BD",
	} {
		t.Setenv(key, "x")
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "creds"))
	city := t.TempDir()
	env := ConditionEnv{
		BeadID: "b", Iteration: 1, CityPath: city, StorePath: city, WorkDir: city,
		WispID: "w", DocPath: "d", MoleculeDir: city, ArtifactDir: city,
		AgentVerdict: "pass", AgentProvider: "p", AgentModel: "m",
	}.Environ()
	for name := range environKeys(env) {
		if !execenv.IsReservedCheckScriptEnvKey(name) {
			t.Errorf("Environ writes %q but execenv.IsReservedCheckScriptEnvKey does not reserve it; add it to execenv.CheckScriptEnvWrittenKeys", name)
		}
	}
}

// TestConditionEnvCheckScriptEnvIsAppendedAndCannotOverride pins the
// precedence contract: operator entries are appended in sorted order, and a
// reserved key that reaches Environ without validation is dropped rather than
// applied.
func TestConditionEnvCheckScriptEnvIsAppendedAndCannotOverride(t *testing.T) {
	city := t.TempDir()
	base := ConditionEnv{BeadID: "b", CityPath: city, StorePath: city}
	plain := base.Environ()

	withEnv := base
	withEnv.CheckScriptEnv = map[string]string{
		"PYTHONUSERBASE":  "/real/home/Library/Python/3.14",
		"ALPHA":           "1",
		"HOME":            "/real/home", // reserved: must not win
		"PATH":            "/evil",      // reserved: must not win
		"BEADS_DIR":       "/elsewhere", // reserved: must not win
		"BASH_ENV":        "/evil.sh",   // hazard: must not appear
		"XDG_CONFIG_HOME": "/real/home/.config",
		"bad-name":        "x",
	}
	got := withEnv.Environ()

	if !slices.Equal(got[:len(plain)], plain) {
		t.Fatalf("operator entries must be appended after the whitelist, not interleaved\n got=%v\nbase=%v", got, plain)
	}
	tail := got[len(plain):]
	want := []string{"ALPHA=1", "PYTHONUSERBASE=/real/home/Library/Python/3.14"}
	if !slices.Equal(tail, want) {
		t.Fatalf("appended tail = %v, want %v (sorted, reserved and invalid names dropped)", tail, want)
	}
	values := environKeys(got)
	if values["HOME"] != city {
		t.Errorf("HOME = %q, want the city %q", values["HOME"], city)
	}
	if values["BEADS_DIR"] != filepath.Join(city, ".beads") {
		t.Errorf("BEADS_DIR = %q, want the store's .beads", values["BEADS_DIR"])
	}
	if strings.Contains(values["PATH"], "/evil") {
		t.Errorf("PATH = %q carries the operator value", values["PATH"])
	}
	for _, key := range []string{"BASH_ENV", "XDG_CONFIG_HOME", "bad-name"} {
		if _, ok := values[key]; ok {
			t.Errorf("%s reached the environment", key)
		}
	}
	// Each key appears exactly once: no duplicate HOME for the child to pick.
	for _, key := range []string{"HOME", "PATH", "BEADS_DIR"} {
		if n := countPrefix(got, key+"="); n != 1 {
			t.Errorf("%s appears %d times, want 1", key, n)
		}
	}
}

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

// TestConditionEnvWithoutCheckScriptEnvIsUnchanged pins the default: a nil or
// empty table leaves Environ byte-identical to the whitelist alone.
func TestConditionEnvWithoutCheckScriptEnvIsUnchanged(t *testing.T) {
	city := t.TempDir()
	base := ConditionEnv{BeadID: "b", CityPath: city, StorePath: city}
	plain := base.Environ()
	empty := base
	empty.CheckScriptEnv = map[string]string{}
	if !slices.Equal(empty.Environ(), plain) {
		t.Fatalf("empty table changed Environ")
	}
}

// TestRunConditionCheckScriptEnvReachesHomeDerivedLookup is the trigger-to-
// effect regression for the setting. The fixture models how Python finds its
// per-user site-packages: from PYTHONUSERBASE when set, else from HOME. The
// marker lives under the controller's real home, which the sandbox replaces
// with the city, so the lookup fails until the operator hands the script
// PYTHONUSERBASE through check_script_env. No Python is needed: the rule is
// three lines of sh.
func TestRunConditionCheckScriptEnvReachesHomeDerivedLookup(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "home")
	city := filepath.Join(root, "city")
	for _, dir := range []string{filepath.Join(realHome, "Library", "Python", "site-packages", "yaml"), city} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realHome, "Library", "Python", "site-packages", "yaml", "__init__.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(city, "check.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
base="${PYTHONUSERBASE:-$HOME/Library/Python}"
if [ -f "$base/site-packages/yaml/__init__.py" ]; then exit 0; fi
echo "PyYAML not importable; HOME=$HOME PYTHONUSERBASE=${PYTHONUSERBASE:-}" >&2
exit 78
`), 0o755); err != nil {
		t.Fatal(err)
	}

	base := ConditionEnv{BeadID: "b", CityPath: city, StorePath: city}
	// Red: the sandbox HOME is the city, where there is no site-packages.
	result := RunCondition(context.Background(), script, base, 5*time.Second, 0)
	if result.Outcome != GateFail || result.ExitCode == nil || *result.ExitCode != 78 {
		t.Fatalf("without check_script_env: outcome=%s exit=%v stderr=%q, want GateFail with exit 78", result.Outcome, result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "HOME="+city) {
		t.Fatalf("stderr %q should show the sandbox HOME", result.Stderr)
	}

	// Green: the operator supplies the one variable the tool needs. HOME
	// stays the city; only the lookup input changed.
	withEnv := base
	withEnv.CheckScriptEnv = map[string]string{"PYTHONUSERBASE": filepath.Join(realHome, "Library", "Python")}
	result = RunCondition(context.Background(), script, withEnv, 5*time.Second, 0)
	if result.Outcome != GatePass {
		t.Fatalf("with check_script_env: outcome=%s stderr=%q, want pass", result.Outcome, result.Stderr)
	}
	if values := environKeys(withEnv.Environ()); values["HOME"] != city {
		t.Fatalf("HOME = %q after the setting, want the city (the setting adds a variable, it does not widen HOME)", values["HOME"])
	}
}
