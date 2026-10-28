//go:build windows

package subprocess

import (
	"fmt"
	"strings"
)

// Command text for the portable provider tests, written in the interpreter the
// platform actually runs. What these tests assert — session tracking, the
// control socket, environment overlay, working directory, stop and interrupt —
// is provider behavior, not shell behavior, so only the script splits.
//
// Every file these fixtures write is written with .NET rather than a
// redirection operator, and every line separator is an explicit LF. PowerShell
// redirection and WriteAllLines produce CRLF and a host-dependent encoding,
// which would make the assertions about PowerShell's output conventions rather
// than about what the provider passed through.

// cmdSleepLong outlives any test that starts it.
const cmdSleepLong = "Start-Sleep -Seconds 3600"

// cmdSleepLongExec has no separate meaning on Windows: PowerShell has no exec,
// so the interpreter always remains the session's direct process. The session
// still dies with the command because the interpreter exits when the script
// ends, and the owning Job Object reaps whatever the script started.
const cmdSleepLongExec = cmdSleepLong

// cmdSleepMedium outlives a conformance run.
const cmdSleepMedium = "Start-Sleep -Seconds 300"

// cmdExitsImmediately succeeds and exits at once.
const cmdExitsImmediately = "exit 0"

// psLiteral renders s as a PowerShell single-quoted literal, in which every
// character including a backslash stands for itself.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// cmdWriteEnvVarToFile writes one environment variable, newline-terminated, to
// path.
func cmdWriteEnvVarToFile(key, path string) string {
	return fmt.Sprintf(`[System.IO.File]::WriteAllText(%s, $env:%s + [char]10)`, psLiteral(path), key)
}

// cmdWriteEnvDumpToFile writes the whole environment to path as sorted
// KEY=VALUE lines.
func cmdWriteEnvDumpToFile(path string) string {
	return fmt.Sprintf(
		`[System.IO.File]::WriteAllText(%s, (((Get-ChildItem Env: | ForEach-Object { $_.Name + '=' + $_.Value }) | Sort-Object) -join [char]10) + [char]10)`,
		psLiteral(path))
}

// cmdWriteCwdToFile writes the command's working directory, newline-terminated,
// to path.
func cmdWriteCwdToFile(path string) string {
	return fmt.Sprintf(`[System.IO.File]::WriteAllText(%s, $PWD.Path + [char]10)`, psLiteral(path))
}
