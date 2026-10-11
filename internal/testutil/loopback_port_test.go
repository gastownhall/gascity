package testutil

import (
	"errors"
	"net"
	"testing"
)

// A reserved port lies between reservedPortRangeStart and the floor of the
// kernel's ephemeral range, which bind(":0") and outgoing connects draw from.
func TestReserveLoopbackPortIsOutsideTheEphemeralRange(t *testing.T) {
	port, err := ReserveLoopbackPort()
	if err != nil {
		t.Fatalf("ReserveLoopbackPort: %v", err)
	}
	if lo := ephemeralPortRangeStart(); port < reservedPortRangeStart || port >= lo {
		t.Fatalf("port %d outside [%d, %d)", port, reservedPortRangeStart, lo)
	}
}

// Two reservations never return the same port while the first holder lives.
// listen-then-close on ":0" gave two concurrent Bazel test actions on one
// rbe-west worker the same supervisor port (ga-96smfk.45, ga-vnycm2.15): the
// lease is netns-wide, so it holds across processes as well as within one.
func TestReserveLoopbackPortNeverHandsOutALeasedPort(t *testing.T) {
	if !portLeasesEnforced {
		t.Skip("port leases need a network-namespace-scoped name (Linux abstract sockets)")
	}
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		port, err := ReserveLoopbackPort()
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		if seen[port] {
			t.Fatalf("reservation %d returned port %d twice", i, port)
		}
		seen[port] = true
	}
}

// With a one-port range, the second reservation of that port fails instead of
// returning a port another holder already leased.
func TestReserveLoopbackPortInRefusesALeasedPort(t *testing.T) {
	if !portLeasesEnforced {
		t.Skip("port leases need a network-namespace-scoped name (Linux abstract sockets)")
	}
	port, err := ReserveLoopbackPort()
	if err != nil {
		t.Fatalf("ReserveLoopbackPort: %v", err)
	}
	if got, err := reserveLoopbackPortIn(port, port); err == nil {
		t.Fatalf("reserveLoopbackPortIn(%d, %d) = %d, want an error for a leased port", port, port, got)
	}
}

// A port that something outside the lease already holds is skipped, and its
// lease is given back. A test binary killed by a Bazel timeout drops its lease
// while the server child it started can still hold the port, so only the bind
// probe keeps the next reservation off it.
func TestReserveLoopbackPortSkipsABoundPort(t *testing.T) {
	listen := listenLoopbackProbe
	t.Cleanup(func() { listenLoopbackProbe = listen })
	bound := 0
	listenLoopbackProbe = func(port int) (net.Listener, error) {
		if bound == 0 {
			bound = port
			return nil, errors.New("address already in use")
		}
		return listen(port)
	}

	port, err := ReserveLoopbackPort()
	if err != nil {
		t.Fatalf("ReserveLoopbackPort: %v", err)
	}
	if bound == 0 {
		t.Fatal("ReserveLoopbackPort never probed whether its port was bound")
	}
	if port == bound {
		t.Fatalf("ReserveLoopbackPort returned port %d, which the probe found bound", port)
	}
	lease, ok, err := leasePort(bound)
	if err != nil || !ok {
		t.Fatalf("leasePort(%d) = %v, %v after the skip, want the bound port's lease given back", bound, ok, err)
	}
	if lease != nil {
		_ = lease.Close()
	}
}
