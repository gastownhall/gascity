//go:build !linux

package loopbackport

import "net"

// portLeasesEnforced reports whether leasePort excludes other processes.
// Without abstract sockets there is no namespace-scoped name to hold, so a
// reservation relies on the range alone.
const portLeasesEnforced = false

// leasePort grants every port: see portLeasesEnforced.
func leasePort(int) (net.Listener, bool, error) {
	return nil, true, nil
}
