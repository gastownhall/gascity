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
	scriptDir := filepath.Join(cityPath, ".gc", "system", "packs", "bd", "assets", "scripts")
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
	return logPath
}

func runRestart(t *testing.T, cityPath, root string, port int) ([]byte, error) {
	t.Helper()
	script := filepath.Join(root, restartScript)
	cmd := exec.Command("sh", script)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
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
