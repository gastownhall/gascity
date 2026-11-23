//go:build windows

package clientgrant

import (
	"context"
	"strings"
	"testing"
)

// Command text for the exec tests, written in the interpreter the platform
// runs: PowerShell (runtime.ShellCommandContext). The tests are about the exec
// contract, not the interpreter, so only the script is split.

// realExecCommand prints valid only when GC_GRANT_INFO carries the city,
// proving env delivery, and fails when GC_EXEC_INFO was inherited.
func realExecCommand(valid string) string {
	return `if ($env:GC_EXEC_INFO) { exit 1 }; if ($env:GC_GRANT_INFO -notlike '*"city":"mc"*') { exit 1 }; [Console]::Out.WriteLine('` + valid + `')`
}

// failingCommand writes "boom" to stderr and exits non-zero.
const failingCommand = "[Console]::Error.WriteLine('boom'); exit 3"

// TestRunGrantCommand_RefusesPOSIXShell pins that a grant command naming a
// POSIX shell is refused rather than handed to whatever sh PATH offers.
func TestRunGrantCommand_RefusesPOSIXShell(t *testing.T) {
	_, err := runGrantCommand(context.Background(), "bash ./sign.sh", GrantInfo{Version: Version})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("runGrantCommand = %v, want a refusal", err)
	}
}
