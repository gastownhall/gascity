//go:build !windows

package clientgrant

// Command text for the exec tests, written in the interpreter the platform
// runs: sh (runtime.ShellCommandContext). The tests are about the exec
// contract, not the interpreter, so only the script is split.

// realExecCommand prints valid only when GC_GRANT_INFO carries the city,
// proving env delivery, and fails when GC_EXEC_INFO was inherited.
func realExecCommand(valid string) string {
	return `test -z "$GC_EXEC_INFO" && echo "$GC_GRANT_INFO" | grep -q '"city":"mc"' && printf '%s\n' '` + valid + `'`
}

// failingCommand writes "boom" to stderr and exits non-zero.
const failingCommand = "echo boom >&2; exit 3"
