//go:build windows

package config

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/genshell"
	"github.com/gastownhall/gascity/internal/genshell/genshelltest"
)

// runShellCommandInProcess runs command as `sh -c command` would, in the
// embedded interpreter that runs generated programs on Windows
// (internal/genshell), and reports its streams and exit status. There is no
// POSIX shell here to hand the command to, so it always answers: ok is true.
//
// A test's fake bd and gc are #!/bin/sh scripts on PATH. They run in the same
// interpreter, and so may use only what a generated program may. A command
// that is not `sh -c SCRIPT [NAME [ARG ...]]` runs as a script itself, as
// `sh -c command` would run it.
func runShellCommandInProcess(t *testing.T, command string, env []string) (stdout, stderr string, exit int, ok bool) {
	t.Helper()
	prog, err := genshell.Parse(command)
	if err != nil {
		prog = genshell.Program{Script: command}
	}
	var outBuf, errBuf strings.Builder
	err = prog.Run(context.Background(), genshell.Options{
		Env:    env,
		Stdout: &outBuf,
		Stderr: &errBuf,
		Exec:   genshelltest.ScriptFakes,
	})
	var exitErr *genshell.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit = exitErr.Status
	default:
		t.Fatalf("run shell command: %v", err)
	}
	return outBuf.String(), errBuf.String(), exit, true
}
