//go:build windows

package orders

import (
	"os/exec"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// prepareConditionCommand makes canceling cmd terminate its whole tree, and
// returns the same cleanup for the caller to run itself. The tree is owned,
// and so reachable, only when startConditionCommand starts the command.
func prepareConditionCommand(cmd *exec.Cmd, cleanupTimeout time.Duration) func() error {
	var cleanupMu sync.Mutex
	var cleanupErr error
	var cleanupOnce sync.Once
	cleanup := func() error {
		cleanupOnce.Do(func() {
			err := processgroup.TerminateCommand(cmd, 0, cleanupTimeout, processgroup.Options{})
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

// startConditionCommand starts a condition check inside a Job Object that
// owns its whole tree, which on Windows is the only boundary a timed-out
// check's cleanup can terminate. The tree survives release, as a Unix process
// group survives its leader's exit.
func startConditionCommand(cmd *exec.Cmd) error {
	_, err := processgroup.StartCommand(cmd, processgroup.SurviveRelease)
	return err
}

// releaseConditionCommand ends the launcher's hold on a finished check's
// tree. Its only failure is closing the job handle, which leaves nothing to
// act on.
func releaseConditionCommand(cmd *exec.Cmd) {
	_ = processgroup.ReleaseCommand(cmd)
}
