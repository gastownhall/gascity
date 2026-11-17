package pidutil

import (
	"os"
	"testing"
	"time"
)

// AliveWithCmdline answers "is this PID the process I think it is" by comparing
// argv. It short-circuited to `return true` on every non-Linux host, because
// Cmdline read only /proc/<pid>/cmdline.
//
// That turns an identity check into a bare existence check. Its callers use it
// to decide whether the PID in a poller pidfile is still *their* poller: on a
// host with high PID churn, a recycled PID owned by an unrelated live process
// then reads as "poller already running", and the caller returns success
// without starting one — cmd/gc/cmd_nudge.go returns 0, internal/session's
// submit path returns nil. Nudge and submit delivery stop for that target with
// no error and nothing logged.
//
// These tests pin the identity semantics on every platform. The existing
// coverage asserted them and then skipped off Linux, which is why the inversion
// survived.

// TestAliveWithCmdline_FalseForDeadPID and _NilMatch pin the two answers that
// must not change.
func TestAliveWithCmdline_FalseForDeadPID(t *testing.T) {
	cmd := shortLivedTestCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := AliveWithCmdline(pid, func([]string) bool { return true })
		if err == nil && !alive {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("AliveWithCmdline(%d) stayed true for an exited child", pid)
}

func TestAliveWithCmdline_NilMatchIsFalse(t *testing.T) {
	got, err := AliveWithCmdline(os.Getpid(), nil)
	if err != nil {
		t.Fatalf("AliveWithCmdline(self, nil): %v", err)
	}
	if got {
		t.Fatal("AliveWithCmdline(self, nil) = true, want false")
	}
}
