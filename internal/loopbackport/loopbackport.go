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

// defaultEphemeralPortRangeStart is the Linux default lower bound of
// net.ipv4.ip_local_port_range, used where the live value cannot be read.
const defaultEphemeralPortRangeStart = 32768

// reservePortAttempts bounds the random probes before giving up.
const reservePortAttempts = 256

var (
	portLeasesMu sync.Mutex
	// portLeases keeps every lease open for the life of the process; a
	// collected listener would be closed by its finalizer and free the name.
	portLeases []net.Listener
)

// Reserve returns a 127.0.0.1 TCP port for a server that a child process
// binds later, and keeps it from every other caller on the host for as long
// as this process lives.
//
// The usual listen-on-":0"-then-close reservation is a race on a shared host:
// the kernel hands the freed port to the next bind(":0") or outgoing connect
// of any process, and two processes reserving at the same moment can be
// handed the same port. Bazel actions on one rbe-west worker share the host
// network namespace, so concurrent test shards were given the same
// supervisor port, and each supervisor exited at startup on "address already
// in use" while the other held it (ga-96smfk.45, ga-96smfk.87).
//
// So the port comes from below the kernel's ephemeral range, which neither
// bind(":0") nor connect ever assigns, and is leased under a name scoped to
// the network namespace (a Linux abstract socket) that this process holds
// until it exits: another caller, in this or any other process, skips a
// leased port. A port that something else has bound is skipped too. The
// kernel drops the lease with the process, so nothing goes stale.
func Reserve() (int, error) {
	return reserveIn(reservedPortRangeStart, ephemeralPortRangeStart()-1)
}

// reserveIn is Reserve over the inclusive range [lo, hi].
func reserveIn(lo, hi int) (int, error) {
	if lo < 1 || hi > 65535 || hi < lo {
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
		lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
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

// ephemeralPortRangeStart returns the lower bound of the kernel's ephemeral
// port range.
func ephemeralPortRangeStart() int {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return defaultEphemeralPortRangeStart
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return defaultEphemeralPortRangeStart
	}
	lo, err := strconv.Atoi(fields[0])
	if err != nil || lo <= reservedPortRangeStart {
		return defaultEphemeralPortRangeStart
	}
	return lo
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
