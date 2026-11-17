//go:build windows

package clientauth

import (
	"context"
	"strings"
	"testing"
)

// Command text for the exec tests, written in the interpreter the platform
// runs: PowerShell (runtime.ShellCommandContext). The tests are about the exec
// contract, not the interpreter, so only the script is split.

// realExecCommand prints a token built from GC_EXEC_INFO, proving env
// delivery, and fails when GC_GRANT_INFO was inherited.
func realExecCommand(exp string) string {
	return `if ($env:GC_GRANT_INFO) { exit 1 }; $m = if ($env:GC_EXEC_INFO -match 'mc') { 'mc' } else { '' }; [Console]::Out.Write('{"token":"t-' + $m + '","expiration_timestamp":"` + exp + `"}')`
}

// failingCommand writes to stderr and exits non-zero.
const failingCommand = "[Console]::Error.WriteLine('oops'); exit 7"

// TestRunCredentialCommand_RefusesPOSIXShell pins that a credential command
// naming a POSIX shell is refused rather than handed to whatever sh PATH
// offers.
func TestRunCredentialCommand_RefusesPOSIXShell(t *testing.T) {
	_, err := runCredentialCommand(context.Background(), "sh -c 'echo {}'", ExecInfo{Version: Version})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("runCredentialCommand = %v, want a refusal", err)
	}
}
