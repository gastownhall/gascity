//go:build !windows

package herdr

import "fmt"

// Command text for the setup-command tests (pre_start, session_setup, and the
// redaction of their failures), written in the interpreter the platform runs
// (runtime.ShellCommandContext). The tests are about the setup-command
// contract (order, working directory, environment, failure reporting, the
// timeout), none of which is platform-specific, so only the script is split.

// fixtureAppend appends text to the file at path.
func fixtureAppend(path, text string) string { return "printf " + text + " >> " + path }

// fixturePrintCwdTo writes the command's working directory to name, resolved
// against that directory, so the file's location and its contents both say
// where the command ran.
func fixturePrintCwdTo(name string) string { return "pwd > " + name }

// fixtureWriteEnvTo writes the environment variable key's value to name,
// resolved against the command's working directory.
func fixtureWriteEnvTo(key, name string) string { return "printf %s \"$" + key + "\" > " + name }

// fixtureSucceed does nothing, successfully.
const fixtureSucceed = "true"

// fixtureSleepPastTimeout outruns any timeout a pre_start test sets.
const fixtureSleepPastTimeout = "sleep 5"

// fixtureEchoToStderrExit writes word and a newline to stderr, then exits with
// code.
func fixtureEchoToStderrExit(word string, code int) string {
	return fmt.Sprintf("echo %s >&2; exit %d", word, code)
}

// fixturePrintToStderrExit writes text to stderr, then exits with code.
func fixturePrintToStderrExit(text string, code int) string {
	return fmt.Sprintf("printf '%s' >&2; exit %d", text, code)
}

// fixtureMkdirAll creates the directory at path and any missing parents.
func fixtureMkdirAll(path string) string { return "mkdir -p " + sq(path) }

// fixtureTouch creates the file at path if it does not exist.
func fixtureTouch(path string) string { return "touch " + sq(path) }

// fixtureRequireFile fails unless a file exists at path.
func fixtureRequireFile(path string) string { return "test -f " + sq(path) }

// fixtureEchoExit writes text and a newline to stdout, then exits with code.
// Each $NAME in text expands to that environment variable.
func fixtureEchoExit(text string, code int) string {
	return fmt.Sprintf(`echo "%s"; exit %d`, text, code)
}

// fixtureAppendLine appends text and a newline to the file at path.
func fixtureAppendLine(path, text string) string { return "printf '" + text + `\n' >> ` + sq(path) }

// fixtureWriteEnvToPath writes the environment variable key's value to the
// file at path.
func fixtureWriteEnvToPath(key, path string) string {
	return `printf '%s' "$` + key + `" > ` + sq(path)
}
