package loopbackport

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A reserved port lies outside the kernel's ephemeral range, which
// bind(":0") and outgoing connects draw from.
func TestReserveIsOutsideTheEphemeralRange(t *testing.T) {
	lo, hi := EphemeralPortRange()
	if _, _, outside := reservableRange(lo, hi); !outside {
		t.Skipf("the kernel's ephemeral range [%d, %d] covers every port from %d up", lo, hi, reservedPortRangeStart)
	}
	port, err := Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if port < reservedPortRangeStart || (port >= lo && port <= hi) {
		t.Fatalf("port %d is below %d or in the kernel's ephemeral range [%d, %d]", port, reservedPortRangeStart, lo, hi)
	}
}

// Where the ephemeral range starts at or below 10000, nothing lies below it to
// draw from, so Reserve draws from above it instead of from the ports
// bind(":0") and connect are handed.
func TestReserveDrawsAboveALowEphemeralRange(t *testing.T) {
	useEphemeralPortRange(t, "1024\t40000\n")
	for i := 0; i < 5; i++ {
		port, err := Reserve()
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		if port <= 40000 {
			t.Fatalf("reservation %d returned port %d, in the ephemeral range [1024, 40000]", i, port)
		}
	}
}

// Reserve draws from the larger of the bands from 10000 up that lie below and
// above the ephemeral range, and falls back to a band inside the range only
// where it covers every port from 10000 up.
func TestReservableRange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		lo, hi         int
		bandLo, bandHi int
		outside        bool
	}{
		{"linux default", 32768, 60999, 10000, 32767, true},
		{"unreadable range", 32768, 65535, 10000, 32767, true},
		{"floor below 10000", 1024, 65000, 65001, 65535, true},
		{"floor below 10000, few ports above", 9000, 65500, 65501, 65535, true},
		{"floor at 10000", 10000, 60999, 61000, 65535, true},
		{"few ports below the floor", 10050, 60999, 61000, 65535, true},
		{"range below 10000", 1024, 5000, 10000, 65535, true},
		{"every port from 10000 up", 1024, 65535, 10000, 32767, false},
		{"every port from exactly 10000 up", 10000, 65535, 10000, 32767, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bandLo, bandHi, outside := reservableRange(tc.lo, tc.hi)
			if bandLo != tc.bandLo || bandHi != tc.bandHi || outside != tc.outside {
				t.Fatalf("reservableRange(%d, %d) = [%d, %d] outside=%v, want [%d, %d] outside=%v",
					tc.lo, tc.hi, bandLo, bandHi, outside, tc.bandLo, tc.bandHi, tc.outside)
			}
		})
	}
}

// EphemeralPortRange reads net.ipv4.ip_local_port_range, and takes
// [32768, 65535] where that cannot be read.
func TestEphemeralPortRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		lo, hi  int
	}{
		{"kernel format", "32768\t60999\n", 32768, 60999},
		{"widened", "1024 65000\n", 1024, 65000},
		{"missing", "", 32768, 65535},
		{"one bound", "1024\n", 32768, 65535},
		{"not a number", "low high\n", 32768, 65535},
		{"inverted", "60999 32768\n", 32768, 65535},
		{"port zero", "0 60999\n", 32768, 65535},
		{"beyond the last port", "1024 70000\n", 32768, 65535},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useEphemeralPortRange(t, tc.content)
			if lo, hi := EphemeralPortRange(); lo != tc.lo || hi != tc.hi {
				t.Fatalf("EphemeralPortRange() = [%d, %d], want [%d, %d]", lo, hi, tc.lo, tc.hi)
			}
		})
	}
}

// Two reservations never return the same port while the first holder lives.
// listen-then-close on ":0" gave two concurrent Bazel test actions on one
// rbe-west worker the same supervisor port (ga-96smfk.45, ga-vnycm2.15): the
// lease is netns-wide, so it holds across processes as well as within one.
// The range is pinned to the Linux default, so the 50 leases come from the
// 22768 ports below it rather than from the host's band, which can be
// narrower (9000-65500 leaves 35 ports).
func TestReserveNeverHandsOutALeasedPort(t *testing.T) {
	if !portLeasesEnforced {
		t.Skip("port leases need a network-namespace-scoped name (Linux abstract sockets)")
	}
	useEphemeralPortRange(t, "32768\t60999\n")
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		port, err := Reserve()
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
// returning a port another holder already leased. Two collections run first:
// a lease the package stopped referencing would be closed by its finalizer,
// freeing the name.
func TestReserveInRefusesALeasedPort(t *testing.T) {
	if !portLeasesEnforced {
		t.Skip("port leases need a network-namespace-scoped name (Linux abstract sockets)")
	}
	port, err := Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	runtime.GC()
	runtime.GC()
	if got, err := reserveIn(port, port); err == nil {
		t.Fatalf("reserveIn(%d, %d) = %d, want an error for a leased port", port, port, got)
	}
}

// A port that something outside the lease already holds is skipped, and its
// lease is given back. A test binary killed by a Bazel timeout drops its lease
// while the server child it started can still hold the port, so only the bind
// probe keeps the next reservation off it.
func TestReserveSkipsABoundPort(t *testing.T) {
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

	port, err := Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if bound == 0 {
		t.Fatal("Reserve never probed whether its port was bound")
	}
	if port == bound {
		t.Fatalf("Reserve returned port %d, which the probe found bound", port)
	}
	lease, ok, err := leasePort(bound)
	if err != nil || !ok {
		t.Fatalf("leasePort(%d) = %v, %v after the skip, want the bound port's lease given back", bound, ok, err)
	}
	if lease != nil {
		_ = lease.Close()
	}
}

// useEphemeralPortRange points EphemeralPortRange at a file holding content,
// or at a missing file where content is empty.
func useEphemeralPortRange(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ip_local_port_range")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := ephemeralPortRangePath
	t.Cleanup(func() { ephemeralPortRangePath = saved })
	ephemeralPortRangePath = path
}
