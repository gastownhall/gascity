//go:build unix

package events

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestFileRecorderWriteRecordLockedDetectsShortWrite pins that
// writeRecordLocked surfaces a partial write instead of silently treating it
// as success, matching writeBatch's existing short-write handling. Uses
// RLIMIT_FSIZE to force a real short write from the OS rather than mocking
// r.file, which is a concrete *os.File.
func TestFileRecorderWriteRecordLockedDetectsShortWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	recorder, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &rlim); err != nil {
		t.Skipf("getrlimit RLIMIT_FSIZE unsupported: %v", err)
	}
	old := rlim
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) })

	// The log is empty (NewFileRecorder performs no header write), so a
	// five-byte cap guarantees the marshaled event -- far larger than five
	// bytes -- gets truncated by the kernel mid-write.
	rlim.Cur = 5
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &rlim); err != nil {
		t.Skipf("setrlimit RLIMIT_FSIZE unsupported: %v", err)
	}

	e := Event{Type: BeadCreated, Actor: "t", Subject: strings.Repeat("x", 200)}
	err = recorder.writeRecordLocked(&e)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeRecordLocked error = %v, want io.ErrShortWrite", err)
	}
}
