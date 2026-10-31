//go:build !windows

package gitcred

// Command text for the helper tests, written in the interpreter the platform
// runs: sh (runtime.ShellCommandContext). The tests are about the helper
// contract, not the interpreter, so only the script is split.
const (
	// tokenHelperCommand prints a token and a trailing newline.
	tokenHelperCommand = "printf 'ghp_tok\\n'"
	// declineCommand succeeds and prints nothing.
	declineCommand = "true"
	// emitCommand prints a username/password response.
	emitCommand = "printf 'username=bot\\npassword=tok\\n'"
	// echoHostCommand answers with the request's host as the password, so the
	// answer proves the request reached the helper's stdin.
	echoHostCommand = `while IFS= read -r line; do case "$line" in host=*) printf 'password=%s\n' "${line#host=}";; esac; done`
	// crlfEmitCommand prints a username/password response with CRLF line
	// endings.
	crlfEmitCommand = "printf 'username=bot\\r\\npassword=tok\\r\\n'"
)
