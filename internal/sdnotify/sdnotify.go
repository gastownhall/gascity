// Package sdnotify implements the client side of the systemd
// sd_notify(3) readiness protocol: short state strings sent as
// datagrams over the unix socket named by the NOTIFY_SOCKET
// environment variable. The protocol is a single unixgram write, so
// the package is deliberately dependency-free.
package sdnotify

import (
	"net"
	"os"
	"time"
)

// State strings from the sd_notify(3) protocol understood by systemd.
const (
	// Ready tells the service manager that startup is finished
	// (Type=notify units stay "activating" until this arrives).
	Ready = "READY=1"
	// Watchdog updates the service watchdog timestamp (WatchdogSec=).
	Watchdog = "WATCHDOG=1"
	// Reloading tells the service manager that a configuration reload
	// has begun; per sd_notify(3) the reload ends with a Ready send.
	Reloading = "RELOADING=1"
	// Stopping tells the service manager that shutdown has begun.
	Stopping = "STOPPING=1"
)

// writeTimeout bounds the datagram send so a wedged notify socket can
// never stall the caller (systemd drains the socket promptly; anything
// else holding it is broken).
const writeTimeout = time.Second

// Notify sends state over the unix datagram socket named by the
// NOTIFY_SOCKET environment variable, following sd_notify(3). When
// NOTIFY_SOCKET is unset or empty — the process is not running under a
// notify-aware service manager — it is a no-op returning (false, nil).
// It returns (true, nil) once the datagram is written, and
// (false, err) when NOTIFY_SOCKET is set but the send fails.
func Notify(state string) (bool, error) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return false, nil
	}
	// A leading '@' names a Linux abstract-namespace socket; the wire
	// form replaces it with a NUL byte.
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close() //nolint:errcheck
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return false, err
	}
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
