//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// The tests in this file pin that a port number a cmd/gc fixture picks stays
// held until t.Cleanup, so a concurrent test action on the same loopback port
// space (remote Bazel executors share one) cannot pick the same number between
// the pick and the fixture's own bind (ga-os7kifr, sibling of ga-bd8cn1x).
//
// Every probe here uses raw syscalls: no net.Listen (Go sets SO_REUSEADDR and
// would hide a held number), no listen(2) and no child process. The held
// sockets are bound and never listen()ed, with SO_REUSEADDR, so a fixture's own
// SO_REUSEADDR bind of the number still succeeds while any other bind does not.

// rawLoopbackSockaddr returns the loopback address for domain with port.
func rawLoopbackSockaddr(domain, port int) syscall.Sockaddr {
	if domain == syscall.AF_INET6 {
		return &syscall.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	}
	return &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
}

// rawBind creates a close-on-exec stream socket in domain, optionally with
// SO_REUSEADDR (and IPV6_V6ONLY for AF_INET6), binds it to the loopback address
// with port (0 picks one) and returns the descriptor and the bound port. It
// never calls listen(2).
func rawBind(domain, port int, reuseAddr bool) (fd, bound int, err error) {
	fd, err = syscall.Socket(domain, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, 0, err
	}
	fail := func(err error) (int, int, error) {
		_ = syscall.Close(fd)
		return -1, 0, err
	}
	if domain == syscall.AF_INET6 {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); err != nil {
			return fail(err)
		}
	}
	if reuseAddr {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			return fail(err)
		}
	}
	if err := syscall.Bind(fd, rawLoopbackSockaddr(domain, port)); err != nil {
		return fail(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		return fd, sa.Port, nil
	case *syscall.SockaddrInet6:
		return fd, sa.Port, nil
	}
	return fail(errors.New("getsockname returned an unexpected address family"))
}

// loopbackV6Usable reports whether this host can bind [::1]. It treats the
// errors a host without IPv6 loopback returns as "not usable" and fails the
// test on any other error.
func loopbackV6Usable(t *testing.T) bool {
	t.Helper()
	fd, _, err := rawBind(syscall.AF_INET6, 0, false)
	if err == nil {
		_ = syscall.Close(fd)
		return true
	}
	switch {
	case errors.Is(err, syscall.EADDRNOTAVAIL), errors.Is(err, syscall.EAFNOSUPPORT), errors.Is(err, syscall.EPROTONOSUPPORT):
		return false
	}
	t.Fatalf("probe [::1]: %v", err)
	return false
}

// loopbackDomains returns the address families a held number must cover.
func loopbackDomains(t *testing.T) []int {
	t.Helper()
	domains := []int{syscall.AF_INET}
	if loopbackV6Usable(t) {
		domains = append(domains, syscall.AF_INET6)
	}
	return domains
}

func loopbackDomainName(domain int) string {
	if domain == syscall.AF_INET6 {
		return "[::1]"
	}
	return "127.0.0.1"
}

// requireLoopbackPortHeld fails unless a bind of port WITHOUT SO_REUSEADDR is
// refused with EADDRINUSE on every family in domains.
func requireLoopbackPortHeld(t *testing.T, port int, domains []int) {
	t.Helper()
	for _, domain := range domains {
		fd, _, err := rawBind(domain, port, false)
		if err == nil {
			_ = syscall.Close(fd)
			t.Errorf("%s:%d is free, want it held", loopbackDomainName(domain), port)
			continue
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("bind %s:%d: %v, want EADDRINUSE", loopbackDomainName(domain), port, err)
		}
	}
}

// requireLoopbackPortFree fails unless a bind of port WITHOUT SO_REUSEADDR
// succeeds on every family in domains.
func requireLoopbackPortFree(t *testing.T, port int, domains []int) {
	t.Helper()
	for _, domain := range domains {
		fd, _, err := rawBind(domain, port, false)
		if err != nil {
			t.Errorf("bind %s:%d: %v, want it released", loopbackDomainName(domain), port, err)
			continue
		}
		_ = syscall.Close(fd)
	}
}

// TestPortHelpersHoldTheNumberUntilCleanup pins AC1: the helpers that pick a
// loopback port keep it bound (127.0.0.1 and, when usable, [::1]) until the
// test's cleanup, and release it then.
func TestPortHelpersHoldTheNumberUntilCleanup(t *testing.T) {
	domains := loopbackDomains(t)
	pickers := []struct {
		name string
		pick func(t *testing.T) int
	}{
		{"reserveRandomTCPPort", reserveRandomTCPPort},
		{"freeLoopbackPort", func(t *testing.T) int {
			port, err := strconv.Atoi(freeLoopbackPort(t))
			if err != nil {
				t.Fatalf("freeLoopbackPort: %v", err)
			}
			return port
		}},
		{"freeEphemeralLoopbackPort", func(t *testing.T) int {
			port := freeEphemeralLoopbackPort(t)
			if port < ephemeralPortThresholdForTest {
				t.Errorf("freeEphemeralLoopbackPort = %d, want >= %d", port, ephemeralPortThresholdForTest)
			}
			return port
		}},
		{"listenOnRandomPort", func(t *testing.T) int {
			ln := listenOnRandomPort(t)
			t.Cleanup(func() { _ = ln.Close() })
			return ln.Addr().(*net.TCPAddr).Port
		}},
	}
	for _, p := range pickers {
		t.Run(p.name, func(t *testing.T) {
			var port int
			t.Run("pick", func(t *testing.T) {
				port = p.pick(t)
				requireLoopbackPortHeld(t, port, domains)
			})
			if port == 0 {
				t.Skip("helper skipped without a port")
			}
			requireLoopbackPortFree(t, port, domains)
		})
	}
}

// TestReservedLoopbackPortIsNeverReturnedByRawBindZero pins AC3a: a held number
// is never handed out by bind(0) in either family. Unguarded, the kernel
// returns a just-closed number about once in 5,700 picks on a loaded executor
// (architect measurement for ga-os7kifr), so 50,000 picks per family make a
// missing guard fail with probability above 0.9998.
func TestReservedLoopbackPortIsNeverReturnedByRawBindZero(t *testing.T) {
	const picks = 50000
	held := reserveRandomTCPPort(t)
	for _, domain := range loopbackDomains(t) {
		for i := 0; i < picks; i++ {
			fd, port, err := rawBind(domain, 0, false)
			if err != nil {
				t.Fatalf("pick %d on %s: %v", i, loopbackDomainName(domain), err)
			}
			_ = syscall.Close(fd)
			if port == held {
				t.Fatalf("bind(0) on %s returned the held port %d on pick %d", loopbackDomainName(domain), port, i)
			}
		}
	}
}

// TestHeldLoopbackPortSemantics pins AC3b: a SO_REUSEADDR bind of the held
// number succeeds (the fixture's own child binds that way) while a bind without
// SO_REUSEADDR gets EADDRINUSE.
func TestHeldLoopbackPortSemantics(t *testing.T) {
	port := reserveRandomTCPPort(t)
	for _, domain := range loopbackDomains(t) {
		fd, _, err := rawBind(domain, port, true)
		if err != nil {
			t.Errorf("SO_REUSEADDR bind %s:%d: %v, want success", loopbackDomainName(domain), port, err)
		} else {
			_ = syscall.Close(fd)
		}
	}
	requireLoopbackPortHeld(t, port, loopbackDomains(t))
}

// v4Candidate is the candidate source holdLoopbackPortFDs is given in these
// tests: a SO_REUSEADDR bind(0) on 127.0.0.1.
func v4Candidate() (fd, port int, err error) {
	return rawBind(syscall.AF_INET, 0, true)
}

// TestHoldLoopbackPortSkipsCandidateWhoseV6TwinIsTaken pins AC3c: when the first
// candidate's [::1] twin is already bound by someone else without SO_REUSEADDR,
// the pick keeps looking and returns a different number, and the rejected
// candidate is released.
func TestHoldLoopbackPortSkipsCandidateWhoseV6TwinIsTaken(t *testing.T) {
	if !loopbackV6Usable(t) {
		t.Skip("[::1] is not usable on this host")
	}
	calls, first := 0, 0
	blocker := -1
	t.Cleanup(func() {
		if blocker >= 0 {
			_ = syscall.Close(blocker)
		}
	})
	candidate := func() (int, int, error) {
		fd, port, err := v4Candidate()
		if err != nil {
			return -1, 0, err
		}
		calls++
		if calls == 1 {
			first = port
			blocker, _, err = rawBind(syscall.AF_INET6, port, false)
			if err != nil {
				_ = syscall.Close(fd)
				return -1, 0, err
			}
		}
		return fd, port, nil
	}
	port, fds, err := holdLoopbackPortFDs(candidate)
	if err != nil {
		t.Fatalf("holdLoopbackPortFDs: %v", err)
	}
	t.Cleanup(func() { closeFDs(fds) })
	if port == first {
		t.Fatalf("held the first candidate %d whose [::1] twin was taken", port)
	}
	if calls < 2 {
		t.Fatalf("candidate source called %d times, want a re-pick", calls)
	}
	if len(fds) != 2 {
		t.Fatalf("held %d descriptors, want the v4 socket and its [::1] twin", len(fds))
	}
	requireLoopbackPortFree(t, first, []int{syscall.AF_INET})
}

// TestHeldLoopbackPortDescriptors pins AC3d: every held descriptor is
// close-on-exec, so a child the fixture spawns cannot inherit it and keep the
// number bound after the test, and none of them listens.
func TestHeldLoopbackPortDescriptors(t *testing.T) {
	port, fds, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatalf("holdLoopbackPortFDs: %v", err)
	}
	t.Cleanup(func() { closeFDs(fds) })
	if len(fds) == 0 {
		t.Fatalf("no descriptors held for port %d", port)
	}
	for _, fd := range fds {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 {
			t.Fatalf("fcntl(F_GETFD, %d): %v", fd, errno)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			t.Errorf("descriptor %d is not close-on-exec", fd)
		}
		accepting, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err != nil {
			t.Fatalf("getsockopt(SO_ACCEPTCONN, %d): %v", fd, err)
		}
		if accepting != 0 {
			t.Errorf("descriptor %d listens, want a bound-only socket", fd)
		}
	}
}

// TestHeldLoopbackPortReleaseFreesTheNumber pins AC3e: after the release a bind
// without SO_REUSEADDR succeeds in every family the hold covered.
func TestHeldLoopbackPortReleaseFreesTheNumber(t *testing.T) {
	port, fds, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatalf("holdLoopbackPortFDs: %v", err)
	}
	domains := []int{syscall.AF_INET}
	if len(fds) > 1 {
		domains = append(domains, syscall.AF_INET6)
	}
	requireLoopbackPortHeld(t, port, domains)
	closeFDs(fds)
	requireLoopbackPortFree(t, port, domains)
}

// TestStartOpenFileAndTCPListenerHoldsLoopbackV6Twin pins AC5: the open-file
// helper's child holds the v6-only [::1] twin of its 127.0.0.1 port for its
// life, so a foreign [::1] listener on the same number cannot fool the
// port-holder lookup.
func TestStartOpenFileAndTCPListenerHoldsLoopbackV6Twin(t *testing.T) {
	if !loopbackV6Usable(t) {
		t.Skip("[::1] is not usable on this host")
	}
	path := filepath.Join(t.TempDir(), "held")
	proc, port, err := startOpenFileAndTCPListener(path, "", filepath.Join(t.TempDir(), "listener.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	})
	requireLoopbackPortHeld(t, port, []int{syscall.AF_INET, syscall.AF_INET6})
}

// The tests below were added by the reviewer (gm-4xiq97, ga-os7kifr). They pin
// the AC2 clauses of holdLoopbackPortFDs that no test above pins: the re-pick is
// bounded at 64, a rejected candidate stays open until the pick ends, and an
// error from the candidate source or from the [::1] bind is wrapped with %w. They
// use the same raw syscalls as the tests above: no listen(2), no child process,
// no probe of a port number after a release.

// socketInode returns the inode of the socket behind fd, or 0 when fd is closed.
// A closed descriptor number a later socket reuses reads as a different inode, so
// a held socket cannot be mistaken for its replacement.
func socketInode(fd int) uint64 {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0
	}
	return st.Ino
}

// ownBoundSocket identifies a socket independently of descriptor-number reuse.
type ownBoundSocket struct {
	fd     int
	domain int
	inode  uint64
}

// ownLoopbackSockets finds this process's sockets bound to port in domains.
func ownLoopbackSockets(_ int, _ []int) ([]ownBoundSocket, error) {
	return nil, errors.New("not implemented")
}

// socketsStillOpen returns current descriptors whose socket identities were pinned.
func socketsStillOpen(pinned, now []ownBoundSocket) []ownBoundSocket {
	var open []ownBoundSocket
	for _, current := range now {
		for _, held := range pinned {
			if current.inode == held.inode {
				open = append(open, current)
				break
			}
		}
	}
	return open
}

// pinHeldLoopbackSockets requires at least one held socket on every covered family.
func pinHeldLoopbackSockets(t *testing.T, port int, domains []int) []ownBoundSocket {
	t.Helper()
	pinned, err := ownLoopbackSockets(port, domains)
	if err != nil {
		t.Fatalf("pin sockets for port %d: %v", port, err)
	}
	for _, domain := range domains {
		found := false
		for _, socket := range pinned {
			if socket.domain == domain {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no held socket on %s:%d", loopbackDomainName(domain), port)
		}
	}
	return pinned
}

// requireLoopbackSocketsReleased fails for each pinned socket that remains open.
func requireLoopbackSocketsReleased(t *testing.T, pinned []ownBoundSocket, port int, domains []int) {
	t.Helper()
	if len(pinned) == 0 {
		t.Fatal("release check requires a nonempty socket pin")
	}
	now, err := ownLoopbackSockets(port, domains)
	if err != nil {
		t.Fatalf("scan released sockets for port %d: %v", port, err)
	}
	for _, socket := range socketsStillOpen(pinned, now) {
		t.Errorf("socket on %s:%d still open: fd=%d inode=%d", loopbackDomainName(socket.domain), port, socket.fd, socket.inode)
	}
}

// TestOwnLoopbackSocketsAccountForExactlyTheHold pins the scan's identity and
// family boundaries, including a released number beside an independently held one.
func TestOwnLoopbackSocketsAccountForExactlyTheHold(t *testing.T) {
	portA, fdsA, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFDs(fdsA) })
	portB, fdsB, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFDs(fdsB) })
	if portA == portB {
		t.Fatalf("independent holds share port %d", portA)
	}
	identities := func(fds []int) ([]int, []ownBoundSocket) {
		domains := []int{syscall.AF_INET}
		if len(fds) > 1 {
			domains = append(domains, syscall.AF_INET6)
		}
		var want []ownBoundSocket
		seen := make(map[uint64]bool)
		for i, fd := range fds {
			inode := socketInode(fd)
			if inode == 0 || seen[inode] {
				t.Fatalf("held fd %d has zero or duplicate socket inode %d", fd, inode)
			}
			seen[inode] = true
			want = append(want, ownBoundSocket{fd: fd, domain: domains[i], inode: inode})
		}
		return domains, want
	}
	domainsA, wantA := identities(fdsA)
	domainsB, wantB := identities(fdsB)
	requireScan := func(port int, domains []int, want []ownBoundSocket) {
		t.Helper()
		got, err := ownLoopbackSockets(port, domains)
		if err != nil {
			t.Fatalf("scan port %d: %v", port, err)
		}
		if len(got) != len(want) {
			t.Fatalf("scan port %d = %+v, want exactly %+v", port, got, want)
		}
		remaining := make(map[ownBoundSocket]bool)
		for _, socket := range want {
			remaining[socket] = true
		}
		for _, socket := range got {
			if !remaining[socket] {
				t.Fatalf("scan port %d returned unexpected or duplicate socket %+v", port, socket)
			}
			delete(remaining, socket)
		}
	}
	requireScan(portA, domainsA, wantA)
	requireScan(portB, domainsB, wantB)
	for _, socket := range wantA {
		requireScan(portA, []int{socket.domain}, []ownBoundSocket{socket})
	}
	closeFDs(fdsA)
	fdsA = nil
	requireScan(portA, domainsA, nil)
	requireScan(portB, domainsB, wantB)
}

// TestReleaseCheckIgnoresAForeignHolderOfTheNumber reproduces durable port reuse
// with replacement sockets whose identities differ from the released hold's.
func TestReleaseCheckIgnoresAForeignHolderOfTheNumber(t *testing.T) {
	port, fds, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFDs(fds) })
	domains := []int{syscall.AF_INET}
	if len(fds) > 1 {
		domains = append(domains, syscall.AF_INET6)
	}
	pinned := pinHeldLoopbackSockets(t, port, domains)
	closeFDs(fds)
	fds = nil
	var replacements []ownBoundSocket
	var replacementFDs []int
	t.Cleanup(func() { closeFDs(replacementFDs) })
	for _, domain := range domains {
		fd, _, err := rawBind(domain, port, false)
		switch {
		case err == nil:
			replacementFDs = append(replacementFDs, fd)
			replacements = append(replacements, ownBoundSocket{fd: fd, domain: domain, inode: socketInode(fd)})
		case errors.Is(err, syscall.EADDRINUSE):
			// A real foreign holder already supplies the reported collision.
		default:
			t.Fatalf("replace %s:%d: %v", loopbackDomainName(domain), port, err)
		}
	}
	requireLoopbackSocketsReleased(t, pinned, port, domains)
	now, err := ownLoopbackSockets(port, domains)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range replacements {
		found := false
		for _, socket := range now {
			if socket == replacement && socket.inode != 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("scan omitted replacement socket %+v: %+v", replacement, now)
		}
	}
	if open := socketsStillOpen(pinned, now); len(open) != 0 {
		t.Errorf("replacement sockets reported as unreleased hold: %+v", open)
	}
}

// TestReleaseCheckDetectsAnUnreleasedOwnSocket pins both an extra bound socket
// and a duplicate descriptor as leaks, then verifies closing them clears the check.
func TestReleaseCheckDetectsAnUnreleasedOwnSocket(t *testing.T) {
	for _, name := range []string{"extra_socket", "dup_descriptor"} {
		t.Run(name, func(t *testing.T) {
			port, fds, err := holdLoopbackPortFDs(v4Candidate)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeFDs(fds) })
			domains := []int{syscall.AF_INET}
			if len(fds) > 1 {
				domains = append(domains, syscall.AF_INET6)
			}
			leakedFD := -1
			t.Cleanup(func() {
				if leakedFD >= 0 {
					_ = syscall.Close(leakedFD)
				}
			})
			if name == "extra_socket" {
				leakedFD, _, err = rawBind(syscall.AF_INET, port, true)
				if err != nil {
					t.Fatal(err)
				}
			}
			originalInode := socketInode(fds[0])
			pinned := pinHeldLoopbackSockets(t, port, domains)
			if name == "dup_descriptor" {
				dup, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fds[0]), syscall.F_DUPFD_CLOEXEC, 0)
				if errno != 0 {
					t.Fatalf("fcntl(F_DUPFD_CLOEXEC): %v", errno)
				}
				leakedFD = int(dup)
				if inode := socketInode(leakedFD); inode == 0 || inode != originalInode {
					t.Fatalf("dup inode %d, want original inode %d", inode, originalInode)
				}
			}
			want := ownBoundSocket{fd: leakedFD, domain: syscall.AF_INET, inode: socketInode(leakedFD)}
			if want.inode == 0 {
				t.Fatal("leaked socket has zero inode")
			}
			closeFDs(fds)
			fds = nil
			now, err := ownLoopbackSockets(port, domains)
			if err != nil {
				t.Fatal(err)
			}
			if open := socketsStillOpen(pinned, now); len(open) != 1 || open[0] != want {
				t.Fatalf("unreleased sockets = %+v, want exactly %+v", open, want)
			}
			if err := syscall.Close(leakedFD); err != nil {
				t.Fatal(err)
			}
			leakedFD = -1
			now, err = ownLoopbackSockets(port, domains)
			if err != nil {
				t.Fatal(err)
			}
			if open := socketsStillOpen(pinned, now); len(open) != 0 {
				t.Errorf("sockets remain after closing the leak: %+v", open)
			}
		})
	}
}

// twinTakenCandidate returns a candidate source for holdLoopbackPortFDs: a
// SO_REUSEADDR bind(0) on 127.0.0.1 like v4Candidate, whose [::1] twin the first
// taken candidates find held by a bound-only socket without SO_REUSEADDR. Later
// candidates are left alone. The blockers close at t.Cleanup.
func twinTakenCandidate(t *testing.T, taken int) func() (int, int, error) {
	t.Helper()
	var blockers []int
	t.Cleanup(func() { closeFDs(blockers) })
	picks := 0
	return func() (int, int, error) {
		fd, port, err := v4Candidate()
		if err != nil {
			return -1, 0, err
		}
		picks++
		if picks <= taken {
			blocker, _, err := rawBind(syscall.AF_INET6, port, false)
			switch {
			case err == nil:
				blockers = append(blockers, blocker)
			case errors.Is(err, syscall.EADDRINUSE):
				// a foreign socket already holds the twin: the pick is rejected either way
			default:
				_ = syscall.Close(fd)
				return -1, 0, err
			}
		}
		return fd, port, nil
	}
}

// TestHoldLoopbackPortFDsGivesUpAtTheTryBound pins AC2's bound: when every
// candidate's [::1] twin is taken, the pick asks for exactly 64 candidates and
// then fails with no descriptor, instead of looping on.
func TestHoldLoopbackPortFDsGivesUpAtTheTryBound(t *testing.T) {
	if !loopbackV6Usable(t) {
		t.Skip("[::1] is not usable on this host")
	}
	const bound = 64
	inner := twinTakenCandidate(t, bound*2)
	calls := 0
	candidate := func() (int, int, error) {
		calls++
		return inner()
	}
	port, fds, err := holdLoopbackPortFDs(candidate)
	if err == nil {
		closeFDs(fds)
		t.Fatalf("held port %d although every [::1] twin was taken", port)
	}
	if calls != bound {
		t.Errorf("asked the candidate source %d times, want exactly %d", calls, bound)
	}
	if fds != nil {
		t.Errorf("returned descriptors %v alongside the error", fds)
	}
}

// TestHoldLoopbackPortFDsKeepsRejectedCandidatesOpenUntilThePickEnds pins AC2's
// keep-open clause: a candidate whose [::1] twin was taken stays the same open
// socket through every later candidate, so bind(0) cannot hand its number back
// inside one pick, and it is released when the pick returns while the winner is
// held.
func TestHoldLoopbackPortFDsKeepsRejectedCandidatesOpenUntilThePickEnds(t *testing.T) {
	if !loopbackV6Usable(t) {
		t.Skip("[::1] is not usable on this host")
	}
	const taken = 3
	type pick struct {
		fd, port int
		ino      uint64
	}
	var picks []pick
	inner := twinTakenCandidate(t, taken)
	candidate := func() (int, int, error) {
		for _, p := range picks {
			if socketInode(p.fd) != p.ino {
				t.Errorf("rejected candidate %d (descriptor %d) was released before the pick ended", p.port, p.fd)
			}
		}
		fd, port, err := inner()
		if err == nil {
			picks = append(picks, pick{fd, port, socketInode(fd)})
		}
		return fd, port, err
	}
	port, fds, err := holdLoopbackPortFDs(candidate)
	if err != nil {
		t.Fatalf("holdLoopbackPortFDs: %v", err)
	}
	t.Cleanup(func() { closeFDs(fds) })
	if len(picks) <= taken {
		t.Fatalf("candidate source called %d times, want more than the %d candidates whose twin was taken", len(picks), taken)
	}
	winner := picks[len(picks)-1]
	if port != winner.port || len(fds) == 0 || fds[0] != winner.fd {
		t.Fatalf("held port %d with descriptors %v, want the last candidate %d on descriptor %d", port, fds, winner.port, winner.fd)
	}
	if socketInode(winner.fd) != winner.ino {
		t.Errorf("the winning candidate %d is no longer the socket the pick returned", winner.port)
	}
	for _, p := range picks[:len(picks)-1] {
		if socketInode(p.fd) == p.ino {
			t.Errorf("rejected candidate %d (descriptor %d) is still open after the pick returned", p.port, p.fd)
		}
	}
}

// TestHoldLoopbackPortFDsWrapsACandidateError pins AC2's last clause for the
// candidate source: its error comes back wrapped with %w and with context, and
// no descriptor comes back with it.
func TestHoldLoopbackPortFDsWrapsACandidateError(t *testing.T) {
	refused := errors.New("candidate source refused")
	port, fds, err := holdLoopbackPortFDs(func() (int, int, error) { return -1, 0, refused })
	if err == nil {
		closeFDs(fds)
		t.Fatalf("held port %d although the candidate source failed", port)
	}
	if !errors.Is(err, refused) {
		t.Errorf("error %q does not wrap the candidate source's error", err)
	}
	if err.Error() == refused.Error() {
		t.Errorf("error %q carries no context beyond the candidate source's own", err)
	}
	if fds != nil {
		t.Errorf("returned descriptors %v alongside the error", fds)
	}
}

// TestHoldLoopbackPortFDsWrapsAV6BindErrorAndReleasesTheCandidate pins AC2's
// last clause for the [::1] twin: a bind error that is neither "in use" nor "no
// [::1]" fails the pick with %w and context, closes the 127.0.0.1 candidate and
// holds nothing. The candidate reports a port number no socket can bind: Go
// refuses anything above 65535 with EINVAL before the kernel sees it.
func TestHoldLoopbackPortFDsWrapsAV6BindErrorAndReleasesTheCandidate(t *testing.T) {
	if !loopbackV6Usable(t) {
		t.Skip("[::1] is not usable on this host")
	}
	const unbindable = 70000
	var candidateFD int
	var candidateInode uint64
	candidate := func() (int, int, error) {
		fd, _, err := v4Candidate()
		if err != nil {
			return -1, 0, err
		}
		candidateFD, candidateInode = fd, socketInode(fd)
		return fd, unbindable, nil
	}
	port, fds, err := holdLoopbackPortFDs(candidate)
	if err == nil {
		closeFDs(fds)
		t.Fatalf("held port %d although [::1]:%d cannot be bound", port, unbindable)
	}
	if !errors.Is(err, syscall.EINVAL) {
		t.Errorf("error %q does not wrap the [::1] bind error", err)
	}
	if err.Error() == syscall.EINVAL.Error() {
		t.Errorf("error %q carries no context beyond the bind error's own", err)
	}
	if fds != nil {
		t.Errorf("returned descriptors %v alongside the error", fds)
	}
	if socketInode(candidateFD) == candidateInode {
		t.Errorf("the 127.0.0.1 candidate (descriptor %d) is still open after the failed pick", candidateFD)
	}
}

// TestLoopbackV6UnavailableMatchesOnlyAHostWithoutLoopbackV6 pins the test the
// v4-only fallback hangs on: only the three errors a host without a usable [::1]
// returns from a bind count, wrapped or not. Any other bind error must fail the
// pick instead.
func TestLoopbackV6UnavailableMatchesOnlyAHostWithoutLoopbackV6(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{syscall.EADDRNOTAVAIL, true},
		{syscall.EAFNOSUPPORT, true},
		{syscall.EPROTONOSUPPORT, true},
		{fmt.Errorf("bind [::1]:0: %w", syscall.EADDRNOTAVAIL), true},
		{syscall.EADDRINUSE, false},
		{syscall.EACCES, false},
		{syscall.EINVAL, false},
		{errors.New("not an errno"), false},
	}
	for _, c := range cases {
		if got := loopbackV6Unavailable(c.err); got != c.want {
			t.Errorf("loopbackV6Unavailable(%v) = %t, want %t", c.err, got, c.want)
		}
	}
}

// TestHoldLoopbackPortFDsHoldsEveryFamilyTheHostHas pins AC2's fallback from the
// other side: the pick holds the v4 socket and, exactly when [::1] is usable, its
// twin. On a host without [::1] it pins the v4-only result.
func TestHoldLoopbackPortFDsHoldsEveryFamilyTheHostHas(t *testing.T) {
	want := 1
	if loopbackV6Usable(t) {
		want = 2
	}
	port, fds, err := holdLoopbackPortFDs(v4Candidate)
	if err != nil {
		t.Fatalf("holdLoopbackPortFDs: %v", err)
	}
	t.Cleanup(func() { closeFDs(fds) })
	if len(fds) != want {
		t.Errorf("held %d descriptors for port %d, want %d", len(fds), port, want)
	}
}
