//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// nudgeQueueLockHeldForTest reports whether some descriptor holds the queue's
// flock, by trying it on a fresh one.
func nudgeQueueLockHeldForTest(city string) bool {
	f, err := os.OpenFile(nudgequeue.LockPath(city), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func statIno(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return st.Ino, true
}
