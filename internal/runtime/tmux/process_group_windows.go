//go:build windows

package tmux

import "fmt"

func signalPID(_ int, signal processSignal) error {
	return fmt.Errorf("process signal %q is unsupported on this platform", signal)
}

func isProcessSignalGoneError(error) bool {
	return false
}
