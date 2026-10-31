//go:build windows

package main

import (
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func prepareProviderOpCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

type providerOpProcessSet struct {
	mu   sync.Mutex
	cmds []*exec.Cmd
}

func (s *providerOpProcessSet) run(cmd *exec.Cmd) error {
	if s == nil {
		prepareProviderOpCommand(cmd)
		return cmd.Run()
	}
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
	err := cmd.Wait()
	if canceled.Load() {
		return err
	}
	return err
}

func (s *providerOpProcessSet) finish(canceled bool) {
	if s == nil {
		return
	}
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

// runProviderOpCommand runs cmd in a Job Object and, when its context ends,
// terminates the job: cmd.Process is the command interpreter, and the program
// it runs is its child, which killing the interpreter alone leaves running.
// The Unix process group gives the same reach. The job is released once cmd
// exits, so what it left running on purpose survives, as there.
func runProviderOpCommand(cmd *exec.Cmd) error {
	cmd.Cancel = func() error {
		return processgroup.KillCommandGroup(cmd, 0, processgroup.Options{})
	}
	if _, err := processgroup.StartCommand(cmd, processgroup.SurviveRelease); err != nil {
		return err
	}
	err := cmd.Wait()
	// Its only failure is closing the job handle, which leaves nothing to act on.
	_ = processgroup.ReleaseCommand(cmd)
	return err
}
