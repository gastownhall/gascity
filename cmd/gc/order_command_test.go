package main

import (
	"fmt"
	goruntime "runtime"
	"strings"
)

// platformCommand picks a test's command text for the platform's interpreter:
// posix under sh, powerShell under PowerShell on Windows
// (runtime.ShellCommandContext). A command string is interpreter source, so a
// test that runs one needs it written for each.
func platformCommand(posix, powerShell string) string {
	if goruntime.GOOS == "windows" {
		return powerShell
	}
	return posix
}

// printCountCommand is a command that prints n under the platform's
// interpreter: `printf n` under sh, `Write-Output n` under PowerShell.
func printCountCommand(n int) string {
	return platformCommand(fmt.Sprintf("printf %d", n), fmt.Sprintf("Write-Output %d", n))
}

// printJSONCommand is a command that prints json as it is under the platform's
// interpreter: `printf '<json>'` under sh, `Write-Output '<json>'` under
// PowerShell. json must hold no single quote, and under sh no % or \ either.
func printJSONCommand(json string) string {
	return platformCommand("printf '"+json+"'", "Write-Output '"+json+"'")
}

// psLiteral renders s as a PowerShell single-quoted string.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
