//go:build unix

package fsyncprobe

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProbeBoundHoldsForCaller is the proof of the binding trap: a worker that
// never answers (a D-state fsync) still holds its copy of the result pipe, and
// must not hold the caller past the bound.
func TestProbeBoundHoldsForCaller(t *testing.T) {
	bound := 300 * time.Millisecond
	start := time.Now()
	res := probe(bound, func(result *os.File) error {
		fd, err := syscall.Dup(int(result.Fd()))
		if err != nil {
			return err
		}
		held := os.NewFile(uintptr(fd), "held-result")
		t.Cleanup(func() { _ = held.Close() })
		return nil
	})
	elapsed := time.Since(start)

	if res.Status != StatusUnfinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusUnfinished)
	}
	if res.FsyncMS != bound.Milliseconds() {
		t.Fatalf("FsyncMS = %d, want the bound %d", res.FsyncMS, bound.Milliseconds())
	}
	if elapsed < bound || elapsed > bound+2*time.Second {
		t.Fatalf("returned after %v, want within [%v, %v]", elapsed, bound, bound+2*time.Second)
	}
}

// TestWorkerCommandIsDetached pins the properties the bound relies on: a new
// session, null stdio, and the result pipe as ResultFD.
func TestWorkerCommandIsDetached(t *testing.T) {
	result, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	cmd := workerCommand("/path/to/gc", "/data/dir", result)

	// exec numbers ExtraFiles from fd 3, and the worker opens ResultFD by
	// number: the two must be the same descriptor.
	if ResultFD != 3 {
		t.Fatalf("ResultFD = %d, want 3 (the first ExtraFiles slot)", ResultFD)
	}

	want := []string{"/path/to/gc", "dolt-state", "fsync-probe-worker", "--dir", "/data/dir"}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Args = %v, want %v", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %+v, want Setsid", cmd.SysProcAttr)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("worker stdio must be null so no inherited pipe outlives the call")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != result {
		t.Fatalf("ExtraFiles = %v, want the result pipe as fd %d", cmd.ExtraFiles, ResultFD)
	}
}
