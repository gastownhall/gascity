//go:build unix

package main

// Deciding whether the supervisor log is already being written by an inherited
// redirection means comparing file descriptors, which needs dup(2). Windows
// inherits handles rather than numbered descriptors and has no equivalent
// comparison.
import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestShouldTeeSupervisorLogAvoidsDoubleLoggingForRedirectedFDs confirms the
// guard that prevents double-writes when stdout/stderr are already the
// supervisor log file, even when the fd carries a cosmetic /dev/stdout name.
func TestShouldTeeSupervisorLogAvoidsDoubleLoggingForRedirectedFDs(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "supervisor.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	// A duped fd with a cosmetic name models os.Stdout/os.Stderr after a
	// service manager or parent process already redirected fd 1/2 to the log.
	dupFD, err := syscall.Dup(int(logFile.Fd()))
	if err != nil {
		t.Fatalf("dup log fd: %v", err)
	}
	redirectedStdout := os.NewFile(uintptr(dupFD), "/dev/stdout")
	if redirectedStdout == nil {
		t.Fatal("os.NewFile returned nil for duplicated log fd")
	}
	defer redirectedStdout.Close() //nolint:errcheck
	if shouldTeeSupervisorLog(redirectedStdout, logFile) {
		t.Errorf("redirected stdout fd: shouldTeeSupervisorLog should return false")
	}
	if shouldTeeSupervisorLog(&switchableWriter{target: redirectedStdout}, logFile) {
		t.Errorf("switchable writer wrapping redirected stdout fd: shouldTeeSupervisorLog should return false")
	}

	// Different file = terminal/container output. Must tee.
	otherPath := filepath.Join(dir, "other.log")
	otherFile, err := os.Create(otherPath)
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	defer otherFile.Close() //nolint:errcheck
	if !shouldTeeSupervisorLog(otherFile, logFile) {
		t.Errorf("different file: shouldTeeSupervisorLog should return true")
	}

	// Non-file writer (a buffer simulating a journal pipe). Must tee.
	if !shouldTeeSupervisorLog(&bytes.Buffer{}, logFile) {
		t.Errorf("non-file writer: shouldTeeSupervisorLog should return true")
	}

	// Nil safety.
	if shouldTeeSupervisorLog(nil, logFile) {
		t.Errorf("nil writer: shouldTeeSupervisorLog should return false")
	}
	if shouldTeeSupervisorLog(otherFile, nil) {
		t.Errorf("nil logFile: shouldTeeSupervisorLog should return false")
	}
}
