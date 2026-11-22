//go:build windows

package acp

import "github.com/gastownhall/gascity/internal/processgroup"

// signalInterrupt sends sc's console process group the Windows interrupt
// (CTRL_BREAK_EVENT), as Interrupt does; Windows has no SIGINT to send.
func signalInterrupt(sc *sessionConn) error {
	return processgroup.InterruptCommand(sc.cmd, sc.pgid, processgroup.Options{})
}
