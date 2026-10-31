//go:build windows

package main

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/genshell"
	"github.com/gastownhall/gascity/internal/processgroup"
)

// generatedProgram returns the runner for command when it is a program this
// process generated (config.GeneratedProgram): a read program, which is the
// default work query in any of its forms or the default pool-demand check, or
// a default on_boot or on_death recovery hook. Otherwise it returns nil.
//
// Those programs are POSIX shell, and Windows has none. They run in gc's
// embedded interpreter (internal/genshell), exactly as generated, and start no
// program but bd and gc (runGeneratedProcess). Every other command keeps the
// platform's interpreter, PowerShell.
func generatedProgram(command string) generatedProgramRunner {
	prog, ok := config.GeneratedProgram(command)
	if !ok {
		return nil
	}
	return func(ctx context.Context, dir string, env []string, stderr io.Writer) ([]byte, error) {
		processes := &generatedProcessSet{}
		var stdout bytes.Buffer
		err := prog.Run(ctx, genshell.Options{
			Dir:    dir,
			Env:    env,
			Stdout: &stdout,
			Stderr: stderr,
			Exec:   processes.run,
		})
		processes.finish(ctx.Err() != nil)
		return stdout.Bytes(), err
	}
}

// generatedProgramWith is generatedProgram with run starting the bd and gc
// processes the program asks for.
func generatedProgramWith(command string, run genshell.ExecFunc) generatedProgramRunner {
	prog, ok := config.GeneratedProgram(command)
	if !ok {
		return nil
	}
	return func(ctx context.Context, dir string, env []string, stderr io.Writer) ([]byte, error) {
		execRun := run
		if execRun == nil {
			execRun = runGeneratedProcess
		}
		var stdout bytes.Buffer
		err := prog.Run(ctx, genshell.Options{
			Dir:    dir,
			Env:    env,
			Stdout: &stdout,
			Stderr: stderr,
			Exec:   execRun,
		})
		return stdout.Bytes(), err
	}
}

type generatedProcessSet struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

func (s *generatedProcessSet) run(ctx context.Context, p genshell.Process) error {
	path, err := genshell.LookPath(p)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, p.Args[1:]...)
	cmd.Dir = p.Dir
	cmd.Env = p.Env
	cmd.Stdin = p.Stdin
	cmd.Stdout = p.Stdout
	cmd.Stderr = p.Stderr
	cmd.WaitDelay = generatedProcessWaitDelay
	var canceled atomic.Bool
	cmd.Cancel = func() error {
		canceled.Store(true)
		return processgroup.KillCommandGroup(cmd, 0, processgroup.Options{})
	}
	if _, err := processgroup.StartCommand(cmd, processgroup.SurviveRelease); err != nil {
		return err
	}
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	err = cmd.Wait()
	if canceled.Load() {
		return err
	}
	return err
}

func (s *generatedProcessSet) finish(canceled bool) {
	s.mu.Lock()
	cmds := append([]*exec.Cmd(nil), s.cmds...)
	s.mu.Unlock()
	if canceled {
		for _, cmd := range cmds {
			_ = processgroup.KillCommandConsoleGroup(cmd, 0, processgroup.Options{})
		}
	}
	for _, cmd := range cmds {
		_ = processgroup.ReleaseCommand(cmd)
	}
}

// generatedProcessWaitDelay bounds how long a bd or gc that has exited may
// keep a generated program's output open through a process it left running.
const generatedProcessWaitDelay = 2 * time.Second

// runGeneratedProcess runs one bd or gc for a generated program, found on the
// program's PATH, inside a Job Object that owns its tree
// (processgroup.StartCommand). A command still running when its own context is
// canceled is reaped as a whole tree. Completed commands are cleaned up by the
// generated program's process set, which releases descendants that deliberately
// detached into another process group.
func runGeneratedProcess(ctx context.Context, p genshell.Process) error {
	processes := &generatedProcessSet{}
	err := processes.run(ctx, p)
	processes.finish(ctx.Err() != nil)
	return err
}
