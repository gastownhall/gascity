//go:build windows

package main

import (
	"net"
	"syscall"
)

const supervisorSignalTermSocketCommand = "signal_term"

func handlePlatformSupervisorConnCommand(command string, conn net.Conn, requestShutdown func(supervisorShutdownMode, shutdownTrigger) bool, shut *shutdownState) bool {
	if command != supervisorSignalTermSocketCommand {
		return false
	}
	peer := ""
	if addr := conn.RemoteAddr(); addr != nil {
		peer = addr.String()
	}
	handleSupervisorShutdownConn(conn, requestShutdown, shut, supervisorShutdownModeForSignal(syscall.SIGTERM), shutdownTrigger{
		Source:     "socket_signal",
		Signal:     syscall.SIGTERM.String(),
		ClientAddr: peer,
	})
	return true
}
