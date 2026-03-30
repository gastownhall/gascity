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

func TestHookInjectFormatsOutput(t *testing.T) {
	runner := func(string, string) (string, error) { return "hw-1  open  Fix the bug\n", nil }
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "", true, runner, &stdout, &stderr)
	if code != 0 {
		t.Errorf("doHook(inject, work) = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "<system-reminder>") {
		t.Errorf("stdout missing <system-reminder>: %q", out)
	}
	if !strings.Contains(out, "</system-reminder>") {
		t.Errorf("stdout missing </system-reminder>: %q", out)
	}
	if !strings.Contains(out, "<work-items>") {
		t.Errorf("stdout missing <work-items>: %q", out)
	}
	if !strings.Contains(out, "hw-1") {
		t.Errorf("stdout missing work item: %q", out)
	}
	if !strings.Contains(out, "gc hook") {
		t.Errorf("stdout missing 'gc hook' hint: %q", out)
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
	code := cmdHook(nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "pwd="+rigDir) {
		t.Fatalf("stdout = %q, want command to run from rig root %q", out, rigDir)
	}
	if !strings.Contains(out, "args=ready --metadata-field gc.routed_to=myrig/polecat --unassigned --json --limit=1") {
		t.Fatalf("stdout = %q, want pool work_query args", out)
	}
}

func TestCmdHookPoolInstanceUsesTemplatePoolLabel(t *testing.T) {
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
	code := cmdHook(nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "pwd="+rigDir) {
		t.Fatalf("stdout = %q, want command to run from rig root %q", out, rigDir)
	}
	if !strings.Contains(out, "args=ready --metadata-field gc.routed_to=myrig/polecat --unassigned --json --limit=1") {
		t.Fatalf("stdout = %q, want pool template work_query args", out)
	}
}

func TestCmdHookExportsResolvedIdentityForFixedAgentQuery(t *testing.T) {
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
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_NAME", "")

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, false, &stdout, &stderr)
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
	if !strings.Contains(out, `args=ready --assignee=worker --json --limit=1`) {
		t.Fatalf("stdout = %q, want fixed-agent assignee-scoped ready query", out)
	}
}

func TestCmdHookExportsResolvedIdentityFromRigContext(t *testing.T) {
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
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_SESSION_NAME", "")

	wantAgent := "myrig/worker"
	wantSession := cliSessionName(cityDir, "test-city", wantAgent, "")

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, false, &stdout, &stderr)
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
	if !strings.Contains(out, `args=ready --assignee=`+wantSession+` --json --limit=1`) {
		t.Fatalf("stdout = %q, want rig-scoped session assignee query", out)
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
