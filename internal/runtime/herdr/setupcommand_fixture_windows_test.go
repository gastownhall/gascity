//go:build windows

package herdr

import (
	"fmt"
	"regexp"
	"strings"
)

// Command text for the setup-command tests (pre_start, session_setup, and the
// redaction of their failures), written in the interpreter the platform runs:
// PowerShell (runtime.ShellCommandContext). The tests are about the
// setup-command contract, not the interpreter, so only the script is split.
// Files are written through [System.IO.File], which writes exactly the text
// given; `>` in Windows PowerShell writes UTF-16 with a byte-order mark and a
// trailing newline.

// psQuote renders s as a single-quoted PowerShell string literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// fixtureAppend appends text to the file at path.
func fixtureAppend(path, text string) string {
	return fmt.Sprintf("[System.IO.File]::AppendAllText(%s, %s)", psQuote(path), psQuote(text))
}

// fixturePrintCwdTo writes the command's working directory to name, resolved
// against that directory, so the file's location and its contents both say
// where the command ran.
func fixturePrintCwdTo(name string) string {
	return fmt.Sprintf("[System.IO.File]::WriteAllText((Join-Path $PWD.Path %s), $PWD.Path)", psQuote(name))
}

// fixtureWriteEnvTo writes the environment variable key's value to name,
// resolved against the command's working directory.
func fixtureWriteEnvTo(key, name string) string {
	return fmt.Sprintf("[System.IO.File]::WriteAllText((Join-Path $PWD.Path %s), $env:%s)", psQuote(name), key)
}

// fixtureSucceed does nothing, successfully.
const fixtureSucceed = "exit 0"

// fixtureSleepPastTimeout outruns any timeout a pre_start test sets.
const fixtureSleepPastTimeout = "Start-Sleep -Seconds 5"

// fixtureEchoToStderrExit writes word and a newline to stderr, then exits with
// code.
func fixtureEchoToStderrExit(word string, code int) string {
	return fmt.Sprintf("[Console]::Error.WriteLine(%s); exit %d", psQuote(word), code)
}

// fixturePrintToStderrExit writes text to stderr, then exits with code.
func fixturePrintToStderrExit(text string, code int) string {
	return fmt.Sprintf("[Console]::Error.Write(%s); exit %d", psQuote(text), code)
}

// fixtureMkdirAll creates the directory at path and any missing parents.
func fixtureMkdirAll(path string) string {
	return fmt.Sprintf("[void][System.IO.Directory]::CreateDirectory(%s)", psQuote(path))
}

// fixtureTouch creates the file at path if it does not exist.
func fixtureTouch(path string) string {
	return fmt.Sprintf("[System.IO.File]::AppendAllText(%s, '')", psQuote(path))
}

// fixtureRequireFile fails unless a file exists at path.
func fixtureRequireFile(path string) string {
	return fmt.Sprintf("if (-not [System.IO.File]::Exists(%s)) { exit 1 }", psQuote(path))
}

// envReference matches the $NAME spelling fixtureEchoExit's text uses for an
// environment variable.
var envReference = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// fixtureEchoExit writes text and a newline to stdout, then exits with code.
// Each $NAME in text expands to that environment variable.
func fixtureEchoExit(text string, code int) string {
	return fmt.Sprintf(`[Console]::Out.WriteLine("%s"); exit %d`, envReference.ReplaceAllString(text, `$$env:$1`), code)
}

// fixtureAppendLine appends text and a newline to the file at path.
func fixtureAppendLine(path, text string) string {
	return fmt.Sprintf("[System.IO.File]::AppendAllText(%s, %s)", psQuote(path), psQuote(text+"\n"))
}

// fixtureWriteEnvToPath writes the environment variable key's value to the
// file at path.
func fixtureWriteEnvToPath(key, path string) string {
	return fmt.Sprintf("[System.IO.File]::WriteAllText(%s, $env:%s)", psQuote(path), key)
}
