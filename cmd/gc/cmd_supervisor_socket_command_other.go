//go:build !windows

package main

import "net"

func handlePlatformSupervisorConnCommand(_ string, _ net.Conn, _ func(supervisorShutdownMode, shutdownTrigger) bool, _ *shutdownState) bool {
	return false
}
