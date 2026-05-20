//go:build windows

package orders

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

func prepareConditionCommand(cmd *exec.Cmd, _ time.Duration) func() error {
	var cleanupMu sync.Mutex
	var cleanupErr error
	var cleanupOnce sync.Once
	cleanup := func() error {
		cleanupOnce.Do(func() {
			if cmd == nil || cmd.Process == nil {
				return
			}
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				cleanupMu.Lock()
				cleanupErr = err
				cleanupMu.Unlock()
			}
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
