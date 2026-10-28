package genshell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// awkDedupeProgram is the one awk program a script may run. It prints each
// line that has a field and has not been printed before, in input order.
const awkDedupeProgram = "NF && !seen[$0]++"

// runAwk runs `awk 'NF && !seen[$0]++'` over stdin, and refuses every other
// use of awk with status 127. As in awk, a line of blanks has no fields, a
// line is its full text, and a last line without a newline still counts.
func runAwk(args []string, stdin io.Reader, stdout, stderr io.Writer) uint8 {
	if len(args) != 1 || args[0] != awkDedupeProgram {
		fmt.Fprintf(stderr, "awk: not run: a generated program may run only awk '%s'\n", awkDedupeProgram) //nolint:errcheck // best-effort stderr
		return 127
	}
	seen := make(map[string]bool)
	err := eachLine(stdin, func(line string) error {
		if strings.Trim(line, " \t") == "" || seen[line] {
			return nil
		}
		seen[line] = true
		_, err := io.WriteString(stdout, line+"\n")
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "awk: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	return 0
}

// The exit statuses xargs reports.
const (
	xargsStatusFailed  = 123 // a run exited with a status other than 0 or 255
	xargsStatusStopped = 124 // a run exited with status 255, so no more ran
	xargsStatusNotRun  = 126 // a run could not start
)

// runXargs runs `xargs -rI{} sh -c SCRIPT NAME {}`: SCRIPT once for each line
// of stdin, in order, as a nested script with NAME as $0 and the line as $1,
// with the exported environment, no standard input and exec as its ExecFunc.
// It refuses every other use of xargs with status 127, as it does a SCRIPT that
// does not parse or a SCRIPT or NAME that holds {}, which xargs would replace.
//
// As with -I, blanks at the start of a line are dropped and an empty line
// runs nothing; with -r, no input runs nothing. xargs would read quotes and
// backslashes in a line, and its implementations disagree on a blank at the
// end of one, so a line with any of them runs nothing and stops the run with
// status 1, as a quote xargs cannot match stops GNU xargs.
//
// The status is xargs's: 123 once a run exits with a status other than 0,
// after the remaining lines run; 124 when a run exits with 255, which stops
// the run; and 126 when a run cannot start, which stops it too.
func runXargs(ctx context.Context, args []string, hc interp.HandlerContext, exec ExecFunc, exportAliases map[string]string) error {
	if len(args) != 6 || args[0] != "-rI{}" || args[1] != "sh" || args[2] != "-c" || args[5] != "{}" ||
		strings.Contains(args[3], "{}") || strings.Contains(args[4], "{}") {
		fmt.Fprintln(hc.Stderr, "xargs: not run: a generated program may run only xargs -rI{} sh -c SCRIPT NAME {}") //nolint:errcheck // best-effort stderr
		return interp.ExitStatus(127)
	}
	script, name := args[3], args[4]
	if _, err := parseScript(script, name); err != nil {
		fmt.Fprintf(hc.Stderr, "xargs: not run: %v\n", err) //nolint:errcheck // best-effort stderr
		return interp.ExitStatus(127)
	}
	env := exportedEnv(hc.Env, exportAliases)
	var status uint8
	errStop := errors.New("stop")
	err := eachLine(hc.Stdin, func(line string) error {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			return nil
		}
		if strings.ContainsAny(line, `'"\`) || strings.TrimRight(line, " \t") != line {
			fmt.Fprintf(hc.Stderr, "xargs: not run: input line %q needs quote or blank handling\n", line) //nolint:errcheck // best-effort stderr
			status = 1
			return errStop
		}
		err := Program{Script: script, Name: name, Args: []string{line}}.Run(ctx, Options{
			Dir:    hc.Dir,
			Env:    env,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
			Exec:   exec,
		})
		var exit *ExitError
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(err, &exit) && exit.Status == 255:
			fmt.Fprintln(hc.Stderr, "xargs: sh: exited with status 255; aborting") //nolint:errcheck // best-effort stderr
			status = xargsStatusStopped
			return errStop
		case errors.As(err, &exit):
			status = xargsStatusFailed
			return nil
		default:
			fmt.Fprintf(hc.Stderr, "xargs: sh: %v\n", err) //nolint:errcheck // best-effort stderr
			status = xargsStatusNotRun
			return errStop
		}
	})
	if err != nil && !errors.Is(err, errStop) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		fmt.Fprintf(hc.Stderr, "xargs: %v\n", err) //nolint:errcheck // best-effort stderr
		return interp.ExitStatus(1)
	}
	if status != 0 {
		return interp.ExitStatus(status)
	}
	return nil
}

// eachLine calls fn with each line of r, without its newline, until r ends or
// fn returns an error. A nil r has no lines.
func eachLine(r io.Reader, fn func(line string) error) error {
	if r == nil {
		return nil
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if fnErr := fn(strings.TrimSuffix(line, "\n")); fnErr != nil {
				return fnErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
