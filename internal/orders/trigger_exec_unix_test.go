//go:build !windows

package orders

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func TestPrepareConditionCommandEscalatesToSIGKILL(t *testing.T) {
	oldOptions := conditionProcessGroupOptions
	var signals []syscall.Signal
	killed := false
	conditionProcessGroupOptions = processgroup.Options{
		CurrentGroupID: func() int { return -1 },
		PollPeriod:     time.Millisecond,
		Kill: func(_ int, sig syscall.Signal) error {
			switch sig {
			case syscall.SIGTERM, syscall.SIGKILL:
				signals = append(signals, sig)
				if sig == syscall.SIGKILL {
					killed = true
				}
				return nil
			case 0:
				if killed {
					return syscall.ESRCH
				}
				return nil
			default:
				t.Fatalf("unexpected signal %v", sig)
				return nil
			}
		},
	}
	t.Cleanup(func() { conditionProcessGroupOptions = oldOptions })

	cmd := exec.CommandContext(context.Background(), "sleep", "10")
	cleanup := prepareConditionCommand(cmd, 0)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup() error = %v, want nil", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}
