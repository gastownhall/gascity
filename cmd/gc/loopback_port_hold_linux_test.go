//go:build linux

package main

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// A cmd/gc fixture that picks a loopback port number holds it until the test
// ends. Releasing it between the pick and the fixture's own bind lets another
// test action take it: remote Bazel executors share one loopback port space, so
// that action's bind(0) can be handed the number just released (ga-os7kifr,
// sibling of ga-bd8cn1x).
//
// The hold is a bound, never-listening socket on 127.0.0.1:P and another on
// [::1]:P (v6-only), both close-on-exec and SO_REUSEADDR. The kernel's bind(0)
// treats either as a conflict, so no other process is handed P on either family.
// The sockets are invisible to LISTEN scans and refuse dials, and a SO_REUSEADDR
// binder (Go's net.Listen, every python fixture) can still bind and listen on P.

// holdLoopbackPortTries bounds the re-picks when a foreign listener already owns
// the [::1] twin of a candidate.
const holdLoopbackPortTries = 64

// holdLoopbackPort returns a loopback port number >= atLeast, held on 127.0.0.1 and,
// when the host has it, [::1] until t.Cleanup. An atLeast of 0 accepts any number.
func holdLoopbackPort(t *testing.T, atLeast int) int {
	t.Helper()
	const tries = 20
	for i := 0; i < tries; i++ {
		port, fds, err := holdLoopbackPortFDs(bindLoopbackV4Candidate)
		if err != nil {
			t.Fatalf("hold a loopback port: %v", err)
		}
		if port >= atLeast {
			t.Cleanup(func() { closeFDs(fds) })
			return port
		}
		closeFDs(fds)
	}
	t.Skipf("could not allocate a port >= %d in %d tries", atLeast, tries)
	return 0
}

// holdLoopbackTwin holds the v6-only [::1] twin of port, which a caller already
// owns on 127.0.0.1, until t.Cleanup. It reports false when a foreign listener
// already owns the twin, so the caller picks another number. A host without
// [::1] has nothing to hold and reports true.
func holdLoopbackTwin(t *testing.T, port int) bool {
	t.Helper()
	fd, _, err := rawBind(syscall.AF_INET6, port, true)
	switch {
	case err == nil:
		t.Cleanup(func() { closeFDs([]int{fd}) })
		return true
	case errors.Is(err, syscall.EADDRINUSE):
		return false
	case loopbackV6Unavailable(err):
		return true
	}
	t.Fatalf("hold [::1]:%d: %v", port, err)
	return false
}

// bindLoopbackV4Candidate is the candidate source for holdLoopbackPort: a
// bound-only SO_REUSEADDR socket on a bind(0) port of 127.0.0.1.
func bindLoopbackV4Candidate() (fd, port int, err error) {
	return rawBind(syscall.AF_INET, 0, true)
}

// holdLoopbackPortFDs asks candidate for a bound 127.0.0.1 socket and binds its
// [::1] twin, retrying with another candidate while a foreign listener owns the
// twin. A candidate whose twin was taken stays open until the pick finishes, so
// the kernel cannot hand the same number back. It returns the port and the
// descriptors to release with closeFDs: the v4 socket and, when the host has
// [::1], its twin.
func holdLoopbackPortFDs(candidate func() (fd, port int, err error)) (int, []int, error) {
	var spare []int
	defer func() { closeFDs(spare) }()
	for try := 0; try < holdLoopbackPortTries; try++ {
		fd4, port, err := candidate()
		if err != nil {
			return 0, nil, fmt.Errorf("hold 127.0.0.1:0: %w", err)
		}
		fd6, _, err := rawBind(syscall.AF_INET6, port, true)
		switch {
		case err == nil:
			return port, []int{fd4, fd6}, nil
		case errors.Is(err, syscall.EADDRINUSE):
			spare = append(spare, fd4)
		case loopbackV6Unavailable(err):
			return port, []int{fd4}, nil
		default:
			_ = syscall.Close(fd4)
			return 0, nil, fmt.Errorf("hold [::1]:%d: %w", port, err)
		}
	}
	return 0, nil, fmt.Errorf("no port free on both loopback families in %d tries", holdLoopbackPortTries)
}

// loopbackV6Unavailable reports whether err is what a host without a usable
// [::1] returns from a bind.
func loopbackV6Unavailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT)
}

// closeFDs closes every descriptor in fds.
func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}
