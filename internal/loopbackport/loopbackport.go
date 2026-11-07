// Package loopbackport hands out 127.0.0.1 TCP ports for servers that a
// process configures now and a child process binds later: an isolated
// supervisor's API port seeded into supervisor.toml, or a port a test harness
// writes into a config before it starts the server.
package loopbackport

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// reservedPortRangeStart is the lowest port Reserve hands out: above the
// registered well-known services a host may run.
const reservedPortRangeStart = 10000

// maxPort is the highest TCP port.
const maxPort = 65535

// defaultEphemeralPortRangeStart is the Linux default lower bound of
// net.ipv4.ip_local_port_range, used where the live value cannot be read.
const defaultEphemeralPortRangeStart = 32768

// reservePortAttempts bounds the random probes before giving up.
const reservePortAttempts = 256

// ephemeralPortRangePath holds the kernel's ephemeral port range on Linux.
// Tests point it at a file of their own.
var ephemeralPortRangePath = "/proc/sys/net/ipv4/ip_local_port_range"

var (
	portLeasesMu sync.Mutex
	// portLeases keeps every lease open for the life of the process; a
	// collected listener would be closed by its finalizer and free the name.
	portLeases []net.Listener
)

// Reserve returns a 127.0.0.1 TCP port for a server that a child process
// binds later (a supervisor's API, an external Dolt). On Linux the port is
// leased: no other Reserve caller in the network namespace is handed it while
// this process lives.
//
// The usual listen-on-":0"-then-close reservation is a race on a shared host:
// the kernel hands the freed port to the next bind(":0") or outgoing connect
// of any process, and two processes reserving at the same moment can be
// handed the same port. Bazel actions on one rbe-west worker share the host
// network namespace, so concurrent test shards and isolated supervisor homes
// were given the same supervisor port, and each supervisor exited at startup
// on "address already in use" while the other held it (ga-96smfk.45,
// ga-vnycm2.15, ga-96smfk.87).
//
// So the port comes from outside the kernel's ephemeral range, which
// bind(":0") and connect draw from: from the larger of the bands from 10000
// up that lie below and above it (reservableRange). On Linux it is leased
// under a name scoped to the network namespace (an abstract socket) that this
// process holds until it exits: another caller, in this or any other process,
// skips a leased port. The kernel drops the lease with the process, so
// nothing goes stale. A port that something else has bound is skipped too,
// which covers a server child that outlives the process that leased its port.
//
// Two gaps remain. Other platforms have no namespace-scoped name to lease, so
// there a reservation rests on the range and the bound-port skip alone
// (portLeasesEnforced). And where the ephemeral range covers every port from
// 10000 up (a host that widened it to 1024-65535), no port lies outside it:
// the port comes from [10000, 32767] instead, and the lease and the
// bound-port skip are the only protection.
func Reserve() (int, error) {
	lo, hi, _ := reservableRange(EphemeralPortRange())
	return reserveIn(lo, hi)
}

// EphemeralPortRange returns the kernel's ephemeral port range, inclusive:
// the ports bind(":0") and outgoing connects are handed
// (net.ipv4.ip_local_port_range). Where that cannot be read, it is
// [32768, 65535], which covers the Linux default and the macOS range
// (49152-65535).
func EphemeralPortRange() (lo, hi int) {
	data, err := os.ReadFile(ephemeralPortRangePath)
	if err != nil {
		return defaultEphemeralPortRangeStart, maxPort
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return defaultEphemeralPortRangeStart, maxPort
	}
	lo, errLo := strconv.Atoi(fields[0])
	hi, errHi := strconv.Atoi(fields[1])
	if errLo != nil || errHi != nil || lo < 1 || hi > maxPort || lo > hi {
		return defaultEphemeralPortRangeStart, maxPort
	}
	return lo, hi
}

// reservableRange returns the band Reserve draws from, given the kernel's
// ephemeral range [lo, hi]: the larger of the bands from
// reservedPortRangeStart up that lie below and above it, so that no
// bind(":0") or connect is handed the port. outside is false where the
// ephemeral range covers every port from reservedPortRangeStart up; the band
// is then [reservedPortRangeStart, defaultEphemeralPortRangeStart-1], inside
// the range.
func reservableRange(lo, hi int) (bandLo, bandHi int, outside bool) {
	aboveLo := max(hi+1, reservedPortRangeStart)
	below := lo - reservedPortRangeStart
	above := maxPort - aboveLo + 1
	switch {
	case below <= 0 && above <= 0:
		return reservedPortRangeStart, defaultEphemeralPortRangeStart - 1, false
	case below >= above:
		return reservedPortRangeStart, lo - 1, true
	default:
		return aboveLo, maxPort, true
	}
}

// reserveIn is Reserve over the inclusive range [lo, hi].
func reserveIn(lo, hi int) (int, error) {
	if lo < 1 || hi > maxPort || hi < lo {
		return 0, fmt.Errorf("reserving a loopback port: empty range [%d, %d]", lo, hi)
	}
	span := hi - lo + 1
	attempts := reservePortAttempts
	if span < attempts {
		attempts = span
	}
	start := rand.IntN(span)
	stride := 1
	if span > 1 {
		// A stride coprime to span visits distinct ports on every attempt.
		stride = 1 + rand.IntN(span-1)
		for gcd(stride, span) != 1 {
			stride++
		}
	}
	for i := 0; i < attempts; i++ {
		port := lo + (start+i*stride)%span
		lease, ok, err := leasePort(port)
		if err != nil {
			return 0, fmt.Errorf("reserving loopback port %d: %w", port, err)
		}
		if !ok {
			continue
		}
		lis, err := listenLoopbackProbe(port)
		if err != nil {
			if lease != nil {
				_ = lease.Close()
			}
			continue
		}
		if err := lis.Close(); err != nil {
			if lease != nil {
				_ = lease.Close()
			}
			return 0, fmt.Errorf("reserving loopback port %d: closing probe: %w", port, err)
		}
		if lease != nil {
			portLeasesMu.Lock()
			portLeases = append(portLeases, lease)
			portLeasesMu.Unlock()
		}
		return port, nil
	}
	return 0, fmt.Errorf("reserving a loopback port: no free unleased port in [%d, %d] after %d attempts", lo, hi, attempts)
}

// listenLoopbackProbe binds 127.0.0.1:port, so reserveIn can skip
// a port that something outside the lease already holds. Tests replace it to
// stand in for that holder.
var listenLoopbackProbe = func(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
