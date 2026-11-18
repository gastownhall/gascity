//go:build windows

package runtimetest

// killProcessGroup does nothing on windows: it has no process groups to
// signal, and killLeftover has already returned unless /proc/<pid>/environ
// was readable, which it never is there.
func killProcessGroup(int) {}
