//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
)

// plantInheritableFD returns a duplicate of a lock file's descriptor without
// close-on-exec, the shape a shell caller leaves behind with
// `exec 9>lock; flock 9`.
func plantInheritableFD(t *testing.T) int {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "held-*.lock")
	if err != nil {
		t.Fatalf("creating lock file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup lock file: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

// Kills: the nudge poller spawn site handing its detached child a descriptor
// the caller holds open without close-on-exec, which would pin the caller's
// flock until the poller exits. The behavior of the scrub itself is pinned in
// internal/nudgepoller; this pins that the spawn site applies it.
func TestNudgePollerCommandClosesInheritedFDs(t *testing.T) {
	planted := plantInheritableFD(t)
	cmd, err := newNudgePollerCommand("/bin/gc", []string{"PATH=/usr/bin"}, "/city", "worker", "coordinator")
	if err != nil {
		t.Fatalf("newNudgePollerCommand: %v", err)
	}
	if slot := planted - 3; slot >= len(cmd.ExtraFiles) || cmd.ExtraFiles[slot] != nil {
		t.Fatalf("ExtraFiles (len %d) does not close planted fd %d in the child", len(cmd.ExtraFiles), planted)
	}
}
