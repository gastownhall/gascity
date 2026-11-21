//go:build !windows

package clientauth

// Command text for the exec tests, written in the interpreter the platform
// runs: sh (runtime.ShellCommandContext). The tests are about the exec
// contract, not the interpreter, so only the script is split.

// realExecCommand prints a token built from GC_EXEC_INFO, proving env
// delivery, and fails when GC_GRANT_INFO was inherited.
func realExecCommand(exp string) string {
	return `test -z "$GC_GRANT_INFO" && printf '{"token":"t-%s","expiration_timestamp":"` + exp + `"}' "$(echo "$GC_EXEC_INFO" | grep -o mc)"`
}

// failingCommand writes to stderr and exits non-zero.
const failingCommand = "echo oops >&2; exit 7"
