package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"golang.org/x/sys/unix"
)

// Dolt holds an exclusive flock on each database's `.dolt/noms/LOCK` for the
// whole life of the store — from open until the chunk journal is flushed and
// the store is closed. A second `dolt sql-server` binding the same data_dir
// while that lock is held races the prior instance's journal flush and
// corrupts the noms journal (gastownhall/gascity#3174). These helpers probe
// that on-disk lock so lifecycle operations can key their singleton guard on
// what dolt actually enforces, rather than on TCP readiness or PID files.

// managedDoltDataDirLockPollInterval is the cadence for re-probing a held
// dolt store lock while waiting for release.
const managedDoltDataDirLockPollInterval = 250 * time.Millisecond

// managedDoltDataDirLockFiles returns the existing dolt exclusive store lock
// files under dataDir: the root-level `.dolt/noms/LOCK` when dataDir is
// itself a dolt directory, plus the per-database `<db>/.dolt/noms/LOCK`
// paths. Candidates are stat'd directly — never globbed — because glob
// metacharacters in the literal dataDir path (an unmatched `[` errors out, a
// valid `[x]`/`?`/`*` matches the wrong directories) would silently disable
// the guard and re-open the #3174 race for any city at such a path.
func managedDoltDataDirLockFiles(dataDir string) []string {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil
	}
	var files []string
	appendLockCandidate := func(path string) {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			files = append(files, path)
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// No store at this path — nothing to probe.
		default:
			// Unknowable lock state: fail open to keep the legacy behavior,
			// but say so (the guard is silently disabled otherwise).
			fmt.Fprintf(os.Stderr, "warning: cannot probe dolt store lock %s: %v; treating as free (gastownhall/gascity#3174)\n", path, err)
		}
	}
	appendLockCandidate(filepath.Join(dataDir, ".dolt", "noms", "LOCK"))
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			fmt.Fprintf(os.Stderr, "warning: cannot enumerate dolt databases under %s: %v; per-database store locks not probed (gastownhall/gascity#3174)\n", dataDir, err)
		}
		return files
	}
	for _, entry := range entries {
		appendLockCandidate(filepath.Join(dataDir, entry.Name(), ".dolt", "noms", "LOCK"))
	}
	return files
}

// managedDoltDataDirLockHolder probes each dolt store lock under dataDir with
// a non-blocking flock and returns the path of the first lock held by a live
// process, or "" when every lock is free. A free lock is acquired and
// released within the probe; callers run this only before spawning or after
// signaling a server, never while a healthy owned server should keep its
// lock.
func managedDoltDataDirLockHolder(dataDir string) string {
	for _, path := range managedDoltDataDirLockFiles(dataDir) {
		f, err := os.Open(path) //nolint:gosec // path derives from the managed data dir layout
		if err != nil {
			// Vanishing between enumeration and open means the store was removed —
			// genuinely free. Anything else means the lock state is unknowable;
			// fail open to keep the legacy behavior, but say so (the guard is
			// silently disabled otherwise). Mirrors the disk-preflight
			// fail-open-with-warning convention.
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "warning: cannot probe dolt store lock %s: %v; treating as free (gastownhall/gascity#3174)\n", path, err)
			}
			continue
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			continue
		}
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return path
		}
		fmt.Fprintf(os.Stderr, "warning: cannot probe dolt store lock %s: %v; treating as free (gastownhall/gascity#3174)\n", path, err)
	}
	return ""
}

// waitForManagedDoltDataDirLockFree blocks until no live process holds a dolt
// exclusive store lock under dataDir, or timeout elapses. Lock release on a
// clean dolt shutdown happens only after the chunk journal is flushed, so a
// successful return also means the prior instance finished writing. On
// timeout it returns an error naming the held lock — callers fail closed
// rather than racing the holder. A non-positive timeout probes exactly once.
func waitForManagedDoltDataDirLockFree(dataDir string, timeout time.Duration) error {
	holder := managedDoltDataDirLockHolder(dataDir)
	if holder == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remain := time.Until(deadline)
		if remain < managedDoltDataDirLockPollInterval {
			time.Sleep(remain)
		} else {
			time.Sleep(managedDoltDataDirLockPollInterval)
		}
		holder = managedDoltDataDirLockHolder(dataDir)
		if holder == "" {
			return nil
		}
	}
	return fmt.Errorf("dolt exclusive store lock %s is still held by a live process after %s; a prior dolt sql-server has not released the data dir", holder, timeout)
}

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

// managedDoltLockHolderPIDs returns the PIDs whose FLOCK rows in procLocksPath
// match lockPath's device and inode. Linux prints the device major and minor in
// hexadecimal but the inode in decimal, so the three components are parsed
// with their respective bases before comparison.
func managedDoltLockHolderPIDs(lockPath, procLocksPath string) ([]int, error) {
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

	wantMajor := uint64(unix.Major(stat.Dev))
	wantMinor := uint64(unix.Minor(stat.Dev))
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
		if lock.major == wantMajor && lock.minor == wantMinor && lock.inode == wantInode {
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

// waitManagedDoltSIGKILLLockGate gates a SIGKILL on the dolt exclusive store
// lock being free or held solely by the process being terminated. It blocks
// until the gate is safe, pid exits, or lockWindow elapses. A measurement
// failure or any other holder fails closed to protect the journal
// (gastownhall/gascity#3174). gracePeriod is the SIGTERM grace that already
// elapsed, reported in the error for context.
func waitManagedDoltSIGKILLLockGate(pid int, dataDir string, alive func(int) bool, gracePeriod, lockWindow, pollInterval time.Duration) error {
	return waitManagedDoltSIGKILLLockGateWithProcLocks(pid, dataDir, alive, gracePeriod, lockWindow, pollInterval, "/proc/locks")
}

func waitManagedDoltSIGKILLLockGateWithProcLocks(pid int, dataDir string, alive func(int) bool, gracePeriod, lockWindow, pollInterval time.Duration, procLocksPath string) error {
	lockDeadline := time.Now().Add(lockWindow)
	for {
		if !alive(pid) {
			return nil
		}
		holder := managedDoltDataDirLockHolder(dataDir)
		if holder == "" {
			return nil
		}
		holderPIDs, measureErr := managedDoltLockHolderPIDs(holder, procLocksPath)
		if measureErr == nil && len(holderPIDs) == 1 && holderPIDs[0] == pid {
			return nil
		}
		if !time.Now().Before(lockDeadline) {
			base := fmt.Sprintf("pid %d did not exit within %s and a live process still holds dolt exclusive store lock %s; refusing SIGKILL mid-journal-write (gastownhall/gascity#3174)", pid, gracePeriod, holder)
			if measureErr != nil {
				return fmt.Errorf("%s; could not measure lock ownership: %w", base, measureErr)
			}
			if len(holderPIDs) == 0 {
				return fmt.Errorf("%s; could not measure lock ownership: flock probe reports held but %s has no matching FLOCK row", base, procLocksPath)
			}
			otherPIDs := make([]int, 0, len(holderPIDs))
			for _, holderPID := range holderPIDs {
				if holderPID != pid {
					otherPIDs = append(otherPIDs, holderPID)
				}
			}
			return fmt.Errorf("%s; other holder pid(s): %v", base, otherPIDs)
		}
		time.Sleep(pollInterval)
	}
}

// resolveManagedDoltLockReleaseTimeout returns the configured wait window for
// dolt's on-disk exclusive lock. Reads `[dolt].dolt_lock_release_timeout`
// from city.toml when available; falls back to
// config.DefaultDoltLockReleaseTimeout when the config cannot be loaded.
// Mirrors resolveManagedDoltStopTimeout's empty-cityPath guard.
func resolveManagedDoltLockReleaseTimeout(cityPath string) time.Duration {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltLockReleaseTimeout
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return config.DefaultDoltLockReleaseTimeout
	}
	return cfg.Dolt.DoltLockReleaseTimeoutDuration()
}
