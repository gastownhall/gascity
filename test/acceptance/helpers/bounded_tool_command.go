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
		if cmd.Process == nil {
			cancel()
			return
		}
		if cmd.ProcessState == nil {
			cleanupResult := make(chan error, 1)
			go func() { cleanupResult <- cleanupBoundedToolCommand(cmd) }()
			_ = cmd.Wait()
			err := <-cleanupResult
			cancel()
			if err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("stop descendants left by %s: %v", path, err)
			}
			return
		}
		cancel()
		if err := cleanupBoundedToolCommand(cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop descendants left by %s: %v", path, err)
		}
	})
	return cmd, ctx
}
