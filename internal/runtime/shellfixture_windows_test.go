//go:build windows

package runtime

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// Command text for the portable setup-command tests, written in the
// interpreter the platform actually runs. The tests are about
// RunSetupCommand's contract — working directory, environment, captured
// streams, exit status, bounded wait — none of which is Windows-specific, so
// the only thing that needs splitting is the script.
//
// [Console]::Out and [Console]::Error are used in place of Write-Output and
// Write-Error because they write the exact bytes asked for. A PowerShell error
// record is a formatted multi-line object, which would make these tests
// assertions about PowerShell's rendering rather than about stream routing.

// fixturePrintCwdToFile writes the command's working directory to out.txt,
// named relatively so it proves where the command ran.
const fixturePrintCwdToFile = `[System.IO.File]::WriteAllText((Join-Path $PWD.Path 'out.txt'), $PWD.Path)`

// fixtureBothStreamsExit3 writes to both streams and fails with a distinctive
// code.
const fixtureBothStreamsExit3 = "[Console]::Out.WriteLine('out')\n[Console]::Error.WriteLine('err')\nexit 3"

// fixtureSleepPastTimeout outruns any per-command timeout a test sets.
const fixtureSleepPastTimeout = "Start-Sleep -Seconds 5"

// fixtureBackgroundChildExit0 leaves a child holding the inherited stdio and
// exits successfully, which is what the WaitDelay bound exists for.
//
// UseShellExecute = $false without redirection is what makes the child inherit
// this process's stdout and stderr handles; a Start-Process or Start-Job child
// gets its own and would not reproduce the hang. The interpreter re-launches
// itself by absolute path so the fixture does not depend on PATH.
const fixtureBackgroundChildExit0 = `$psi = [System.Diagnostics.ProcessStartInfo]::new()` + "\n" +
	`$psi.FileName = (Get-Process -Id $PID).Path` + "\n" +
	`$psi.Arguments = '-NoProfile -NonInteractive -Command Start-Sleep -Seconds 20'` + "\n" +
	`$psi.UseShellExecute = $false` + "\n" +
	`[void][System.Diagnostics.Process]::Start($psi)` + "\n" +
	`exit 0`

// fixtureEchoEnvSecretsExit3 echoes a session credential and an inherited one
// to opposite streams before failing.
const fixtureEchoEnvSecretsExit3 = `[Console]::Error.WriteLine("session=$env:GH_TOKEN")` + "\n" +
	`[Console]::Out.WriteLine("inherited=$env:ANTHROPIC_AUTH_TOKEN")` + "\n" +
	`exit 3`

// fixtureRequireEnv fails unless key holds value in the command's environment.
func fixtureRequireEnv(key, value string) string {
	return fmt.Sprintf(`if ($env:%s -ne '%s') { exit 1 }`, key, value)
}

// fixtureEchoArgs is a command prefix that writes each argument appended to it
// to stdout, one line of base64 of its UTF-8 bytes per argument. Base64 keeps
// the bytes clear of the console code page Windows PowerShell 5.1 encodes
// [Console]::Out with.
const fixtureEchoArgs = `& { foreach ($a in $args) { [Console]::Out.WriteLine([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$a))) } }`

// decodeEchoedArgs reads fixtureEchoArgs's output back into its arguments.
func decodeEchoedArgs(out string) ([]string, error) {
	lines := strings.Split(out, "\n")
	if lines[len(lines)-1] != "" {
		return nil, fmt.Errorf("output %q does not end in a newline", out)
	}
	args := make([]string, 0, len(lines)-1)
	for _, line := range lines[:len(lines)-1] {
		arg, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(line, "\r"))
		if err != nil {
			return nil, fmt.Errorf("output line %q: %w", line, err)
		}
		args = append(args, string(arg))
	}
	return args, nil
}

// eachShell runs fn under every interpreter a command could run under here,
// with run executing a command in dir the way production builds it for that
// interpreter.
func eachShell(t *testing.T, fn func(t *testing.T, run func(dir, command string) (stdout, stderr string, err error))) {
	t.Helper()
	eachPowerShell(t, func(t *testing.T, shell string) {
		fn(t, func(dir, command string) (string, string, error) {
			return runShellWithIn(t, shell, dir, command)
		})
	})
}
