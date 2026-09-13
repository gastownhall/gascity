//go:build !windows

package orders

import (
	"os/exec"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

var conditionProcessGroupOptions processgroup.Options

func prepareConditionCommand(cmd *exec.Cmd, cleanupTimeout time.Duration) func() error {
	processgroup.StartCommandInNewGroup(cmd)
	var cleanupMu sync.Mutex
	var cleanupErr error
	var cleanupOnce sync.Once
	cleanup := func() error {
		cleanupOnce.Do(func() {
			knownPGID := 0
			if cmd != nil && cmd.Process != nil {
				knownPGID = cmd.Process.Pid
			}
			err := processgroup.TerminateCommand(cmd, knownPGID, cleanupTimeout, conditionProcessGroupOptions)
			cleanupMu.Lock()
			cleanupErr = err
			cleanupMu.Unlock()
		})
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		return cleanupErr
	}
	cmd.Cancel = func() error {
		_ = cleanup()
		return nil
	}
	return cleanup
}
