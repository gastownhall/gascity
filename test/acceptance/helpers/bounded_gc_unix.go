//go:build unix

package acceptancehelpers

import (
	"bytes"
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

// BoundedGCRun is the outcome of RunGCBounded.
type BoundedGCRun struct {
	// Stderr is everything gc and its descendants wrote to the inherited
	// stderr.
	Stderr string
	// Err is the exit error, nil on a zero exit.
	Err error
	// TimedOut reports that the deadline passed and the process group was
	// terminated.
	TimedOut bool
	// PGID is the process group gc ran in, which every descendant that did not
	// leave it shares.
	PGID int
}

// RunGCBounded runs gc with args in its own process group under a deadline.
// It is for tests that drive gc into a deliberately runaway shape (a
// self-calling stub, a loop) and must fail rather than hang or exhaust the
// host if the bound under test is missing: on timeout the whole group is
// terminated, and the group is terminated again when the test ends. Stdout is
// discarded.
func RunGCBounded(t *testing.T, env *Env, dir string, timeout time.Duration, args ...string) BoundedGCRun {
	t.Helper()
	gcPath, err := ResolveGCPath(env)
	if err != nil {
		t.Fatal(err)
	}
	cmd, ctx := boundedToolCommand(t, timeout, gcPath, args...)
	cmd.Dir = dir
	cmd.Env = env.List()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", gcPath, err)
	}
	run := BoundedGCRun{PGID: cmd.Process.Pid}
	run.Err = cmd.Wait()
	run.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	run.Stderr = stderr.String()
	return run
}

// ProcessGroupGone reports whether no process of group pgid remains within
// the given time, polling as it waits.
func ProcessGroupGone(pgid int, within time.Duration) bool {
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return true
		}
		select {
		case <-deadline.C:
			return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
		case <-tick.C:
		}
	}
}
