//go:build windows

package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecCommandRunnerKeepsBeadTextFromABatchFileBd pins that bead text never
// reaches cmd.exe through a batch-file bd, such as an npm .cmd shim: a title
// holding & would run a command as the gc host user, and one holding %NAME%
// would copy the host environment into the bead.
func TestExecCommandRunnerKeepsBeadTextFromABatchFileBd(t *testing.T) {
	dir := t.TempDir()
	bd := filepath.Join(t.TempDir(), "bd.cmd")
	if err := os.WriteFile(bd, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": bd})
	_, err := run(dir, "bd", "create", "--json", "a&echo>pwned", "-t", "task")
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Errorf("runner = %v, want the batch-file bd refused", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pwned")); statErr == nil {
		t.Error("cmd.exe ran the bead title: pwned exists in the store directory")
	}
}
