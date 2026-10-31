//go:build windows

package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
)

// powerShellNames are the interpreters a command string is run through, in
// preference order. cmd.exe is deliberately absent: its quoting rules do not
// survive the round trip a command string makes through CreateProcess, so on
// Windows command strings run only through PowerShell.
var powerShellNames = []string{"pwsh.exe", "powershell.exe"}

// commandLineLimit is the CreateProcess command-line ceiling in UTF-16 code
// units, counting the terminating NUL.
const commandLineLimit = 32767

// shellExitEpilogue makes the interpreter exit with the status of the script's
// last operation, which is what `sh -c` does and PowerShell does not.
//
// Left alone, PowerShell reports its own success at parsing and running the
// script: a native program that exited 3 leaves that code in $LASTEXITCODE and
// PowerShell still exits 0, so a failed setup command would be recorded as a
// successful one. The epilogue reads both halves of "did the last thing work":
// $? covers cmdlets and native programs alike, $LASTEXITCODE carries the real
// code of a native one. $? is captured first because every later statement,
// assignment included, overwrites it.
//
// A script that calls exit itself never reaches here, and a terminating error
// unwinds past it — PowerShell's own exit code is correct in both cases.
//
// A success exits before $LASTEXITCODE is read, and a failure reads it inside
// try. The script's own preferences are still in force here, and under
// Set-StrictMode reading a variable that was never set is an error: a script
// that ran no native program would otherwise print that error after
// succeeding, and with $ErrorActionPreference = 'Stop' as well it would exit 1
// (both measured, on each interpreter).
//
// The blank line before the epilogue is load-bearing. A backtick is
// PowerShell's line-continuation character, so a command ending in one escapes
// the newline that separates it from what follows and swallows the next line.
// With a single separator that line is `$gcShellOK = $?`, which is then never
// assigned: the epilogue reads it as false and reports a command that
// succeeded as a failure. The second newline cannot be escaped by the first,
// because the backtick has already consumed one.
//
// Known limits, each of which changes a failure's code but never its verdict:
//   - A top-level `return` ends the script before the epilogue runs, and
//     PowerShell then reports 1 rather than the command's real code. Every
//     construct that would catch it costs more than it saves, which is measured
//     rather than assumed — see TestShellExitEpilogueRejectedAlternatives.
//   - $LASTEXITCODE holds the last native program's code until another native
//     program runs, so a script whose last failure is a PowerShell error, after
//     a native program that failed earlier, reports that program's code rather
//     than 1. Neither variable records which kind of operation ran last.
const shellExitEpilogue = "\n\n" +
	"$gcShellOK = $?\n" +
	"if ($gcShellOK) { exit 0 }\n" +
	"try { $gcShellCode = $LASTEXITCODE } catch { $gcShellCode = $null }\n" +
	"if ($gcShellCode -is [int] -and $gcShellCode -ne 0) { exit $gcShellCode }\n" +
	"exit 1\n"

// inBoxPowerShell is where Windows PowerShell is always installed.
func inBoxPowerShell() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

// findPowerShell resolves the interpreter to run. The in-box absolute path is
// tried after PATH so a command still runs under an environment whose PATH was
// trimmed or replaced, which a session environment legitimately may be.
func findPowerShell() (string, error) {
	for _, name := range powerShellNames {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	fallback := inBoxPowerShell()
	if info, err := os.Stat(fallback); err == nil && !info.IsDir() {
		return fallback, nil
	}
	return "", fmt.Errorf("no PowerShell interpreter found: looked for %s on PATH and %s", strings.Join(powerShellNames, ", "), fallback)
}

// powerShellInvocation builds the interpreter path and argv for command.
func powerShellInvocation(command string) (string, []string, error) {
	shell, err := findPowerShell()
	if err != nil {
		return "", nil, err
	}
	args, err := powerShellArgs(shell, command)
	if err != nil {
		return "", nil, err
	}
	return shell, args, nil
}

// powerShellArgs builds the argv that runs command under shell.
//
// The script is the one argument after -Command, which PowerShell takes as
// everything that follows it. exec escapes each argument by the CreateProcess
// argv rules, and both pwsh and Windows PowerShell split their command line by
// those rules, so the interpreter parses exactly the script handed in, double
// quotes, backslashes, `$`, backticks and newlines included. That is measured
// on each interpreter present, not assumed; see
// TestShellCommandScriptReachesEachInterpreterVerbatim.
//
// -EncodedCommand is deliberately not used, although it sidesteps quoting. An
// interpreter started with it assumes its caller is another PowerShell and,
// when stderr is redirected, writes error, warning and progress records there
// as CLIXML, serialized objects rather than text. pwsh can be told otherwise
// with -OutputFormat Text; Windows PowerShell 5.1 cannot, because its choice
// never consults the output format. -Command writes text on both.
//
// -NoProfile and -NonInteractive are not tuning: a user profile prints to
// stdout, which corrupts the output this package captures and folds into
// failure messages, and it is loaded on every launch. -NonInteractive keeps a
// prompt from stalling a command that has no one to answer it.
func powerShellArgs(shell, command string) ([]string, error) {
	script := scriptPrologue(shell, command) + command + shellExitEpilogue
	if strings.IndexByte(script, 0) >= 0 {
		return nil, errors.New("command contains a NUL character, which a Windows command line cannot carry")
	}
	args := []string{"-NoProfile", "-NonInteractive", "-Command", script}
	if n := commandLineLength(append([]string{shell}, args...)); n >= commandLineLimit {
		return nil, fmt.Errorf("command does not fit a Windows command line: it needs %d characters and at most %d are allowed; move it into a script file and run that file instead", n, commandLineLimit-1)
	}
	return args, nil
}

// plainTextPrologue turns off pwsh's ANSI rendering for the rest of the
// script. pwsh colors its error, warning, verbose and debug records with escape
// sequences even when the stream is a pipe, so a captured failure read
// "\x1b[31;1mWrite-Error: …" (measured, pwsh 7.6, with and without TERM set).
// NO_COLOR would have the same effect on pwsh, but everything the script runs
// would inherit it.
const plainTextPrologue = "if ($PSStyle) { $PSStyle.OutputRendering = 'PlainText' }; "

// gcAliasPrologue removes PowerShell's `gc` alias, so that `gc` in a command
// runs the gc program on PATH, Gas City, as it does under sh. Both interpreters
// define `gc` as an alias for Get-Content (ReadOnly in pwsh, ReadOnly and
// AllScope in Windows PowerShell 5.1), and an alias outranks a program, so
// without this `gc hook` runs Get-Content. Remove-Item -Force removes the alias
// in both (measured). Test-Path keeps the prologue quiet where the alias is
// already gone: an unguarded Remove-Item there writes an error and leaves $?
// false (measured).
const gcAliasPrologue = "if (Test-Path -LiteralPath Alias:gc) { Remove-Item -LiteralPath Alias:gc -Force }; "

// scriptPrologue is what powerShellArgs puts in front of command for shell:
// gcAliasPrologue, after plainTextPrologue when shell is pwsh.
//
// Only pwsh gets plainTextPrologue. Windows PowerShell has no $PSStyle and
// writes no escape sequences, and its error records repeat the script line
// they came from, so a prologue there would only add noise.
//
// A script whose first statement must come first gets no prologue: `using`
// must precede every other statement, and a param block must open the script.
// It keeps pwsh's colors and PowerShell's `gc` alias rather than fail to
// parse, so in it Gas City is `gc.exe`.
func scriptPrologue(shell, command string) string {
	if scriptMustLead(command) {
		return ""
	}
	name := strings.ToLower(filepath.Base(shell))
	if name != "pwsh.exe" && name != "pwsh" {
		return gcAliasPrologue
	}
	return plainTextPrologue + gcAliasPrologue
}

// scriptMustLead reports whether command opens, after any blank lines and
// comments, with a statement PowerShell accepts only at the start of a
// script: a using statement, a param block, or the attribute that precedes
// one.
func scriptMustLead(command string) bool {
	position, incomplete := skipScriptLeadTrivia(command, 0)
	if incomplete {
		return true
	}
	if scriptLeadKeyword(command[position:], "using") || scriptLeadKeyword(command[position:], "param") {
		return true
	}
	if position >= len(command) || command[position] != '[' {
		return false
	}
	for position < len(command) && command[position] == '[' {
		next, ok := skipScriptAttribute(command, position)
		if !ok {
			return false
		}
		position, incomplete = skipScriptLeadTrivia(command, next)
		if incomplete {
			return true
		}
	}
	return scriptLeadKeyword(command[position:], "param")
}

func skipScriptLeadTrivia(command string, position int) (int, bool) {
	for position < len(command) {
		switch command[position] {
		case ' ', '\t', '\r', '\n':
			position++
		case '<':
			if position+1 >= len(command) || command[position+1] != '#' {
				return position, false
			}
			end := strings.Index(command[position+2:], "#>")
			if end < 0 {
				return len(command), true
			}
			position += end + 4
		case '#':
			end := strings.IndexByte(command[position:], '\n')
			if end < 0 {
				return len(command), false
			}
			position += end + 1
		default:
			return position, false
		}
	}
	return position, false
}

func scriptLeadKeyword(rest string, keyword string) bool {
	if len(rest) < len(keyword) || !strings.EqualFold(rest[:len(keyword)], keyword) {
		return false
	}
	if len(rest) == len(keyword) {
		return keyword == "param"
	}
	switch rest[len(keyword)] {
	case ' ', '\t', '\r', '\n':
		return true
	case '(':
		return keyword == "param"
	default:
		return false
	}
}

func skipScriptAttribute(command string, position int) (int, bool) {
	depth := 0
	for position < len(command) {
		switch command[position] {
		case '\'', '"':
			next, ok := skipScriptString(command, position)
			if !ok {
				return len(command), false
			}
			position = next
			continue
		case '`':
			position++
			if position < len(command) {
				position++
			}
			continue
		case '[':
			depth++
		case ']':
			depth--
			position++
			if depth == 0 {
				return position, true
			}
			continue
		}
		position++
	}
	return len(command), false
}

func skipScriptString(command string, position int) (int, bool) {
	quote := command[position]
	position++
	for position < len(command) {
		switch command[position] {
		case '`':
			if quote == '"' {
				position++
				if position < len(command) {
					position++
				}
			} else {
				position++
			}
		case '\'':
			if quote == '\'' && position+1 < len(command) && command[position+1] == '\'' {
				position += 2
				continue
			}
			if quote == '\'' {
				return position + 1, true
			}
			position++
		case '"':
			if quote == '"' {
				return position + 1, true
			}
			position++
		default:
			position++
		}
	}
	return len(command), false
}

// commandLineLength is the length, in UTF-16 code units, of the command line
// exec builds from argv: each argument escaped by the CreateProcess argv rules,
// joined by single spaces.
func commandLineLength(argv []string) int {
	n := len(argv) - 1
	for _, arg := range argv {
		n += len(utf16.Encode([]rune(syscall.EscapeArg(arg))))
	}
	return n
}

// PromptLaunch is a command rendered as one statement to type at an
// interactive PowerShell prompt; see [ShellCommandPrompt].
type PromptLaunch struct {
	// Statement is the line to type.
	Statement string
	// Interpreter is the child interpreter the statement starts.
	Interpreter string
	// EncodedCommand is the child's -EncodedCommand argument. It appears
	// verbatim in the child's command line, so it identifies the process this
	// statement started.
	EncodedCommand string
}

// ShellCommandPrompt renders command as one PowerShell statement to type at an
// interactive PowerShell prompt, such as a terminal pane's shell. The statement
// runs command the way [ShellCommandContext] does, in a child interpreter, and
// then exits the prompt's shell with the command's status, so a pane that
// exists to run the command closes when it ends.
//
// The script travels base64-encoded (-EncodedCommand), unlike
// [ShellCommandContext]'s. A statement typed at a prompt is re-quoted by that
// prompt before the interpreter sees it, and a literal script does not survive
// that (measured): Windows PowerShell 5.1 silently drops its double quotes, so
// `'he said "hi"'` arrives as `he said hi` and the command still succeeds, and
// pwsh failed to parse one carrying doubled quotes. A base64 token carries no
// quotes. The child writes to the prompt's console rather than to a redirected
// stream, so the CLIXML -EncodedCommand writes to a redirected stderr does not
// arise.
//
// The exit runs in finally, and reads a $LASTEXITCODE the statement cleared
// first, because the prompt is the user's shell and runs with the user's
// preferences:
//   - pwsh with $PSNativeCommandUseErrorActionPreference and
//     $ErrorActionPreference = 'Stop' turns the child's non-zero exit into an
//     error that ends the statement, and a plain `; exit` after it never ran:
//     the prompt survived its command (measured).
//   - A child that could not be started leaves $LASTEXITCODE as the prompt's
//     last native program set it, which a profile commonly leaves at 0.
//
// -NonInteractive is left off: the command has the prompt's console and, as
// under `sh -c` in a terminal, may ask it for input.
func ShellCommandPrompt(command string) (PromptLaunch, error) {
	shell, err := findPowerShell()
	if err != nil {
		return PromptLaunch{}, err
	}
	return shellCommandPromptWith(shell, command)
}

// shellCommandPromptWith renders the statement with shell as the child
// interpreter.
func shellCommandPromptWith(shell, command string) (PromptLaunch, error) {
	script := promptScript(command)
	if strings.IndexByte(script, 0) >= 0 {
		return PromptLaunch{}, errors.New("command contains a NUL character, which a Windows command line cannot carry")
	}
	encoded := encodePowerShellCommand(script)
	if n := promptChildCommandLineLength(shell, encoded); n >= commandLineLimit {
		return PromptLaunch{}, fmt.Errorf("command does not fit a Windows command line: encoded for a prompt it needs %d characters and at most %d are allowed; move it into a script file and run that file instead", n, commandLineLimit-1)
	}
	return PromptLaunch{
		Statement: "$global:LASTEXITCODE = $null; try { & " + psSingleQuoted(shell) + " -NoProfile -EncodedCommand " + encoded +
			" } finally { if ($global:LASTEXITCODE -is [int]) { exit $global:LASTEXITCODE }; exit 1 }",
		Interpreter:    shell,
		EncodedCommand: encoded,
	}, nil
}

// promptScript is the script a prompt's statement hands its child: command,
// after gcAliasPrologue unless command must lead, then shellExitEpilogue. The
// child draws on the prompt's console, where pwsh's colors belong, so it gets
// no plainTextPrologue.
func promptScript(command string) string {
	if scriptMustLead(command) {
		return command + shellExitEpilogue
	}
	return gcAliasPrologue + command + shellExitEpilogue
}

// promptChildCommandLineLength is the length, in UTF-16 code units, of the
// command line the prompt's PowerShell builds to start the child. It starts
// native programs through .NET, which quotes the program path whether or not it
// needs quoting.
func promptChildCommandLineLength(shell, encoded string) int {
	return len(utf16.Encode([]rune(`"`+shell+`"`))) + 1 + commandLineLength([]string{"-NoProfile", "-EncodedCommand", encoded})
}

// encodePowerShellCommand renders script as the argument to -EncodedCommand:
// base64 of UTF-16LE.
func encodePowerShellCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		buf = append(buf, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// psSingleQuoted renders s as a PowerShell single-quoted string. PowerShell
// takes the typographic single quotes as quote characters too, so each of them
// is doubled along with the ASCII one.
func psSingleQuoted(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201A', '\u201B':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

func shellCommandContext(ctx context.Context, command string) (*exec.Cmd, error) {
	shell, args, err := powerShellInvocation(command)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, shell, args...), nil
}

// shellQuote is psSingleQuoted. Nothing inside a PowerShell single-quoted
// string is special except a quote character, which psSingleQuoted doubles.
func shellQuote(arg string) string {
	return psSingleQuoted(arg)
}

func shellCommand(command string) (*exec.Cmd, error) {
	shell, args, err := powerShellInvocation(command)
	if err != nil {
		return nil, err
	}
	return exec.Command(shell, args...), nil
}

// shellSessionCommand starts the interpreter with no script, so it reads its
// stdin and exits at EOF. That mirrors what an empty session command does on
// Unix, and it avoids nesting a second interpreter inside the first.
func shellSessionCommand() (*exec.Cmd, error) {
	shell, err := findPowerShell()
	if err != nil {
		return nil, err
	}
	return exec.Command(shell, "-NoProfile", "-NonInteractive"), nil
}
