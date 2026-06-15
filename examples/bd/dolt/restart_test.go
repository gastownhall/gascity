package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const restartScript = "commands/restart/run.sh"

// writeFakeBeadsBDForRestart writes a stub gc-beads-bd that records each
// invocation's first argument and exits with the code specified for that
// op. ops that aren't in opExitCodes exit 0.
func writeFakeBeadsBDForRestart(t *testing.T, cityPath string, opExitCodes map[string]int) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bd dir: %v", err)
	}
	logPath := filepath.Join(cityPath, "bd.log")
	var cases strings.Builder
	for op, code := range opExitCodes {
		fmt.Fprintf(&cases, "  %s) exit %d ;;\n", op, code)
	}
	body := `#!/bin/sh
printf '%s\n' "$1" >> "` + logPath + `"
case "$1" in
` + cases.String() + `  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(scriptDir, "gc-beads-bd.sh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd script: %v", err)
	}
	enospcHelper := `#!/bin/sh
recovery_should_skip_due_to_enospc() {
  [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
  tail -n 1000 "$LOG_FILE" 2>/dev/null \
    | grep -qE 'no space left on device|copy_file_range:.*no space|ENOSPC' \
    || return 1
  return 0
}
`
	if err := os.WriteFile(filepath.Join(scriptDir, "dolt-enospc.sh"), []byte(enospcHelper), 0o644); err != nil {
		t.Fatalf("write fake enospc helper: %v", err)
	}
	return logPath
}

func runRestart(t *testing.T, cityPath, root string, port int) ([]byte, error) {
	t.Helper()
	return runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)})
}

func runRestartWithEnv(t *testing.T, cityPath, root string, extraEnv []string, args ...string) ([]byte, error) {
	t.Helper()
	script := filepath.Join(root, restartScript)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_CITY_RUNTIME_DIR", "GC_PACK_STATE_DIR", "GC_DOLT_LOG_FILE",
		"GC_BEADS_BD_SCRIPT",
	),
		"PATH="+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

func TestRestartCallsStopThenStart_HappyPath(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start', got %q\noutput:\n%s", got, out)
	}
}

func TestRestartCallsStartWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exits 2 when no managed dolt PID is found. restart must
	// treat that as success and still invoke start.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 2, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed when stop reported nothing-running: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' (exit 2 on stop is recoverable), got %q\noutput:\n%s", got, out)
	}
}

func TestRestartDoesNotRequirePortWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 2, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, nil)
	if err != nil {
		t.Fatalf("gc dolt restart failed without a resolved runtime port: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' without a runtime port, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartAbortsAndDoesNotStartWhenStopFails(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exit code 1 is the genuine-failure path (e.g., couldn't kill
	// the managed PID). restart must abort without calling start so the
	// operator can investigate.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 1, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when stop failed:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if strings.Contains(string(data), "start") {
		t.Fatalf("restart called start after stop failed; ops log:\n%s\noutput:\n%s", data, out)
	}
}

func TestRestartPropagatesStartFailureWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 1})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when start failed:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: start failed (exit 1)") {
		t.Fatalf("restart did not report start failure; output:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected restart to attempt stop and start before reporting start failure, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartRefusesRecentENOSPCUnlessForced(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	logPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir dolt log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("fatal: no space left on device\n"), 0o644); err != nil {
		t.Fatalf("write dolt log: %v", err)
	}

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly ignored recent ENOSPC:\n%s", out)
	}
	if !strings.Contains(string(out), "recent Dolt log shows ENOSPC") {
		t.Fatalf("restart did not explain ENOSPC refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite ENOSPC refusal; ops log:\n%s\noutput:\n%s", data, out)
	}

	out, err = runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)}, "--force")
	if err != nil {
		t.Fatalf("gc dolt restart --force failed despite fake stop/start success: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected forced restart to call stop then start, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartRejectsRemoteHostWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, []string{
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_HOST=example.internal",
	})
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded for a remote host:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: not supported for remote dolt servers") {
		t.Fatalf("restart did not explain remote-host refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite remote-host refusal; ops log:\n%s\noutput:\n%s", data, out)
	}
}
