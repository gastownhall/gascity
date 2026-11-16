//go:build windows

package gitcred

import (
	"strings"
	"testing"
)

// Command text for the helper tests, written in the interpreter the platform
// runs: PowerShell (runtime.ShellCommandContext). The tests are about the
// helper contract, not the interpreter, so only the script is split.
const (
	// tokenHelperCommand prints a token and a trailing newline.
	tokenHelperCommand = "[Console]::Out.Write(\"ghp_tok`n\")"
	// declineCommand succeeds and prints nothing.
	declineCommand = "exit 0"
	// emitCommand prints a username/password response.
	emitCommand = "[Console]::Out.Write(\"username=bot`npassword=tok`n\")"
	// echoHostCommand answers with the request's host as the password, so the
	// answer proves the request reached the helper's stdin.
	echoHostCommand = "foreach ($line in [Console]::In.ReadToEnd() -split \"`n\") { if ($line -like 'host=*') { [Console]::Out.Write('password=' + $line.Substring(5) + \"`n\") } }"
	// crlfEmitCommand prints a username/password response with CRLF line
	// endings, the way PowerShell's Write-Output ends every line.
	crlfEmitCommand = "Write-Output 'username=bot'; Write-Output 'password=tok'"
)

// TestCredentialHelpersRefusePOSIXShell pins that neither helper path hands a
// POSIX shell command to whatever sh PATH offers.
func TestCredentialHelpersRefusePOSIXShell(t *testing.T) {
	if _, err := Resolve(Rule{Match: "github.com", Helper: "sh -c 'echo tok'"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("Resolve = %v, want a refusal", err)
	}
	if _, _, err := RunCredentialCommand("bash ./helper.sh", Request{Host: "github.com"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("RunCredentialCommand = %v, want a refusal", err)
	}
}
