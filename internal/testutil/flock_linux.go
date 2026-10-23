//go:build linux

package testutil

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// procLocks is the kernel's table of held and pending file locks.
const procLocks = "/proc/locks"

// flockWaiterPollInterval is how often AwaitFlockWaiters rereads procLocks.
const flockWaiterPollInterval = 5 * time.Millisecond

// AwaitFlockWaiters waits until exactly n flock(2) requests on path are
// blocked waiting for the lock. A goroutine parked inside flock(2) exposes no
// completion signal, so the kernel lock table is the boundary this polls: it
// rereads /proc/locks on a ticker and fails the test with the last observed
// count after GoroutineRaceTimeout.
func AwaitFlockWaiters(t testing.TB, path string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), GoroutineRaceTimeout)
	defer cancel()
	ticker := time.NewTicker(flockWaiterPollInterval)
	defer ticker.Stop()
	for {
		got, err := flockWaiters(path)
		if err != nil {
			t.Fatalf("counting flock waiters on %s: %v", path, err)
		}
		if got == n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("flock waiters on %s = %d after %s, want %d", path, got, GoroutineRaceTimeout, n)
		case <-ticker.C:
		}
	}
}

// flockWaiters counts the blocked FLOCK requests procLocks lists for path's
// inode. A blocked request is the "->" row under the lock that blocks it. The
// rows are matched on inode alone because the device column reports the
// superblock's s_dev, which on btrfs differs from the st_dev a stat reports.
func flockWaiters(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: unsupported file metadata %T", path, info.Sys())
	}
	wantInode := strconv.FormatUint(stat.Ino, 10)
	f, err := os.Open(procLocks)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	waiters := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// "<id>: -> FLOCK ADVISORY WRITE <pid> <major>:<minor>:<inode> <start> <end>"
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 || fields[1] != "->" || fields[2] != "FLOCK" {
			continue
		}
		deviceInode := fields[6]
		if deviceInode[strings.LastIndex(deviceInode, ":")+1:] == wantInode {
			waiters++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("reading %s: %w", procLocks, err)
	}
	return waiters, nil
}
