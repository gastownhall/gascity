//go:build unix

package main

// The parent-death signal under test is EOF on an inherited pipe descriptor,
// handed to the child by number after dup(2). Windows has no descriptor
// numbering to inherit, and managed dolt is gated off there.
import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestManagedDoltTestParentDoneClosesOnPipeEOF(t *testing.T) {
	parentPipeRead, parentPipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer parentPipeRead.Close() //nolint:errcheck
	parentPipeFD, err := syscall.Dup(int(parentPipeRead.Fd()))
	if err != nil {
		t.Fatalf("dup parent pipe fd: %v", err)
	}
	done, closeDone, err := managedDoltTestParentDone(strconv.Itoa(parentPipeFD))
	if err != nil {
		_ = syscall.Close(parentPipeFD)
		t.Fatalf("managedDoltTestParentDone: %v", err)
	}
	defer closeDone()

	if err := parentPipeWrite.Close(); err != nil {
		t.Fatalf("close parent pipe writer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent pipe EOF did not close done channel")
	}
}

func TestManagedDoltWatchdogParentPipeEOFHonorsDisarm(t *testing.T) {
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "1")
	fakeDoltDir := writeFakeDoltSQLServer(t)
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	disarmFile := filepath.Join(dir, "watchdog.disarm")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(disarmFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("write disarm file: %v", err)
	}
	parentPipeRead, parentPipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create parent pipe: %v", err)
	}
	defer parentPipeRead.Close()  //nolint:errcheck
	defer parentPipeWrite.Close() //nolint:errcheck
	watchdogParentPipeFD, err := syscall.Dup(int(parentPipeRead.Fd()))
	if err != nil {
		t.Fatalf("dup parent pipe fd for watchdog: %v", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(watchdogParentPipeFD)
		t.Fatalf("create stdout pipe: %v", err)
	}
	defer stdoutRead.Close()  //nolint:errcheck
	defer stdoutWrite.Close() //nolint:errcheck
	stderrPath := filepath.Join(dir, "watchdog.stderr")
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = syscall.Close(watchdogParentPipeFD)
		t.Fatalf("open stderr file: %v", err)
	}
	defer stderrFile.Close() //nolint:errcheck

	result := make(chan int, 1)
	args := []string{strconv.Itoa(os.Getpid()), configPath, logPath, disarmFile, strconv.Itoa(watchdogParentPipeFD)}
	go func() {
		result <- runManagedDoltTestWatchdog(args, stdoutWrite, stderrFile)
	}()

	doltPID, err := readManagedDoltTestWatchdogPID(stdoutRead, os.Getpid())
	if err != nil {
		t.Fatalf("read fake dolt pid: %v", err)
	}
	t.Cleanup(func() { cleanupManagedDoltTestPID(t, doltPID) })
	if err := parentPipeWrite.Close(); err != nil {
		t.Fatalf("close parent pipe writer: %v", err)
	}
	select {
	case code := <-result:
		if code != 0 {
			stderrData, _ := os.ReadFile(stderrPath)
			t.Fatalf("watchdog exit code = %d, want 0; stderr:\n%s", code, stderrData)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit after disarm file and parent pipe EOF")
	}
	if !pidAlive(doltPID) {
		t.Fatalf("fake dolt pid %d exited; disarm file should win over parent pipe EOF", doltPID)
	}
	if _, err := os.Stat(disarmFile); !os.IsNotExist(err) {
		t.Fatalf("disarm file still exists after watchdog exit: %v", err)
	}
}
