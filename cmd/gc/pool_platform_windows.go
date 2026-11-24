//go:build windows

package main

import (
	"bytes"
	"errors"
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// posixEnvPrefix reports whether prefixShellEnv renders its POSIX
// `KEY='value' command` prefix. PowerShell, the interpreter here, has no such
// syntax.
const posixEnvPrefix = false

// generatedHookUnavailable reports why this platform cannot run command, an
// agent's generated on_boot or on_death recovery hook as it stands after
// template expansion, or "" when it can.
//
// The hooks are POSIX shell programs, and PowerShell does not reject all of
// them: the generated on_boot parses, and runs awk and xargs from PATH over its
// own text. So a hook runs only in gc's embedded interpreter, which runs it
// only exactly as gc generated it (generatedProgram), and a hook that is not
// exactly that is not run at all. An on_boot or on_death the agent sets itself
// runs as usual.
func generatedHookUnavailable(command string) string {
	if generatedProgram(command) != nil {
		return ""
	}
	return "the default hook is a POSIX shell program that no longer reads as Gas City generated it, and Gas City runs no POSIX shell on Windows"
}

// runPreparedCommand applies prepare, when it is non-nil, and runs cmd to
// completion, returning its standard output as cmd.Output does.
//
// A prepared command starts inside a Job Object that owns its whole tree
// (processgroup.StartCommand). prepare opts into a cancellation policy that
// reaches past the command's own process, and on Windows only an owned tree
// can be terminated: a console process group routes Ctrl+Break and contains
// nothing. The tree survives release, as a Unix process group survives its
// leader's exit, so a process the command left behind keeps running once the
// command has been waited for.
func runPreparedCommand(cmd *exec.Cmd, prepare func(*exec.Cmd)) ([]byte, error) {
	if prepare == nil {
		return cmd.Output()
	}
	prepare(cmd)
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr *limitedBuffer
	if cmd.Stderr == nil {
		stderr = &limitedBuffer{limit: preparedStderrLimit}
		cmd.Stderr = stderr
	}
	if _, err := processgroup.StartCommand(cmd, processgroup.SurviveRelease); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	// Its only failure is closing the job handle, which leaves nothing to act on.
	_ = processgroup.ReleaseCommand(cmd)
	var exitErr *exec.ExitError
	if stderr != nil && errors.As(err, &exitErr) {
		exitErr.Stderr = stderr.buf.Bytes()
	}
	return stdout.Bytes(), err
}
