package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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

// managedDoltDataDirHeldLocks probes each dolt store lock under dataDir with
// a non-blocking flock and returns every lock currently held by a live
// process, in enumeration order. A free lock is acquired and released within
// the probe; callers run this only before spawning or after signaling a
// server, never while a healthy owned server should keep its lock.
func managedDoltDataDirHeldLocks(dataDir string) []string {
	var held []string
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
			held = append(held, path)
			continue
		}
		fmt.Fprintf(os.Stderr, "warning: cannot probe dolt store lock %s: %v; treating as free (gastownhall/gascity#3174)\n", path, err)
	}
	return held
}

// managedDoltDataDirLockHolder returns the path of the first dolt store lock
// under dataDir held by a live process, or "" when every lock is free.
func managedDoltDataDirLockHolder(dataDir string) string {
	held := managedDoltDataDirHeldLocks(dataDir)
	if len(held) == 0 {
		return ""
	}
	return held[0]
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

// waitManagedDoltSIGKILLLockGate gates a SIGKILL on the dolt exclusive store
// lock being free or held solely by the process being terminated. It blocks
// until the gate is safe, pid exits, or lockWindow elapses. A measurement
// failure or any other holder fails closed to protect the journal
// (gastownhall/gascity#3174). gracePeriod is the SIGTERM grace that already
// elapsed, reported in the error for context.
func waitManagedDoltSIGKILLLockGate(pid int, dataDir string, alive func(int) bool, gracePeriod, lockWindow, pollInterval time.Duration) error {
	return waitManagedDoltSIGKILLLockGateWithProcLocks(pid, dataDir, alive, gracePeriod, lockWindow, pollInterval, "/proc/locks")
}

// waitManagedDoltSIGKILLLockGateWithProcLocks evaluates the gate against
// every held lock under dataDir, not just the first: with several databases
// in one data dir, keying on a single lock would let os.ReadDir ordering
// decide whether the sole-holder exception applies. SIGKILL is permitted only
// once every held lock resolves to exactly pid.
func waitManagedDoltSIGKILLLockGateWithProcLocks(pid int, dataDir string, alive func(int) bool, gracePeriod, lockWindow, pollInterval time.Duration, procLocksPath string) error {
	lockDeadline := time.Now().Add(lockWindow)
	for {
		if !alive(pid) {
			return nil
		}
		blocker, blockerPIDs, measureErr := managedDoltSIGKILLLockBlocker(dataDir, pid, procLocksPath)
		if blocker == "" {
			return nil
		}
		if !time.Now().Before(lockDeadline) {
			base := fmt.Sprintf("pid %d did not exit within %s and a live process still holds dolt exclusive store lock %s; refusing SIGKILL mid-journal-write (gastownhall/gascity#3174)", pid, gracePeriod, blocker)
			if measureErr != nil {
				return fmt.Errorf("%s; could not measure lock ownership: %w", base, measureErr)
			}
			if len(blockerPIDs) == 0 {
				return fmt.Errorf("%s; could not measure lock ownership: flock probe reports held but %s has no matching FLOCK row", base, procLocksPath)
			}
			otherPIDs := make([]int, 0, len(blockerPIDs))
			for _, holderPID := range blockerPIDs {
				if holderPID != pid {
					otherPIDs = append(otherPIDs, holderPID)
				}
			}
			return fmt.Errorf("%s; other holder pid(s): %v", base, otherPIDs)
		}
		time.Sleep(pollInterval)
	}
}

// managedDoltSIGKILLLockBlocker returns the first held lock under dataDir
// that does not resolve to pid alone, along with its measured holders and any
// measurement failure. An empty path means every held lock (if any) is held
// solely by pid, so SIGKILL is safe.
func managedDoltSIGKILLLockBlocker(dataDir string, pid int, procLocksPath string) (string, []int, error) {
	for _, held := range managedDoltDataDirHeldLocks(dataDir) {
		holderPIDs, err := managedDoltLockHolderPIDs(held, procLocksPath)
		if err != nil {
			return held, nil, err
		}
		if len(holderPIDs) == 1 && holderPIDs[0] == pid {
			continue
		}
		return held, holderPIDs, nil
	}
	return "", nil, nil
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
