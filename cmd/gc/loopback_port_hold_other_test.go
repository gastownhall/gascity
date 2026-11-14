//go:build !linux

package main

import (
	"net"
	"testing"
)

// Off Linux a fixture's port pick keeps the bind-and-close shape: BSD and macOS
// refuse a second bind of an address a held socket owns even with SO_REUSEADDR,
// so a held number would lock the fixture's own child out. See the Linux twin
// in loopback_port_hold_linux_test.go for the rule this relaxes.

// holdLoopbackPort returns a loopback port number >= atLeast that is free when it
// returns. An atLeast of 0 accepts any number.
func holdLoopbackPort(t *testing.T, atLeast int) int {
	t.Helper()
	const tries = 20
	for i := 0; i < tries; i++ {
		ln := listenOnRandomPort(t)
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if port >= atLeast {
			return port
		}
	}
	t.Skipf("could not allocate a port >= %d in %d tries", atLeast, tries)
	return 0
}

// holdLoopbackTwin has nothing to hold off Linux and always reports success.
func holdLoopbackTwin(*testing.T, int) bool { return true }
