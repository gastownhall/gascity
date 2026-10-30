package acceptancehelpers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

const acceptanceToolCommandTimeout = 60 * time.Second

func boundedToolCommand(t *testing.T, timeout time.Duration, path string, args ...string) (*exec.Cmd, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, path, args...)
	prepareBoundedToolCommand(cmd)
	cmd.Cancel = func() error { return terminateBoundedToolCommand(cmd) }
	cmd.WaitDelay = time.Second
	t.Cleanup(func() {
		defer cancel()
		if cmd.Process == nil {
			return
		}
		var err error
		if cmd.ProcessState == nil {
			stopped := make(chan error, 1)
			go func() { stopped <- terminateBoundedToolCommand(cmd) }()
			_ = cmd.Wait()
			err = <-stopped
		} else {
			err = terminateBoundedToolCommand(cmd)
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Logf("stop descendants left by %s: %v", path, err)
		}
	})
	return cmd, ctx
}
