//go:build unix

package acceptancehelpers

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

const (
	descendantReady         = "ready\n"
	boundedToolTestDeadline = 3 * time.Second
	waitingLeaderScript     = "#!/bin/sh\nsleep 300 &\necho ready >&3\nwait\n"
	exitingLeaderScript     = "#!/bin/sh\nsleep 300 </dev/null >/dev/null 2>&1 &\necho ready >&3\n"
)

func TestBoundedToolCommandKillsDescendantsAtDeadline(t *testing.T) {
	held, holder := descendantPipe(t)
	cmd, ctx := boundedToolCommand(t, boundedToolTestDeadline, descendantLeavingStub(t, waitingLeaderScript))
	cmd.ExtraFiles = []*os.File{holder}
	out, err := cmd.CombinedOutput()
	closeHolder(t, holder)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("command context error = %v, want deadline exceeded; command error = %v\n%s", ctx.Err(), err, out)
	}
	readDescendantReady(t, held)
	requireDescendantsGone(t, cmd, held, "command deadline")
}

func TestBoundedToolCommandCleanupStopsAnUnwaitedCommand(t *testing.T) {
	held, holder := descendantPipe(t)
	var cmd *exec.Cmd
	t.Run("start without wait", func(t *testing.T) {
		cmd, _ = boundedToolCommand(t, time.Minute, descendantLeavingStub(t, waitingLeaderScript))
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = time.Hour
		cmd.ExtraFiles = []*os.File{holder}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		readDescendantReady(t, held)
	})
	closeHolder(t, holder)
	requireDescendantsGone(t, cmd, held, "test cleanup of a command that was never waited")
}

func TestBoundedToolCommandCleanupStopsDescendantsOfAnExitedLeader(t *testing.T) {
	held, holder := descendantPipe(t)
	var cmd *exec.Cmd
	t.Run("leader exits before its child", func(t *testing.T) {
		cmd, _ = boundedToolCommand(t, time.Minute, descendantLeavingStub(t, exitingLeaderScript))
		cmd.ExtraFiles = []*os.File{holder}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("stub leader failed: %v\n%s", err, out)
		}
		readDescendantReady(t, held)
	})
	closeHolder(t, holder)
	requireDescendantsGone(t, cmd, held, "test cleanup after its leader exited")
}

func TestBoundedToolCommandCleanupToleratesAZombieInItsGroup(t *testing.T) {
	held, holder := descendantPipe(t)
	t.Run("zombie outlives the leader", func(t *testing.T) {
		joiner, _ := boundedToolCommand(t, time.Minute, descendantLeavingStub(t, "#!/bin/sh\nexit 0\n"))
		leader, _ := boundedToolCommand(t, time.Minute, descendantLeavingStub(t, "#!/bin/sh\nexec sleep 300\n"))
		if err := leader.Start(); err != nil {
			t.Fatal(err)
		}
		joiner.SysProcAttr.Pgid = leader.Process.Pid
		joiner.ExtraFiles = []*os.File{holder}
		if err := joiner.Start(); err != nil {
			t.Fatal(err)
		}
		closeHolder(t, holder)
		requireDescendantsGone(t, joiner, held, "the joiner exited")
	})
}

func descendantLeavingStub(t *testing.T, script string) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

func descendantPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	held, holder, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = held.Close()
		_ = holder.Close()
	})
	return held, holder
}

func closeHolder(t *testing.T, holder *os.File) {
	t.Helper()
	if err := holder.Close(); err != nil {
		t.Fatalf("close the test's copy of the descendant pipe: %v", err)
	}
}

func readDescendantReady(t *testing.T, held *os.File) {
	t.Helper()
	if err := held.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, len(descendantReady))
	if _, err := io.ReadFull(held, ready); err != nil || string(ready) != descendantReady {
		t.Fatalf("stub never reported its background child: read %q, %v", ready, err)
	}
}

func requireDescendantsGone(t *testing.T, cmd *exec.Cmd, held *os.File, after string) {
	t.Helper()
	if err := held.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(held)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		_ = terminateBoundedToolCommand(cmd)
		t.Fatalf("a descendant of the bounded command survived %s and still holds its pipe open", after)
	}
	if err != nil || len(rest) != 0 {
		t.Fatalf("drain descendant pipe after %s: read %q, %v", after, rest, err)
	}
}
