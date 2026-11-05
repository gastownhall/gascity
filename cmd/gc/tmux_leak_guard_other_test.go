//go:build !linux

package main

import "github.com/rogpeppe/go-internal/testscript"

// newTmuxLeakGuardedTestingM is a passthrough on platforms without the
// /proc-based tmux leak guard: the guard attributes leaked tmux servers to a
// run by reading /proc/<pid>/{cmdline,environ}, which only exists on Linux.
func newTmuxLeakGuardedTestingM(m testscript.TestingM, _ string) testscript.TestingM {
	return m
}
