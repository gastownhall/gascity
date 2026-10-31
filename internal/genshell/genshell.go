// Package genshell runs POSIX shell programs of the restricted forms Gas City
// generates, inside the calling process and without an external shell.
//
// A read program is a command of the form
//
//	[NAME=value ...] sh -c SCRIPT [NAME [ARG ...]]
//
// and a recovery hook is a script given whole. Either is interpreted exactly
// as written by an embedded POSIX interpreter (mvdan.cc/sh). jq runs in
// process (github.com/itchyny/gojq, see jq.go), as do awk and xargs in the
// one form each that the hooks use (see shims.go). bd and gc are the only
// programs a script may start, and they start through the caller's ExecFunc.
// Any other command fails with status 127 and starts nothing.
package genshell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// ErrUnsupported reports a command that is not of the form Parse accepts.
var ErrUnsupported = errors.New("not an sh -c program of literal words")

// Program is a command of the form
//
//	[NAME=value ...] sh -c SCRIPT [NAME [ARG ...]]
//
// split into its parts.
type Program struct {
	// Assigns are the NAME=value assignments before sh, in order. They are
	// added to the script's environment.
	Assigns []string
	// Script is the text sh runs.
	Script string
	// Name is the script's $0; "" means "sh".
	Name string
	// Args are the script's positional parameters, $1 onward.
	Args []string
}

// Parse splits command, which must be one simple command of the form
//
//	[NAME=value ...] sh -c SCRIPT [NAME [ARG ...]]
//
// in which every word is literal text: plain, single-quoted or double-quoted,
// with nothing to expand, substitute or glob, and no redirection. Without a
// NAME, $0 is "sh". Any other command returns an error wrapping ErrUnsupported.
func Parse(command string) (Program, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return Program{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if len(file.Stmts) != 1 {
		return Program{}, fmt.Errorf("%w: %d commands, want 1", ErrUnsupported, len(file.Stmts))
	}
	stmt := file.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown || len(stmt.Redirs) > 0 {
		return Program{}, fmt.Errorf("%w: not a simple command", ErrUnsupported)
	}

	var prog Program
	for _, assign := range call.Assigns {
		if assign.Name == nil || assign.Append || assign.Naked || assign.Index != nil || assign.Array != nil {
			return Program{}, fmt.Errorf("%w: assignment is not NAME=value", ErrUnsupported)
		}
		value, err := literal(assign.Value)
		if err != nil {
			return Program{}, err
		}
		prog.Assigns = append(prog.Assigns, assign.Name.Value+"="+value)
	}

	args := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		arg, err := literal(word)
		if err != nil {
			return Program{}, err
		}
		args = append(args, arg)
	}
	if len(args) < 3 || args[0] != "sh" || args[1] != "-c" {
		return Program{}, fmt.Errorf("%w: not sh -c SCRIPT", ErrUnsupported)
	}
	prog.Script = args[2]
	prog.Name = "sh"
	if len(args) > 3 {
		prog.Name = args[3]
		prog.Args = args[4:]
	}
	return prog, nil
}

// literal returns the text word stands for when reading it takes quote
// removal and nothing else, and an error wrapping ErrUnsupported otherwise.
func literal(word *syntax.Word) (string, error) {
	if word == nil {
		return "", nil
	}
	var text strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			unquoted, ok := unquotedLiteral(part.Value)
			if !ok {
				return "", fmt.Errorf("%w: %q would be expanded", ErrUnsupported, part.Value)
			}
			text.WriteString(unquoted)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", fmt.Errorf("%w: $'...' quoting", ErrUnsupported)
			}
			text.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return "", fmt.Errorf("%w: $\"...\" quoting", ErrUnsupported)
			}
			for _, inner := range part.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", fmt.Errorf("%w: expansion inside double quotes", ErrUnsupported)
				}
				text.WriteString(doubleQuotedLiteral(lit.Value))
			}
		default:
			return "", fmt.Errorf("%w: word needs expansion", ErrUnsupported)
		}
	}
	return text.String(), nil
}

// unquotedLiteral removes the backslashes from an unquoted literal. It
// reports false when the literal has an unescaped glob, tilde or brace
// character, which a shell would expand.
func unquotedLiteral(text string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case '\\':
			i++
			if i == len(text) {
				return "", false
			}
			c = text[i]
		case '*', '?', '[', ']', '~', '{', '}':
			return "", false
		}
		out.WriteByte(c)
	}
	return out.String(), true
}

// doubleQuotedLiteral removes the backslashes that escape $, `, " and \
// inside double quotes. Any other backslash stands for itself there.
func doubleQuotedLiteral(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' && i+1 < len(text) {
			switch text[i+1] {
			case '$', '`', '"', '\\':
				i++
				c = text[i]
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

// Process is one run of bd or gc that a script asks for.
type Process struct {
	// Args are the program's name, as the script wrote it, and its arguments.
	Args []string
	// Dir is the script's working directory.
	Dir string
	// Env is the script's exported environment.
	Env []string
	// Stdin is the program's standard input, or nil for none.
	Stdin io.Reader
	// Stdout and Stderr receive the program's output.
	Stdout io.Writer
	Stderr io.Writer
}

// ExecFunc runs p to completion. It returns nil when p exits with status 0,
// an error with an ExitCode() int method (such as *exec.ExitError) when p
// exits with another status, and any other error when p could not run, which
// the script sees as status 127 with the error on its standard error.
type ExecFunc func(ctx context.Context, p Process) error

// LookPath returns the path of the executable p names, searched for in the
// PATH of p.Env from p.Dir as a shell would. On Windows a name without an
// extension matches a file with one of the extensions in PATHEXT.
func LookPath(p Process) (string, error) {
	if len(p.Args) == 0 {
		return "", errors.New("no program name")
	}
	return interp.LookPathDir(p.Dir, expand.ListEnviron(p.Env...), p.Args[0])
}

// Options configure Program.Run.
type Options struct {
	// Dir is the script's working directory; "" means the current directory.
	Dir string
	// Env is the script's environment as NAME=value pairs. Nil means the
	// environment of the current process.
	Env []string
	// Stdin is the script's standard input; nil means none.
	Stdin io.Reader
	// Stdout and Stderr receive the script's output; nil discards it.
	Stdout io.Writer
	Stderr io.Writer
	// Exec runs the bd and gc processes the script starts. Nil refuses them
	// with status 127.
	Exec ExecFunc
}

// ExitError reports that a script exited with a status other than 0.
type ExitError struct {
	Status int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Status) }

// ExitCode returns the script's exit status.
func (e *ExitError) ExitCode() int { return e.Status }

// Run runs p's script with p.Assigns added to opts.Env, p.Name as $0 and
// p.Args as $1 onward. It returns nil when the script exits with status 0, an
// *ExitError for any other status, and any other error when the script did not
// run to an exit status: it could not be parsed, or ctx was done.
//
// The script is read as bash, the interpreter's best-supported language and a
// superset of what both dash and bash accept as sh. Read as POSIX, the
// interpreter would refuse export and readonly.
func (p Program) Run(ctx context.Context, opts Options) error {
	name := p.Name
	if name == "" {
		name = "sh"
	}
	baseEnv := opts.Env
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	env := newScriptEnviron(baseEnv, p.Assigns)
	file, err := parseScript(p.Script, name)
	if err != nil {
		return err
	}
	var exportAliases map[string]string
	stdout, stderr := opts.Stdout, opts.Stderr
	if runtime.GOOS == "windows" {
		exportAliases = rewriteCaseConflictingBindings(file, env)
		stdout, stderr = lockedOutputWriters(opts.Stdout, opts.Stderr)
	}
	runner, err := interp.New(
		interp.Env(env),
		interp.Dir(opts.Dir),
		interp.StdIO(opts.Stdin, stdout, stderr),
		interp.Params(append([]string{"--"}, p.Args...)...),
		interp.ExecHandlers(execHandler(opts.Exec, exportAliases)),
	)
	if err != nil {
		return fmt.Errorf("starting interpreter: %w", err)
	}
	err = runner.Run(ctx, file)
	var status interp.ExitStatus
	if errors.As(err, &status) {
		return &ExitError{Status: int(status)}
	}
	return err
}

func lockedOutputWriters(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	var mu sync.Mutex
	if stdout != nil {
		stdout = lockedWriter{mu: &mu, w: stdout}
	}
	if stderr != nil {
		stderr = lockedWriter{mu: &mu, w: stderr}
	}
	return stdout, stderr
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// parseScript parses script, named name in errors, as Run reads it.
func parseScript(script, name string) (*syntax.File, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), name)
	if err != nil {
		return nil, fmt.Errorf("parsing script: %w", err)
	}
	return file, nil
}

// execHandler runs every command the interpreter does not run itself: jq, and
// awk and xargs in the one form each that generated programs use, in process;
// bd and gc through run; and nothing else.
func execHandler(run ExecFunc, exportAliases map[string]string) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			switch args[0] {
			case "jq":
				return toolStatus(ctx, runJQ(ctx, args[1:], hc.Stdin, hc.Stdout, hc.Stderr, exportedEnv(hc.Env, exportAliases)))
			case "awk":
				return toolStatus(ctx, runAwk(args[1:], hc.Stdin, hc.Stdout, hc.Stderr))
			case "xargs":
				return runXargs(ctx, args[1:], hc, run, exportAliases)
			case "bd", "gc":
				if run == nil {
					fmt.Fprintf(hc.Stderr, "%s: not run: no program runner\n", args[0]) //nolint:errcheck // best-effort stderr
					return interp.ExitStatus(127)
				}
				return scriptStatus(ctx, hc.Stderr, run(ctx, Process{
					Args:   args,
					Dir:    hc.Dir,
					Env:    exportedEnv(hc.Env, exportAliases),
					Stdin:  hc.Stdin,
					Stdout: hc.Stdout,
					Stderr: hc.Stderr,
				}))
			}
			fmt.Fprintf(hc.Stderr, "%s: not run: a generated program may run only bd, gc, jq, and awk and xargs in their generated forms\n", args[0]) //nolint:errcheck // best-effort stderr
			return interp.ExitStatus(127)
		}
	}
}

// toolStatus converts the status of a tool run in process into the result the
// interpreter expects.
func toolStatus(ctx context.Context, status uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status != 0 {
		return interp.ExitStatus(status)
	}
	return nil
}

// scriptStatus converts an ExecFunc's error into the result the interpreter
// expects: ctx's error once ctx is done, the process's exit status when it
// has one, and otherwise status 127 with err on stderr.
func scriptStatus(ctx context.Context, stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return interp.ExitStatus(statusByte(exit.ExitCode()))
	}
	fmt.Fprintln(stderr, err) //nolint:errcheck // best-effort stderr
	return interp.ExitStatus(127)
}

// statusByte maps a failed process's exit code onto the 1-255 range a shell
// reports: a code outside 0-255, such as a Windows NTSTATUS, is 255, and a
// failure claiming code 0 is 1, so neither reads as success.
func statusByte(code int) uint8 {
	switch {
	case code == 0:
		return 1
	case code < 0 || code > 255:
		return 255
	default:
		return uint8(code)
	}
}

// exportedEnv returns the exported variables of env as NAME=value pairs, as
// the interpreter passes them to a program it starts.
func exportedEnv(env expand.Environ, aliases map[string]string) []string {
	aliasExports := map[string]struct{}{}
	for name, vr := range env.Each {
		original, ok := aliases[name]
		if !ok || !vr.IsSet() || !vr.Exported || vr.Kind != expand.String {
			continue
		}
		aliasExports[original] = struct{}{}
	}

	list := make([]string, 0, 64)
	for name, vr := range env.Each {
		if original, ok := aliases[name]; ok {
			name = original
		} else if runtime.GOOS == "windows" && exportedAliasShadows(name, aliasExports) {
			continue
		}
		if !vr.IsSet() {
			list = removeEnvEntry(list, name)
			continue
		}
		if vr.Exported && vr.Kind == expand.String {
			list = setEnvEntry(list, name, vr.String())
		}
	}
	return list
}

func exportedAliasShadows(name string, aliases map[string]struct{}) bool {
	for alias := range aliases {
		if strings.EqualFold(alias, name) {
			return true
		}
	}
	return false
}

func setEnvEntry(list []string, name, value string) []string {
	entry := name + "=" + value
	for i, kv := range list {
		key, _, ok := strings.Cut(kv, "=")
		if ok && envNamesEqual(key, name) {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}

func removeEnvEntry(list []string, name string) []string {
	return slices.DeleteFunc(list, func(kv string) bool {
		key, _, ok := strings.Cut(kv, "=")
		return ok && envNamesEqual(key, name)
	})
}

func envNamesEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
