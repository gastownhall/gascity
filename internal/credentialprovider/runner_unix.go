//go:build !windows

package credentialprovider

import (
	"os/exec"
	"sync"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func newCommandControl(cmd *exec.Cmd) (*commandControl, error) {
	processgroup.StartCommandInNewGroup(cmd)
	var cleanupOnce sync.Once
	var cleanupErr error
	cleanup := func() error {
		cleanupOnce.Do(func() {
			knownProcessGroup := 0
			if cmd != nil && cmd.Process != nil {
				knownProcessGroup = cmd.Process.Pid
			}
			cleanupErr = processgroup.TerminateCommand(cmd, knownProcessGroup, commandKillGrace, processgroup.Options{})
		})
		return cleanupErr
	}
	return &commandControl{
		afterStart: func() error { return nil },
		cancel:     cleanup,
		close:      cleanup,
	}, nil
}
