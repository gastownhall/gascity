//go:build !linux

package dolttest

import (
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// waitProcessExit reports whether the process identified by pid and
// startTime has exited. Without pidfds it cannot wait, and it does not need
// to: the owner reaper scans /proc and is a no-op on these platforms.
func waitProcessExit(pid int, startTime string, _ time.Duration) bool {
	return !pidutil.AliveWithStartTime(pid, startTime)
}
