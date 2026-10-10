package testutil

import (
	"net"
	"strconv"
	"testing"
)

// A reserved port lies outside the kernel's ephemeral range, where no
// bind(":0") or outgoing connect of any process on the host is ever handed
// it, and is free when returned.
func TestReserveLoopbackPortIsOutsideTheEphemeralRangeAndFree(t *testing.T) {
	port, err := ReserveLoopbackPort()
	if err != nil {
		t.Fatalf("ReserveLoopbackPort: %v", err)
	}
	if lo := ephemeralPortRangeStart(); port < reservedPortRangeStart || port >= lo {
		t.Fatalf("port %d outside [%d, %d)", port, reservedPortRangeStart, lo)
	}
	lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("reserved port %d is not bindable: %v", port, err)
	}
	_ = lis.Close()
}

// Two reservations never return the same port while the first holder lives.
// listen-then-close on ":0" gave two concurrent Bazel test actions on one
// rbe-west worker the same supervisor port (ga-96smfk.45, ga-vnycm2.15): the
// lease is netns-wide, so it holds across processes as well as within one.
func TestReserveLoopbackPortNeverHandsOutALeasedPort(t *testing.T) {
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

// A port some other socket already holds is skipped.
func TestReserveLoopbackPortInSkipsABoundPort(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck
	port := lis.Addr().(*net.TCPAddr).Port
	if got, err := reserveLoopbackPortIn(port, port); err == nil {
		t.Fatalf("reserveLoopbackPortIn(%d, %d) = %d, want an error for a bound port", port, port, got)
	}
}
