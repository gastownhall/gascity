package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestWatchConfigTargets_CloseReturnsFDsToBaseline is the Darwin
// regression for gastownhall/gascity#4504.
//
// gascity already calls Watcher.Close() in watchConfigTargets cleanup.
// On Darwin, fsnotify v1.9.0 still leaked one kqueue-backed REG/DIR
// descriptor per watched path (fsnotify#732). fsnotify#740, shipped in
// v1.10.0, drops watches directly in Close(). This test asserts the
// process FD count returns to the pre-watcher baseline after cleanup
// and after a Close+NewWatcher restart — not a magic absolute threshold.
func TestWatchConfigTargets_CloseReturnsFDsToBaseline(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug")
	}

	root := t.TempDir()
	const nDirs = 40
	targets := make([]config.WatchTarget, 0, nDirs)
	for i := 0; i < nDirs; i++ {
		p := filepath.Join(root, fmt.Sprintf("d%02d", i))
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		targets = append(targets, config.WatchTarget{Path: p})
	}

	var dirty atomic.Bool
	pokeCh := make(chan struct{}, 1)
	var stderr bytes.Buffer

	baseline := countProcessFDs(t)
	cleanup := watchConfigTargets(targets, &dirty, pokeCh, &stderr)
	afterAdd := countProcessFDs(t)
	held := afterAdd - baseline
	if held < nDirs/2 {
		t.Fatalf("watcher did not hold a measurable number of FDs (baseline=%d afterAdd=%d held=%d dirs=%d); test is not probing the leak; stderr=%q",
			baseline, afterAdd, held, nDirs, stderr.String())
	}

	cleanup()
	afterClose := countProcessFDs(t)
	residual := afterClose - baseline
	if residual > fdCountNoise {
		t.Fatalf("FD count did not return to baseline after Watcher.Close(): baseline=%d afterAdd=%d afterClose=%d held=%d residual=%d; stderr=%q",
			baseline, afterAdd, afterClose, held, residual, stderr.String())
	}

	// Supervisor reload path: Close then NewWatcher. Generation 2 must not
	// inherit leaked FDs from generation 1.
	restartBaseline := countProcessFDs(t)
	cleanup2 := watchConfigTargets(targets, &dirty, pokeCh, &stderr)
	afterAdd2 := countProcessFDs(t)
	held2 := afterAdd2 - restartBaseline
	if held2 < nDirs/2 {
		cleanup2()
		t.Fatalf("restarted watcher did not hold a measurable number of FDs (restartBaseline=%d afterAdd2=%d held2=%d); stderr=%q",
			restartBaseline, afterAdd2, held2, stderr.String())
	}
	cleanup2()
	afterClose2 := countProcessFDs(t)
	residual2 := afterClose2 - restartBaseline
	if residual2 > fdCountNoise {
		t.Fatalf("FD count did not return to baseline after watcher restart: restartBaseline=%d afterAdd2=%d afterClose2=%d held2=%d residual2=%d; stderr=%q",
			restartBaseline, afterAdd2, afterClose2, held2, residual2, stderr.String())
	}
}

// fdCountNoise is slack for FDs the Go runtime or test harness may open
// while the test runs (not a leak threshold on the watch set). The
// assertion is still return-to-baseline: residual must stay far below
// the watcher's own held-while-open count.
//
// It also absorbs the two descriptors fsnotify tears down asynchronously.
// The cleanup returned by watchConfigTargets is synchronous for everything
// this test measures: it closes done, waits on registrationWG, calls
// watcher.Close(), then blocks on <-eventLoopDone. fsnotify v1.10.1's
// kqueue Close() unix.Close()es every watch descriptor inline before
// returning (fsnotify#740) -- those are the ~nDirs FDs the leak was about,
// so they are already released when cleanup() returns and the count needs
// no settling wait. Only w.kq and w.closepipe[0] are closed later, in the
// readEvents goroutine's defer, and that is at most 2 FDs against this
// slack of 8.
const fdCountNoise = 8

func countProcessFDs(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := 0; fd < 10240; fd++ {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err == nil {
			n++
		}
	}
	return n
}
