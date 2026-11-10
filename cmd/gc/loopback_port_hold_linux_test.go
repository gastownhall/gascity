//go:build linux

package main

import "errors"

// RED placeholder (ga-os7kifr): the loopback port hold is not implemented yet.
// The GREEN step replaces these stubs.

var errLoopbackPortHoldNotImplemented = errors.New("loopback port hold not implemented")

// holdLoopbackPortFDs is the RED placeholder for the real hold.
func holdLoopbackPortFDs(candidate func() (fd, port int, err error)) (port int, fds []int, err error) {
	_ = candidate
	return 0, nil, errLoopbackPortHoldNotImplemented
}

// closeFDs is the RED placeholder for the real release.
func closeFDs(fds []int) {
	_ = fds
}
