//go:build windows

package main

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

// TestRunProviderOpCommandEndsTheCommandsTreeWhenCanceled pins that a canceled
// provider op (a timed-out agent-script shell action) ends the program its
// interpreter started, as the Unix process group's kill does. Killing only the
// interpreter left that program running after gc had reported the timeout.
func TestRunProviderOpCommandEndsTheCommandsTreeWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := runtime.ShellCommandContext(ctx,
		`$psi = [System.Diagnostics.ProcessStartInfo]::new('ping.exe', '-n 600 127.0.0.1')`+"\n"+
			`$psi.UseShellExecute = $false`+"\n"+
			`$psi.RedirectStandardOutput = $true`+"\n"+
			`$p = [System.Diagnostics.Process]::Start($psi)`+"\n"+
			`[Console]::Out.WriteLine("started $($p.Id)")`+"\n"+
			`$p.WaitForExit()`)
	if err != nil {
		t.Fatal(err)
	}
	cmd.WaitDelay = 2 * time.Second
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	cmd.Stdout = w
	done := make(chan error, 1)
	go func() { done <- runProviderOpCommand(cmd) }()

	line, err := bufio.NewReader(r).ReadString('\n')
	_ = w.Close()
	if err != nil {
		t.Fatalf("reading the started child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "started ")))
	if err != nil {
		t.Fatalf("parsing %q: %v", line, err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening the child %d: %v", pid, err)
	}
	defer windows.CloseHandle(child) //nolint:errcheck
	t.Cleanup(func() { _ = windows.TerminateProcess(child, 1) })

	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the canceled command did not return")
	}
	if event, err := windows.WaitForSingleObject(child, 10_000); err != nil || event != windows.WAIT_OBJECT_0 {
		t.Fatalf("the program the canceled command started is still running (wait = %d, %v)", event, err)
	}
}
