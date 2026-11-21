//go:build windows

package proctable

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// startPing starts a ping that outlives the test's reads, with creation flags
// flags and any new console window hidden, and returns it as the snapshot
// lists it under this process.
func startPing(t *testing.T, flags uint32) ChildProcess {
	t.Helper()
	cmd := exec.Command("ping.exe", "-n", "60", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ping: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	child, ok := findChild(t, os.Getpid(), cmd.Process.Pid)
	if !ok {
		t.Fatalf("ping PID %d is not among this process's children", cmd.Process.Pid)
	}
	return child
}

// TestConsoleHostSeparatesAttachedFromDetachedChildren pins what ConsoleHost
// distinguishes: a child that inherits this process's console shares its host,
// and one given a console of its own, a windowless console or none does not.
func TestConsoleHostSeparatesAttachedFromDetachedChildren(t *testing.T) {
	self, err := ConsoleHost(os.Getpid(), time.Time{})
	if err != nil {
		t.Fatalf("ConsoleHost(this process): %v", err)
	}
	if self == 0 {
		t.Skip("this test process has no console, so no child can share one")
	}

	attached := startPing(t, 0)
	if got, err := ConsoleHost(attached.PID, attached.Created); err != nil || got != self {
		t.Errorf("a child inheriting the console: ConsoleHost = %d, %v; want this process's host %d", got, err, self)
	}
	for _, tc := range []struct {
		name  string
		flags uint32
		none  bool
	}{
		{name: "a new console", flags: windows.CREATE_NEW_CONSOLE},
		{name: "a windowless console", flags: windows.CREATE_NO_WINDOW},
		{name: "no console", flags: windows.DETACHED_PROCESS, none: true},
	} {
		child := startPing(t, tc.flags)
		got, err := ConsoleHost(child.PID, child.Created)
		switch {
		case err != nil:
			t.Errorf("a child with %s: ConsoleHost: %v", tc.name, err)
		case got == self:
			t.Errorf("a child with %s shares this process's console host %d", tc.name, self)
		case tc.none && got != 0:
			t.Errorf("a child with %s: ConsoleHost = %d, want 0", tc.name, got)
		case !tc.none && got == 0:
			t.Errorf("a child with %s: ConsoleHost = 0, want the host of its own console", tc.name)
		}
	}
}

// TestProcessQueriesFenceTheSnapshottedProcess pins the creation-time fence:
// asked about the process a snapshot saw, both reads answer; asked about one
// created at any other time, or one that has exited, both report
// ErrProcessGone rather than describe whatever holds the PID now.
func TestProcessQueriesFenceTheSnapshottedProcess(t *testing.T) {
	cmd := exec.Command("ping.exe", "-n", "60", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ping: %v", err)
	}
	child, ok := findChild(t, os.Getpid(), cmd.Process.Pid)
	if !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("ping PID %d is not among this process's children", cmd.Process.Pid)
	}

	commandLine, err := ProcessCommandLine(child.PID, child.Created)
	if err != nil || !strings.Contains(commandLine, "-n 60 127.0.0.1") {
		t.Errorf("ProcessCommandLine = %q, %v; want ping's arguments", commandLine, err)
	}
	later := child.Created.Add(time.Nanosecond)
	if _, err := ConsoleHost(child.PID, later); !errors.Is(err, ErrProcessGone) {
		t.Errorf("ConsoleHost fenced to another creation time = %v, want ErrProcessGone", err)
	}
	if _, err := ProcessCommandLine(child.PID, later); !errors.Is(err, ErrProcessGone) {
		t.Errorf("ProcessCommandLine fenced to another creation time = %v, want ErrProcessGone", err)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := ConsoleHost(child.PID, child.Created); !errors.Is(err, ErrProcessGone) {
		t.Errorf("ConsoleHost after the process exited = %v, want ErrProcessGone", err)
	}
	if _, err := ProcessCommandLine(child.PID, child.Created); !errors.Is(err, ErrProcessGone) {
		t.Errorf("ProcessCommandLine after the process exited = %v, want ErrProcessGone", err)
	}
}
