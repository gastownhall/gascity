//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// managedDoltProcRoot is the procfs mount whose per-PID descriptor
// directories confirm lock ownership. Injectable only for tests.
const managedDoltProcRoot = "/proc"

type managedDoltProcLock struct {
	pid                 int
	major, minor, inode uint64
}

func parseManagedDoltProcFlock(line string) (managedDoltProcLock, bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[1] != "FLOCK" {
		return managedDoltProcLock{}, false, nil
	}
	if len(fields) < 8 {
		return managedDoltProcLock{}, true, fmt.Errorf("expected at least 8 fields")
	}
	pid, err := strconv.Atoi(fields[4])
	if err != nil {
		return managedDoltProcLock{}, true, fmt.Errorf("pid %q: %w", fields[4], err)
	}
	deviceInode := strings.Split(fields[5], ":")
	if len(deviceInode) != 3 {
		return managedDoltProcLock{}, true, fmt.Errorf("device and inode %q", fields[5])
	}
	major, err := strconv.ParseUint(deviceInode[0], 16, 64)
	if err != nil {
		return managedDoltProcLock{}, true, fmt.Errorf("major %q as hexadecimal: %w", deviceInode[0], err)
	}
	minor, err := strconv.ParseUint(deviceInode[1], 16, 64)
	if err != nil {
		return managedDoltProcLock{}, true, fmt.Errorf("minor %q as hexadecimal: %w", deviceInode[1], err)
	}
	inode, err := strconv.ParseUint(deviceInode[2], 10, 64)
	if err != nil {
		return managedDoltProcLock{}, true, fmt.Errorf("inode %q as decimal: %w", deviceInode[2], err)
	}
	return managedDoltProcLock{pid: pid, major: major, minor: minor, inode: inode}, true, nil
}

// managedDoltLockHolderPIDs returns the PIDs holding an flock on lockPath.
// It reads procLocksPath for FLOCK rows, and confirms each candidate against
// the descriptors under procRoot.
func managedDoltLockHolderPIDs(lockPath, procLocksPath string) ([]int, error) {
	return managedDoltLockHolderPIDsWithProcRoot(lockPath, procLocksPath, managedDoltProcRoot)
}

// managedDoltLockHolderPIDsWithProcRoot resolves lockPath's flock holders in
// two stages. The FLOCK rows in procLocksPath are matched on inode alone: the
// device column reports the superblock's `s_dev`, which on btrfs (and any
// filesystem exposing an anonymous device per subvolume) differs from the
// `st_dev` a stat of the same file reports, so keying on the device number
// finds nothing and silently disables the gate. Inode alone is ambiguous
// across filesystems, so every candidate is then confirmed positively by
// walking <procRoot>/<pid>/fd and accepting the PID only when one of its open
// descriptors os.SameFile's the lock file — a stricter identity check than
// any device comparison. A candidate whose descriptors cannot be read (EACCES
// or an exited process) is kept as unverifiable: over-inclusion makes the
// gate see a foreign holder and refuse, which is the fail-closed direction.
// Under-inclusion is impossible, since an flock holder necessarily has the
// file open.
func managedDoltLockHolderPIDsWithProcRoot(lockPath, procLocksPath, procRoot string) ([]int, error) {
	// /proc/locks is a seq_file read in several read(2) calls, and a lock
	// another process takes or drops between them shifts the rows the next
	// call starts from, so one scan can miss a row that was there the whole
	// time (seen 3 to 49 times in 3000 scans under flock churn on a busy
	// host). An empty holder set is therefore scanned again, up to
	// managedDoltProcLocksScans times, about a millisecond each, before it
	// is believed. Every caller shares this: the managed Dolt SIGKILL gate
	// and the guided split's co-tenant liveness.
	var pids []int
	var err error
	for scan := 0; scan < managedDoltProcLocksScans; scan++ {
		pids, err = managedDoltScanProcLocksOnce(lockPath, procLocksPath, procRoot)
		if err != nil || len(pids) > 0 {
			return pids, err
		}
	}
	return pids, nil
}

// managedDoltProcLocksScans is how many times an empty holder set is scanned
// before it is believed.
const managedDoltProcLocksScans = 3

// managedDoltScanProcLocksOnce is one scan; a test seam, so a scan that
// misses can be made to happen on demand.
var managedDoltScanProcLocksOnce = managedDoltScanProcLocks

// managedDoltScanProcLocks is one scan of procLocksPath for lockPath's
// holders (managedDoltLockHolderPIDsWithProcRoot).
func managedDoltScanProcLocks(lockPath, procLocksPath, procRoot string) ([]int, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		return nil, fmt.Errorf("stat lock file %s: %w", lockPath, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat lock file %s: unsupported file metadata %T", lockPath, info.Sys())
	}
	f, err := os.Open(procLocksPath) //nolint:gosec // fixed procfs path in production; injectable only for tests
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procLocksPath, err)
	}
	defer f.Close() //nolint:errcheck

	wantInode := stat.Ino
	holders := make(map[int]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lock, flock, err := parseManagedDoltProcFlock(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("parse %s FLOCK row %q: %w", procLocksPath, scanner.Text(), err)
		}
		if !flock {
			continue
		}
		if lock.inode != wantInode {
			continue
		}
		if managedDoltPIDHoldsFile(lock.pid, procRoot, info) {
			holders[lock.pid] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", procLocksPath, err)
	}
	pids := make([]int, 0, len(holders))
	for pid := range holders {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

// managedDoltPIDHoldsFile reports whether pid has want open as one of its
// descriptors under procRoot. An unreadable descriptor directory yields true:
// the PID stays in the holder set as unverifiable so the caller fails closed.
func managedDoltPIDHoldsFile(pid int, procRoot string, want os.FileInfo) bool {
	fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		got, err := os.Stat(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if os.SameFile(got, want) {
			return true
		}
	}
	return false
}
