//go:build windows

package main

import (
	"bufio"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// A background child (gc supervisor start, the drift restart) must not share
// the console of the terminal that started it. A supervisor that did was
// measured shutting down, sessions and all, when that terminal got a Ctrl+C,
// and shutting down again when the terminal was closed.
func TestBackgroundChildLeavesTheTerminalsConsole(t *testing.T) {
	attr := backgroundSysProcAttr()
	if attr == nil || !sysProcAttrDetaches(attr) {
		t.Fatalf("backgroundSysProcAttr() = %+v, want a new process group", attr)
	}
	self, err := proctable.ConsoleHost(os.Getpid(), time.Time{})
	if err != nil {
		t.Fatalf("ConsoleHost(this process): %v", err)
	}
	if self == 0 {
		t.Skip("this test process has no console, so no child can share one")
	}
	cmd := exec.Command("ping.exe", "-n", "60", "127.0.0.1")
	cmd.SysProcAttr = attr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("ping stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting ping: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// A process's console is attached before its own code runs, so once ping
	// has written a line, the host read below is the one it runs with.
	// Immediately after Start, the read was measured to answer otherwise.
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("reading ping's first line: %v", err)
	}
	host, err := proctable.ConsoleHost(cmd.Process.Pid, time.Time{})
	if err != nil {
		t.Fatalf("ConsoleHost(background child): %v", err)
	}
	if host == self {
		t.Fatalf("the background child shares this process's console host %d", self)
	}
	if host == 0 {
		t.Fatal("the background child has no console, so every console program it starts would open a window")
	}
}
