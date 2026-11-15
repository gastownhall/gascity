package runtime

import (
	"context"
	"os/exec"
)

// ShellCommandContext builds the unstarted [exec.Cmd] that runs one command
// string through the host platform's command interpreter: `sh -c` on Unix,
// PowerShell on Windows.
//
// [Config]'s command fields (PreStart, SessionSetup, SessionLive, Command) hold
// interpreter source text rather than an argv, so the contract package owns
// what that text means on each platform. Callers keep responsibility for Dir,
// Env and stdio; this decides only the interpreter and how the script reaches
// it.
//
// The two platforms do not share a command language and this does not pretend
// otherwise: a command string written in POSIX shell will not run on Windows,
// and one written in PowerShell will not run on Unix. What is identical across
// platforms is the surrounding contract — the working directory and environment
// the caller set, stdout and stderr captured through the caller's writers, and
// the exit status of the command's last operation reaching the caller as an
// [exec.ExitError] carrying the real code.
//
// On Windows, PowerShell and the programs it starts bound that contract in four
// ways:
//   - Exit status: a top-level return, or a PowerShell error after an earlier
//     native failure, can change a failure's code but never its verdict; see
//     shellExitEpilogue.
//   - Native arguments: PowerShell rebuilds a native program's command line
//     itself. Windows PowerShell 5.1, pwsh before 7.3, and any pwsh for a .cmd
//     or .bat target do so by legacy rules, under which an empty argument is
//     dropped and an embedded double quote, or a trailing backslash in an
//     argument holding a space, splits or alters what the program receives.
//     [ShellQuote] guarantees what the interpreter reads, not what a program's
//     own argument parser makes of it.
//   - Batch files: when the target is a .cmd or .bat, such as an npm shim,
//     cmd.exe parses that rebuilt line again, outside PowerShell's control: it
//     runs & | < > as commands, expands %NAME%, and ends the line at a line
//     break. No quoting prevents this, so caller data must not reach a batch
//     file's command line at all; deliver it through stdin, the environment or
//     a file, or refuse it.
//   - Output encoding: Windows PowerShell 5.1 writes text it produces itself
//     ([Console]::Out, Write-Output) in the console's code page, so non-ASCII
//     text a caller reads back, such as a credential, arrives byte for byte
//     only from pwsh or from a native program, whose output passes through
//     unchanged.
//
// An error means no interpreter could be resolved, or the command cannot be
// expressed within the platform's command-line limits. Both are reported rather
// than papered over, because a command that silently did not run is worse than
// one that failed.
func ShellCommandContext(ctx context.Context, command string) (*exec.Cmd, error) {
	return shellCommandContext(ctx, command)
}

// ShellSessionCommand builds the command for a long-running managed session.
//
// It differs from [ShellCommandContext] in one respect: an empty command means
// "a bare interpreter session" rather than "run nothing", so a session with no
// configured command is still a live process that holds the session open until
// its stdin reaches EOF.
func ShellSessionCommand(command string) (*exec.Cmd, error) {
	if command == "" {
		return shellSessionCommand()
	}
	return shellCommand(command)
}

// ShellQuote renders arg as one literal argument in the command language
// [ShellCommandContext] runs: a POSIX single-quoted word on Unix, a PowerShell
// single-quoted string on Windows. Data a caller splices into command text
// must go through it, because a value quoted for one interpreter is code to
// the other: POSIX quoting writes an embedded quote as '\”, which PowerShell
// reads as the end of the string, and PowerShell also ends a string at the
// typographic quotes ‘ ’ ‚ ‛, which POSIX quoting leaves alone.
//
// It is necessary, not sufficient: it fixes what the interpreter reads and
// nothing after. On Windows, when the command runs a .cmd or .bat target,
// cmd.exe reads the argument again and runs its & | < > and expands its %NAME%
// however it was quoted (see [ShellCommandContext]), so text that is not
// known to be a plain word must not reach such a command line at all.
//
// No quoting can carry NUL; a command holding one fails to start.
func ShellQuote(arg string) string {
	return shellQuote(arg)
}
