//go:build !windows

package runtime

import (
	"context"
	"os/exec"
	"strings"
)

// posixShell is the interpreter every Unix command string is written for. It is
// `sh` and not the user's login shell: command strings come from city config
// that must behave the same for everyone who runs it.
const posixShell = "sh"

// defaultShellSession is what an empty session command runs — a shell, which
// reads its stdin and exits at EOF.
const defaultShellSession = "sh"

func shellCommandContext(ctx context.Context, command string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, posixShell, "-c", command), nil
}

func shellCommand(command string) (*exec.Cmd, error) {
	return exec.Command(posixShell, "-c", command), nil
}

func shellSessionCommand() (*exec.Cmd, error) {
	return shellCommand(defaultShellSession)
}

// shellQuote is POSIX single quoting, byte for byte what internal/shellquote's
// Quote writes; this package stays stdlib-only, so it is not imported.
func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
