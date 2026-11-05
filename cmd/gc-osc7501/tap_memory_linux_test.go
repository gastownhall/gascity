package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This child executes initialization from an unlinked copy, as an atomic
// companion upgrade can leave it. Re-exec must retain the running image, PID,
// arguments, unread stdin and the caller's stricter memory limit.
func TestTapMemoryReexecKeepsUnlinkedImage(t *testing.T) {
	if os.Getenv("GC_TEST_TAP_MEMORY_CHILD") == "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(executable); err == nil {
			// Force the startup path even if the test runner disabled THP.
			if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 0, 0, 0, 0); err != nil {
				t.Fatal(err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := configureTapMemory(); err != nil {
			t.Fatal(err)
		}
		disabled, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
		if err != nil || disabled != 1 {
			t.Fatalf("THP policy %d: %v", disabled, err)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		report := struct {
			PID   int
			Argv  []string
			Input string
			Limit int64
		}{os.Getpid(), os.Args, string(data), debug.SetMemoryLimit(-1)}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "tap-memory-child")
	if err := os.WriteFile(copyPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	arg := "-test.run=^TestTapMemoryReexecKeepsUnlinkedImage$"
	cmd := exec.CommandContext(ctx, copyPath, arg)
	cmd.Env = append(os.Environ(), "GC_TEST_TAP_MEMORY_CHILD=1", "GOMEMLIMIT=3MiB")
	cmd.Stdin = strings.NewReader("unread tap input")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unlinked initialization: %v\n%s", err, out)
	}
	var report struct {
		PID   int
		Argv  []string
		Input string
		Limit int64
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("child report %q: %v", out, err)
	}
	if report.PID != cmd.ProcessState.Pid() || len(report.Argv) != 2 || report.Argv[0] != copyPath || report.Argv[1] != arg || report.Input != "unread tap input" || report.Limit != 3<<20 {
		t.Fatalf("initialization changed process identity or input: %+v", report)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("child did not exercise an unlinked image: %v", err)
	}
}
