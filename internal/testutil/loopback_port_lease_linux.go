//go:build linux

package testutil

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// portLeasesEnforced reports whether leasePort excludes other processes.
const portLeasesEnforced = true

// leasePort takes the host-wide lease on port: an abstract Unix socket, whose
// name is scoped to the network namespace the port lives in and is released
// by the kernel when the holder exits. ok is false when another holder has it.
func leasePort(port int) (lease net.Listener, ok bool, err error) {
	lis, err := net.Listen("unix", "@gascity-test-loopback-port-"+strconv.Itoa(port))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return lis, true, nil
}
