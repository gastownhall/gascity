// Package dolt_test validates that the dolt pack's sql.sh script
// forwards extra arguments to the underlying `dolt sql` invocation.
// Without forwarding, `gc dolt sql -q "QUERY"` is silently dropped:
// the script execs `dolt … sql` and the agent's diagnostic SQL never
// runs. The operational-awareness fragment relies on this for the
// non-fatal Dolt diagnostic protocol (issue #1485).
package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sqlScript = "commands/sql/run.sh"

// writeFakeDolt installs a stub `dolt` binary in dir that records
// argv (one arg per line) to a file inside dir and exits 0. Returns
// the argv-log path. Used to assert the wrapper script forwards args
// verbatim without booting a real Dolt server.
func writeFakeDolt(t *testing.T, dir string) string {
	t.Helper()
	argvFile := filepath.Join(dir, "argv.log")
	body := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a"
done > "` + argvFile + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return argvFile
}

// readArgv returns the recorded argv from a single fake-dolt
// invocation. Empty if the binary was never called.
func readArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	trimmed := strings.Trim(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestSQLScriptForwardsQueryArgs is the regression guard for the
// arg-forwarding gap that motivated the #1485 fix. The wrapper used
// to call `exec dolt $args sql` (no "$@"), which silently dropped
// `-q "QUERY"`. The non-fatal Dolt diagnostic protocol (SHOW FULL
// PROCESSLIST via `gc dolt sql -q`) only works if the wrapper passes
// trailing args through.
func TestSQLScriptForwardsQueryArgs(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, sqlScript)

	binDir := t.TempDir()
	argvFile := writeFakeDolt(t, binDir)

	// Provide a minimal data dir so the embedded branch finds a
	// dolt-shaped subdirectory and reaches the exec. GC_DOLT_DATA_DIR
	// overrides runtime.sh's DOLT_DATA_DIR computation directly.
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "testdb", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	// Strip every Dolt-related env var the script consults so the
	// branch selection inside the wrapper is determined entirely by
	// the values set below. An ambient GC_DOLT_HOST in CI or a
	// developer shell would otherwise silently flip the branch and
	// hide whether the embedded path actually exercised "$@".
	// Use a non-numeric GC_DOLT_PORT so managed_runtime_tcp_reachable
	// (runtime.sh) takes its `''|*[!0-9]*` early-return path and the
	// script falls deterministically into the embedded branch. This
	// avoids the bind-then-close TOCTOU window of an "unused" port.
	cmd := exec.Command("sh", script, "-q", "SELECT 1")
	cmd.Env = append(filteredEnv("PATH",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR",
		"GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=unreachable",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sql.sh exited non-zero: %v\noutput: %s", err, out)
	}

	argv := readArgv(t, argvFile)
	if len(argv) == 0 {
		t.Fatalf("fake dolt was never invoked; output: %s", out)
	}

	sqlIdx := -1
	dataDirIdx := -1
	for i, a := range argv {
		switch a {
		case "sql":
			if sqlIdx == -1 {
				sqlIdx = i
			}
		case "--data-dir":
			if dataDirIdx == -1 {
				dataDirIdx = i
			}
		}
	}

	// The embedded branch must be the one that ran (--data-dir before
	// sql). If a future bug flips the script into the connected branch,
	// this assertion catches it before the arg-forwarding check below.
	if dataDirIdx == -1 || dataDirIdx >= sqlIdx {
		t.Fatalf("argv did not exercise the embedded branch (--data-dir before sql): %v", argv)
	}
	if sqlIdx+2 >= len(argv) {
		t.Fatalf("argv truncated after `sql`: %v (-q SELECT 1 was dropped)", argv)
	}
	if argv[sqlIdx+1] != "-q" || argv[sqlIdx+2] != "SELECT 1" {
		t.Fatalf("argv after `sql` = %v; want [-q, SELECT 1] (the wrapper is dropping trailing args)", argv[sqlIdx+1:])
	}
}
