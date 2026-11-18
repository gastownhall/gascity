//go:build windows

package runtimecontract

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// hostPIDLiveness reports whether pid is a live host process. Windows has no
// signal 0, and pidutil.Alive does not separate another user's process from a
// missing one, so a pid it cannot open reads as gone.
func hostPIDLiveness(pid int) (Status, string) {
	if !pidutil.Alive(pid) {
		return StatusFail, fmt.Sprintf("agent_pid %d is no process on this host (a pid from another pid namespace?)", pid)
	}
	return StatusPass, fmt.Sprintf("agent_pid %d is a live host process of the caller's user", pid)
}
