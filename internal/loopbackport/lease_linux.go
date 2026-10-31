//go:build linux

package loopbackport

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// portLeasesEnforced reports whether leasePort excludes other processes.
const portLeasesEnforced = true

// leasePort takes the netns-wide lease on port: an abstract Unix socket, whose
// name is scoped to the network namespace the port lives in and is released
// by the kernel when the holder exits. ok is false when another holder has it.
// Every binary that leases ports must use this one name, test harness or not,
// so binaries built from different commits on one CI worker skip each other's
// ports.
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
