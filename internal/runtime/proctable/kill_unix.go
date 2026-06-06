//go:build linux || darwin

package proctable

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// KillByPID terminates pid with SIGTERM, then SIGKILL after
// runtime.ManagedProcessStopGrace. Already-gone processes are success.
func KillByPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	if !pidAlive(pid) {
		return nil
	}
	if err := signalPID(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal PID %d with SIGTERM: %w", pid, err)
	}
	deadline := time.NewTimer(runtime.ManagedProcessStopGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			if err := signalPID(pid, syscall.SIGKILL); err != nil {
				return fmt.Errorf("signal PID %d with SIGKILL: %w", pid, err)
			}
			return nil
		case <-ticker.C:
			if !pidAlive(pid) {
				return nil
			}
		}
	}
}

func signalPID(pid int, sig syscall.Signal) error {
	return signalPIDWith(pid, sig, syscall.Kill)
}

func signalPIDWith(pid int, sig syscall.Signal, kill func(int, syscall.Signal) error) error {
	if err := kill(-pid, sig); err == nil {
		return nil
	}
	err := kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
