package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	testGCBinaryDirPrefix        = "gc-test-binary-pid"
	testCmdGCTempRootPrefix      = "gct"
	testCmdGCShardTempRootPrefix = "gcx"
	testShardIndexEnv            = "GC_TEST_SHARD_INDEX"
	testShardTotalEnv            = "GC_TEST_SHARD_TOTAL"
	testActiveTempRootMarker     = ".gc-test-active-root"
	testSharedFixtureDirPrefix   = "gascity-gc-test-fixtures-pid"
	testSlingFormulaDirPrefix    = "gc-sling-test-formulas-pid"
	testSlingCityDirPrefix       = "gc-sling-test-city-pid"
	testGCHomeDirPrefix          = "gascity-gc-home-pid"
	testRuntimeDirPrefix         = "gascity-runtime-pid"
	testProviderStubDirPrefix    = "gascity-provider-stubs-pid"
	// testAliveSentinelName is a lock file inside each test temp root. The
	// creating process holds an exclusive flock on it for its lifetime;
	// sweepers probe the lock instead of trusting PID visibility, which lies
	// across PID namespaces (ga-djbcqt: bwrap --unshare-pid sandboxes see
	// every host PID as dead while sharing the host /tmp).
	testAliveSentinelName = ".gc-test-alive.lock"
)

// testOrphanSweepMinAge is the minimum age before a PID-prefixed dir becomes
// a sweep candidate. It closes the window where a sibling run has created its
// dir but not yet acquired the alive sentinel.
const testOrphanSweepMinAge = time.Hour

// holdAliveSentinel creates <dir>/.gc-test-alive.lock and takes an exclusive
// flock on it. The caller must keep the returned file referenced for as long
// as the dir must stay protected: the runtime finalizes unreachable os.Files,
// which closes the descriptor and releases the lock.
func holdAliveSentinel(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, testAliveSentinelName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening alive sentinel in %q: %w", dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking alive sentinel in %q: %w", dir, err)
	}
	return f, nil
}

// aliveSentinelHeld probes <dir>'s alive sentinel. exists reports whether the
// sentinel file is present; held reports whether some process still holds its
// flock. Probe failures are reported as held so the sweep stays conservative.
func aliveSentinelHeld(dir string) (exists, held bool) {
	f, err := os.OpenFile(filepath.Join(dir, testAliveSentinelName), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false
		}
		return true, true
	}
	defer f.Close() //nolint:errcheck
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true, true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, false
}

// createActiveTestTempRoot sweeps stale prefix-matching roots under the
// inherited temp dir (honoring TMPDIR rather than hardcoding /tmp, so gate
// runners can isolate concurrent runs), creates this process's test temp
// root there, writes the active-root marker, and acquires the alive sentinel
// lock. The caller must keep the returned file referenced for the lifetime
// of the process so the flock is not released by a finalizer.
func createActiveTestTempRoot(prefix string) (string, *os.File, error) {
	parent := os.TempDir()
	sweepOrphanPIDPrefixedDirs(parent, prefix)
	root, err := os.MkdirTemp(parent, pidPrefixedTempPattern(prefix))
	if err != nil {
		return "", nil, fmt.Errorf("creating test temp root under %q: %w", parent, err)
	}
	if err := os.WriteFile(filepath.Join(root, testActiveTempRootMarker), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("writing active test temp root marker: %w", err)
	}
	sentinel, err := holdAliveSentinel(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return "", nil, err
	}
	return root, sentinel, nil
}

func pidPrefixedTempPattern(prefix string) string {
	return prefix + strconv.Itoa(os.Getpid()) + "-*"
}

func cmdGCTestTempRootPrefix() string {
	if strings.TrimSpace(os.Getenv(testShardIndexEnv)) != "" || strings.TrimSpace(os.Getenv(testShardTotalEnv)) != "" {
		return testCmdGCShardTempRootPrefix
	}
	return testCmdGCTempRootPrefix
}

func pidFromPrefixedDirName(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	end := 0
	for end < len(suffix) && suffix[end] >= '0' && suffix[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if end < len(suffix) && suffix[end] != '-' {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:end])
	if err != nil {
		return 0, false
	}
	return pid, true
}

// sweepOrphanPIDPrefixedDirs removes <root>/<prefix><PID> dirs whose creator
// is gone, including MkdirTemp names such as <prefix><PID>-<random>.
// Best-effort; ignores errors. Used by test setup to clean leftover test
// fixtures from prior crashed/SIGKILL'd runs.
//
// Liveness is decided by the alive sentinel flock when present: flock state
// is visible across PID namespaces, whereas pidAlive reports every host PID
// as dead from inside a bwrap --unshare-pid sandbox that shares the host
// /tmp (ga-djbcqt). PID liveness and the active-root marker are only a
// fallback for legacy dirs without a sentinel. Dirs younger than
// testOrphanSweepMinAge are never touched, covering the window before a
// sibling run's sentinel exists.
func sweepOrphanPIDPrefixedDirs(root, prefix string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, ok := pidFromPrefixedDirName(e.Name(), prefix)
		if !ok || pid <= 0 || pid == self {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < testOrphanSweepMinAge {
			continue
		}
		path := filepath.Join(root, e.Name())
		exists, held := aliveSentinelHeld(path)
		var reason string
		switch {
		case held:
			// Creator (possibly in another PID namespace) is still alive.
			continue
		case exists:
			// Sentinel present but unlocked: the creator is gone. Remove
			// even though the active-root marker is still there — crashed
			// runs never clear their marker.
			reason = "free sentinel"
		default:
			// Legacy dir without a sentinel: fall back to PID liveness and
			// the active-root marker.
			if pidAlive(pid) {
				continue
			}
			if _, err := os.Stat(filepath.Join(path, testActiveTempRootMarker)); err == nil {
				continue
			}
			reason = "legacy: pid dead, no active marker"
		}
		// Name each removal so a recurrence of ga-djbcqt is attributable
		// from run logs instead of gate-log forensics.
		fmt.Fprintf(os.Stderr, "cmd/gc test sweep: removing %s (%s)\n", path, reason)
		_ = os.RemoveAll(path)
	}
}
