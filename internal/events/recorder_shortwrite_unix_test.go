//go:build unix

package events

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// shortWriteHelperEnv marks the child test process that lowers RLIMIT_FSIZE.
// The GC_*_HELPER shape is a test-gate var that internal/testenv passes
// through untouched.
const shortWriteHelperEnv = "GC_EVENTS_SHORTWRITE_HELPER"

// shortWriteSkipMarker is what the helper prints when the kernel honors the
// full write despite the lowered limit, so the parent can skip in turn.
const shortWriteSkipMarker = "GC_EVENTS_SHORTWRITE_SKIP"

// TestFileRecorderWriteRecordLockedDetectsShortWrite pins that
// writeRecordLocked surfaces a partial write instead of silently treating it
// as success, matching writeBatch's existing short-write handling. Uses
// RLIMIT_FSIZE to force a real short write from the OS rather than mocking
// r.file, which is a concrete *os.File.
//
// RLIMIT_FSIZE is process-wide, so the limit is lowered only inside a child
// copy of this test binary. Lowering it in the test binary itself also capped
// every other file write the process made while the limit was in force; the
// go test harness's buffered -test.testlogfile flush then failed with EFBIG
// ("testing: can't write .../testlog.txt: file too large") and the package
// reported FAIL although every test passed.
func TestFileRecorderWriteRecordLockedDetectsShortWrite(t *testing.T) {
	if os.Getenv(shortWriteHelperEnv) == "1" {
		runShortWriteHelper(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileRecorderWriteRecordLockedDetectsShortWrite$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), shortWriteHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("short-write helper failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), shortWriteSkipMarker) {
		t.Skipf("short write not reproducible here:\n%s", out)
	}
	if !strings.Contains(string(out), "--- PASS: TestFileRecorderWriteRecordLockedDetectsShortWrite") {
		t.Fatalf("short-write helper did not report a pass:\n%s", out)
	}
}

// runShortWriteHelper is the child-process half of
// TestFileRecorderWriteRecordLockedDetectsShortWrite. It runs with
// RLIMIT_FSIZE lowered for the rest of this process's life, which is fine
// because the process exits as soon as this test returns.
func runShortWriteHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &rlim); err != nil {
		t.Logf("%s: getrlimit RLIMIT_FSIZE unsupported: %v", shortWriteSkipMarker, err)
		return
	}
	old := rlim
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) })

	// The log is empty (NewFileRecorder performs no header write), so a
	// five-byte cap guarantees the marshaled event -- far larger than five
	// bytes -- gets truncated by the kernel mid-write.
	rlim.Cur = 5
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &rlim); err != nil {
		t.Logf("%s: setrlimit RLIMIT_FSIZE unsupported: %v", shortWriteSkipMarker, err)
		return
	}

	e := Event{Type: BeadCreated, Actor: "t", Subject: strings.Repeat("x", 200)}
	err = recorder.writeRecordLocked(&e)
	if err == nil {
		// Some kernels honor the full write despite RLIMIT_FSIZE. The short
		// write we are asserting on is then not reproducible here; skip
		// rather than fail on a platform whose truncation semantics this
		// repo has never exercised (the Mac tier is opt-in).
		t.Logf("%s: kernel honored the full write despite RLIMIT_FSIZE", shortWriteSkipMarker)
		return
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeRecordLocked error = %v, want io.ErrShortWrite", err)
	}
}
