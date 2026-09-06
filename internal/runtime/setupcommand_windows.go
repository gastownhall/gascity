//go:build windows

package runtime

import (
	"os/exec"
	"time"
)

// Windows has no POSIX process-group signal equivalent in this package. The
// standard CommandContext cancellation remains the safe fallback; the common
// WaitDelay/output contract is still applied by setupcommand.go.
func applySetupCommandCancellation(_ *exec.Cmd, _ time.Duration) func(bool) {
	return func(bool) {}
}
