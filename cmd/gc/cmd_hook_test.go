package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookNoWork(t *testing.T) {
	runner := func(string, string) (string, error) { return "", nil }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", false, runner, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doHook(no work) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestHookHasWork(t *testing.T) {
	runner := func(string, string) (string, error) { return "hw-1  open  Fix the bug\n", nil }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(has work) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "hw-1") {
		t.Errorf("stdout = %q, want to contain %q", stdout.String(), "hw-1")
	}
}

func TestHookCommandError(t *testing.T) {
	runner := func(string, string) (string, error) { return "", fmt.Errorf("command failed") }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", false, runner, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doHook(error) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "command failed") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "command failed")
	}
}

func TestHookInjectNoWork(t *testing.T) {
	runner := func(string, string) (string, error) { return "", nil }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", true, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(inject, no work) = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestHookNoReadyMessagePrintsButExitsOne(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "✨ No ready work found (all issues have blocking dependencies)\n", nil
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", false, runner, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doHook(no-ready-message) = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "No ready work found") {
		t.Errorf("stdout = %q, want no-ready message", stdout.String())
	}
}

func TestHookInjectSuppressesNoReadyMessage(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "✨ No ready work found (all issues have blocking dependencies)\n", nil
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", true, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(inject, no-ready-message) = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestHookInjectIsNonIntrusiveWithWork(t *testing.T) {
	runner := func(string, string) (string, error) { return "hw-1  open  Fix the bug\n", nil }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", true, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(inject, work) = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty non-intrusive inject output", stdout.String())
	}
}

func TestHookCommandCodexInjectDoesNotBlockStop(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
work_query = "printf '[{\"id\":\"hw-1\",\"title\":\"Fix the bug\"}]'"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newHookCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"worker", "--inject", "--hook-format", "codex"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc hook command failed: %v; stderr=%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty non-blocking Stop hook output", stdout.String())
	}
}

func TestCmdHookSessionTemplateContextDoesNotScanSessionsForName(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBD := filepath.Join(fakeBin, "bd")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nprintf '[]'\n", logPath)
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_SESSION_ID", "mc-session")
	t.Setenv("GC_SESSION_NAME", "runtime-session")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithFormat(nil, true, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHookWithFormat() = %d, want 0; stderr=%s", code, stderr.String())
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", logPath, err)
	}
	logText := string(logData)
	if strings.Contains(logText, "--label=gc:session") {
		t.Fatalf("gc hook scanned all session beads before running work_query:\n%s", logText)
	}
	if !strings.Contains(logText, "--assignee=runtime-session") {
		t.Fatalf("gc hook did not pass runtime session name into work_query; bd log:\n%s", logText)
	}
}

func TestHookInjectAlwaysExitsZero(t *testing.T) {
	// Even on command failure, inject mode exits 0.
	runner := func(string, string) (string, error) { return "", fmt.Errorf("command failed") }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", true, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(inject, error) = %d, want 0", code)
	}
}

func TestHookPassesWorkQuery(t *testing.T) {
	// Verify the runner receives the correct work query string.
	var receivedCmd, receivedDir string
	runner := func(cmd, dir string) (string, error) {
		receivedCmd = cmd
		receivedDir = dir
		return "item-1\n", nil
	}
	var stdout, stderr bytes.Buffer
	doHook("bd ready --assignee=mayor", "/tmp/work", false, runner, &stdout, &stderr)
	if receivedCmd != "bd ready --assignee=mayor" {
		t.Errorf("runner command = %q, want %q", receivedCmd, "bd ready --assignee=mayor")
	}
	if receivedDir != "/tmp/work" {
		t.Errorf("runner dir = %q, want %q", receivedDir, "/tmp/work")
	}
}

func TestWorkQueryHasReadyWorkEmptyJSONArray(t *testing.T) {
	if workQueryHasReadyWork("[]") {
		t.Fatal("workQueryHasReadyWork([]) = true, want false")
	}
}

func TestWorkQueryHasReadyWorkNonEmptyJSONArray(t *testing.T) {
	if !workQueryHasReadyWork(`[{"id":"abc"}]`) {
		t.Fatal("workQueryHasReadyWork(non-empty array) = false, want true")
	}
}

func TestCmdHookUsesAgentCityAndRigRoot(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "polecat-1")
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "polecat"
dir = "myrig"

[agent.pool]
min = 0
max = 5
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'pwd=%s\nstore_root=%s\nstore_scope=%s\nprefix=%s\nrig=%s\nrig_root=%s\nargs=%s\n' \"$PWD\" \"${GC_STORE_ROOT:-}\" \"${GC_STORE_SCOPE:-}\" \"${GC_BEADS_PREFIX:-}\" \"${GC_RIG:-}\" \"${GC_RIG_ROOT:-}\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "myrig/polecat")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdHook(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "pwd="+rigDir) {
		t.Fatalf("stdout = %q, want command to run from rig root %q", out, rigDir)
	}
	if !strings.Contains(out, "store_root="+rigDir) {
		t.Fatalf("stdout = %q, want GC_STORE_ROOT=%q", out, rigDir)
	}
	if !strings.Contains(out, "store_scope=rig") {
		t.Fatalf("stdout = %q, want GC_STORE_SCOPE=rig", out)
	}
	if !strings.Contains(out, "prefix=my") {
		t.Fatalf("stdout = %q, want GC_BEADS_PREFIX=my", out)
	}
	if !strings.Contains(out, "rig=myrig") {
		t.Fatalf("stdout = %q, want GC_RIG=myrig", out)
	}
	if !strings.Contains(out, "rig_root="+rigDir) {
		t.Fatalf("stdout = %q, want GC_RIG_ROOT=%q", out, rigDir)
	}
	// Tiered query: first tier checks in_progress assigned to session name.
	if !strings.Contains(out, "args=list --status in_progress --assignee=myrig--polecat --json --limit=1") {
		t.Fatalf("stdout = %q, want pool work_query args", out)
	}
}

// TestCmdHookOverridesInheritedCityBeadsDir is a regression test for #514:
// when the gc hook process inherits a city-scoped BEADS_DIR from its parent,
// the work query subprocess must still run against the rig-scoped bead store
// for rig-backed agents. Without the fix, the subprocess reads the city
// store and returns [] for rig-routed work.
func TestCmdHookOverridesInheritedCityBeadsDir(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "worker"
dir = "myrig"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'beads_dir=%s\\nrig_root=%s\\nrig=%s\\n' \"$BEADS_DIR\" \"$GC_RIG_ROOT\" \"$GC_RIG\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", rigDir)
	// Pollute parent env with a city-scoped BEADS_DIR. Without the fix,
	// this value leaks into the fake-bd subprocess and the hook reads the
	// city store instead of the rig store.
	cityBeads := filepath.Join(cityDir, ".beads")
	t.Setenv("BEADS_DIR", cityBeads)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	wantBeads := filepath.Join(rigDir, ".beads")
	if !strings.Contains(out, "beads_dir="+wantBeads) {
		t.Fatalf("stdout = %q, want BEADS_DIR=%s (rig store), not inherited city value", out, wantBeads)
	}
	if strings.Contains(out, "beads_dir="+cityBeads) {
		t.Fatalf("stdout = %q, inherited city BEADS_DIR leaked into subprocess", out)
	}
	if !strings.Contains(out, "rig_root="+rigDir) {
		t.Fatalf("stdout = %q, want GC_RIG_ROOT=%s", out, rigDir)
	}
	if !strings.Contains(out, "rig=myrig") {
		t.Fatalf("stdout = %q, want GC_RIG=myrig", out)
	}
}

// TestCmdHookResolvesRelativeRigPath guards the relative-rig-path handling:
// when `[[rigs]].path` is relative (e.g. "myrig-repo"), cmdHook must
// normalize it to an absolute path before building the rig env, or
// BEADS_DIR/GC_RIG_ROOT land as relative garbage and bdRuntimeEnvForRig's
// rig-matching loop misses the rig entirely (skipping GC_RIG and any
// per-rig Dolt overrides).
func TestCmdHookResolvesRelativeRigPath(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigAbs := filepath.Join(cityDir, "myrig-repo")
	if err := os.MkdirAll(rigAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Relative rig path — the fix normalizes this to cityDir/myrig-repo.
	cityToml := `[workspace]
name = "test-city"

[[rigs]]
name = "myrig"
path = "myrig-repo"

[[agent]]
name = "worker"
dir = "myrig"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'beads_dir=%s\\nrig_root=%s\\nrig=%s\\n' \"$BEADS_DIR\" \"$GC_RIG_ROOT\" \"$GC_RIG\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", rigAbs)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	wantBeads := filepath.Join(rigAbs, ".beads")
	if !strings.Contains(out, "beads_dir="+wantBeads) {
		t.Fatalf("stdout = %q, want absolute BEADS_DIR=%s (relative rig path should be resolved)", out, wantBeads)
	}
	if !strings.Contains(out, "rig_root="+rigAbs) {
		t.Fatalf("stdout = %q, want absolute GC_RIG_ROOT=%s", out, rigAbs)
	}
	// GC_RIG is only set when bdRuntimeEnvForRig's loop finds a matching
	// rig config. With unresolved relative paths, samePath() fails and
	// GC_RIG stays empty — this assertion catches that regression.
	if !strings.Contains(out, "rig=myrig") {
		t.Fatalf("stdout = %q, want GC_RIG=myrig (rig-matching loop must find the rig)", out)
	}
}

func TestCmdHookExpandsTemplateCommandsWithCityFallback(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := filepath.Join(t.TempDir(), "demo-city")
	rigDir := filepath.Join(cityDir, "frontend")
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[[rigs]]
name = "frontend"
path = %q

[[agent]]
name = "worker"
dir = "frontend"
work_query = "bd {{.CityName}} {{.Rig}} {{.AgentBase}}"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'args=%s\\n' \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", rigDir)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "args=demo-city frontend worker") {
		t.Fatalf("stdout = %q, want expanded city/rig/agent-base template", stdout.String())
	}
}

// TestCmdHookNonRigDirAgentUsesCityStore guards the rig-detection heuristic
// in hookQueryEnv: agents whose `dir` is a plain path (not a configured
// rig) must fall back to the city-scoped bead store, not mistakenly be
// treated as rig-backed and pointed at `<dir>/.beads`.
func TestCmdHookNonRigDirAgentUsesCityStore(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "workdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No [[rigs]] section — "workdir" is a plain agent dir, not a rig.
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
dir = "workdir"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'beads_dir=%s\\nrig_root=%s\\n' \"$BEADS_DIR\" \"$GC_RIG_ROOT\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	wantBeads := filepath.Join(cityDir, ".beads")
	if !strings.Contains(out, "beads_dir="+wantBeads) {
		t.Fatalf("stdout = %q, want BEADS_DIR=%s (city store), non-rig agent must not be pointed at <dir>/.beads", out, wantBeads)
	}
	// Non-rig agents must not receive GC_RIG_ROOT. doHook strips trailing
	// whitespace, so the empty value lands at the very end of the output.
	if !strings.HasSuffix(out, "rig_root=") {
		t.Fatalf("stdout = %q, want empty GC_RIG_ROOT for non-rig agent", out)
	}
}

func TestCmdHookPoolInstanceUsesTemplatePoolLabel(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	workDir := filepath.Join(cityDir, ".gc", "worktrees", "myrig", "polecat-1")
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "polecat"
dir = "myrig"

[agent.pool]
min = 0
max = 5
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'pwd=%s\\nargs=%s\\n' \"$PWD\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_AGENT", "myrig/polecat-1")
	t.Setenv("GC_SESSION_NAME", "myrig--polecat-1")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdHook(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "pwd="+rigDir) {
		t.Fatalf("stdout = %q, want command to run from rig root %q", out, rigDir)
	}
	// Tiered query: first tier checks in_progress assigned to session name.
	if !strings.Contains(out, "args=list --status in_progress --assignee=myrig--polecat-1 --json --limit=1") {
		t.Fatalf("stdout = %q, want pool template work_query args", out)
	}
}

func TestWorkQueryEnvForDirOverridesInheritedPWD(t *testing.T) {
	env := []string{
		"PATH=/tmp/bin",
		"PWD=/tmp/stale",
		"GC_CITY=/tmp/city",
	}

	got := workQueryEnvForDir(env, "/tmp/rig")

	if strings.Contains(strings.Join(got, "\n"), "PWD=/tmp/stale") {
		t.Fatalf("workQueryEnvForDir preserved stale PWD: %v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "PWD=/tmp/rig") {
		t.Fatalf("workQueryEnvForDir missing updated PWD: %v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "PATH=/tmp/bin") {
		t.Fatalf("workQueryEnvForDir dropped unrelated env: %v", got)
	}
}

func TestCmdHookExportsResolvedIdentityForFixedAgentQuery(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'agent=%s\\nsession=%s\\nargs=%s\\n' \"$GC_AGENT\" \"$GC_SESSION_NAME\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "agent=worker") {
		t.Fatalf("stdout = %q, want GC_AGENT=worker", out)
	}
	if !strings.Contains(out, "session=worker") {
		t.Fatalf("stdout = %q, want GC_SESSION_NAME=worker", out)
	}
	// Tiered query: first tier checks in_progress assigned to session name.
	if !strings.Contains(out, `args=list --status in_progress --assignee=worker --json --limit=1`) {
		t.Fatalf("stdout = %q, want metadata-routed work query", out)
	}
}

func TestCmdHookExportsResolvedIdentityFromRigContext(t *testing.T) {
	t.Setenv("GC_TMUX_SESSION", "host-session")
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig-repo")
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "worker"
dir = "myrig"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'agent=%s\\nsession=%s\\nargs=%s\\n' \"$GC_AGENT\" \"$GC_SESSION_NAME\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", rigDir)

	wantAgent := "myrig/worker"
	wantSession := cliSessionName(cityDir, "test-city", wantAgent, "")

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "agent="+wantAgent) {
		t.Fatalf("stdout = %q, want GC_AGENT=%s", out, wantAgent)
	}
	if !strings.Contains(out, "session="+wantSession) {
		t.Fatalf("stdout = %q, want GC_SESSION_NAME=%s", out, wantSession)
	}
	// Tiered query: first tier checks in_progress assigned to session name.
	if !strings.Contains(out, `args=list --status in_progress --assignee=myrig--worker --json --limit=1`) {
		t.Fatalf("stdout = %q, want metadata-routed work query", out)
	}
}

func TestDoHookNormalizesSingleObjectOutputToArray(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := func(_, _ string) (string, error) {
		return `{"id":"bd-1","title":"Work"}`, nil
	}

	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != `[{"id":"bd-1","title":"Work"}]` {
		t.Fatalf("stdout = %q, want normalized JSON array", got)
	}
}
