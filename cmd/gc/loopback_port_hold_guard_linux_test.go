//go:build linux

package main

import (
	"errors"
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
