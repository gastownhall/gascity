//go:build !windows

package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installScriptGit puts a fake git first on PATH whose script is body.
func installScriptGit(t *testing.T, body string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// startOrphanFake installs a fake git running body, with GC_TEST_READY_FIFO
// naming a fifo the script reports its orphan pid through. It returns a channel
// that yields that pid (0 when the read fails); the orphan is killed at cleanup.
func startOrphanFake(t *testing.T, body string) <-chan int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatalf("Mkfifo(ready): %v", err)
	}
	installScriptGit(t, body)
	t.Setenv("GC_TEST_READY_FIFO", ready)

	pidCh := make(chan int, 1)
	go func() {
		data, err := os.ReadFile(ready)
		if err != nil {
			pidCh <- 0
			return
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		pidCh <- pid
	}()
	t.Cleanup(func() {
		select {
		case pid := <-pidCh:
			if p, err := os.FindProcess(pid); err == nil && pid > 0 {
				_ = p.Kill()
			}
		default:
		}
	})
	return pidCh
}

func waitOrphanPid(t *testing.T, pidCh <-chan int) int {
	t.Helper()
	select {
	case pid := <-pidCh:
		if pid == 0 {
			t.Fatal("fake git never reported its orphan pid")
		}
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		})
		return pid
	case <-time.After(30 * time.Second):
		t.Fatal("fake git did not report its orphan pid within 30s")
		return 0
	}
}

// A ctx that carries a deadline must not leave runCtx waiting on a descendant
// that inherited the output pipe: the deadline kills git, then WaitDelay bounds
// the wait. The orphan lives 30s; the call must return in well under that.
func TestRunCtx_DeadlineBoundsWaitForPipeHoldingOrphanElapsed(t *testing.T) {
	pidCh := startOrphanFake(t,
		"sleep 30 &\n"+
			"echo $! > \"$GC_TEST_READY_FIFO\"\n"+
			"exec sleep 60\n")

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(t.TempDir()).runCtx(ctx, "status")
		done <- err
	}()

	waitOrphanPid(t, pidCh)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runCtx err = %v, want errors.Is(context.Canceled)", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("runCtx still waiting %s after cancel: a deadline-carrying ctx must bound the wait for a pipe holder", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Fatalf("runCtx returned %s after cancel, want under 10s (WaitDelay bound, orphan lives 30s)", elapsed)
	}
}

// Without a deadline runCtx keeps the plain exec behavior: it waits for the
// pipe holder, so a slow-but-finishing git (a helper still writing) is not
// truncated. The orphan holds the pipe for 4s after git itself has exited.
func TestRunCtx_WithoutDeadlineStillWaitsForPipeHolder(t *testing.T) {
	installScriptGit(t,
		"sleep 4 &\n"+
			"echo ok\n"+
			"exit 0\n")

	start := time.Now()
	out, err := New(t.TempDir()).runCtx(context.Background(), "status")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runCtx: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("out = %q, want the script output", out)
	}
	if elapsed < 3*time.Second {
		t.Fatalf("runCtx returned after %s, want it to wait out the 4s pipe holder (no WaitDelay without a deadline)", elapsed)
	}
}

// A git that exits while a descendant still holds its output pipe is an
// incomplete probe even under a live ctx: WaitDelay expires, and
// DefaultBranchCtx must return that error rather than walk on to a guessed
// "main". The orphan lives 30s; the call must return in well under that.
func TestDefaultBranchCtx_PipeHeldAfterExitReturnsErrorNotMain(t *testing.T) {
	holdPipe := "sleep 30 &\n" +
		"echo $! > \"$GC_TEST_READY_FIFO\"\n"
	cases := []struct {
		name string
		body string
	}{
		{
			name: "symbolic-ref",
			body: "case \"$1\" in symbolic-ref) " + holdPipe + "exit 0 ;; esac\n" +
				"exit 1\n",
		},
		{
			name: "show-ref",
			body: "case \"$1\" in symbolic-ref) exit 1 ;; show-ref) " + holdPipe + "exit 0 ;; esac\n" +
				"exit 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidCh := startOrphanFake(t, tc.body)
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()

			start := time.Now()
			branch, err := New(t.TempDir()).DefaultBranchCtx(ctx)
			elapsed := time.Since(start)
			pid := waitOrphanPid(t, pidCh)

			if !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("DefaultBranchCtx = %q, %v; want errors.Is(exec.ErrWaitDelay)", branch, err)
			}
			if branch != "" {
				t.Fatalf("branch = %q, want empty: an incomplete probe must not fall back to a guessed main", branch)
			}
			if elapsed >= 10*time.Second {
				t.Fatalf("DefaultBranchCtx returned after %s, want under 10s (WaitDelay bound, orphan lives 30s)", elapsed)
			}
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("orphan %d gone when DefaultBranchCtx returned (%v): the pipe was not still held", pid, err)
			}
		})
	}
}
