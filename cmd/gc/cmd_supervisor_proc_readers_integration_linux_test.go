//go:build integration && linux

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// startSupervisorProcReaderChild supplies a real kernel cmdline without running
// a supervisor. The shell uses only builtins and waits on a parent-owned pipe.
func startSupervisorProcReaderChild(t *testing.T, args, env []string) *exec.Cmd {
	t.Helper()
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skipf("process cmdline unavailable: %v", err)
	}
	dir := t.TempDir()
	for _, script := range []string{"supervisor", "session"} {
		if err := os.WriteFile(filepath.Join(dir, script), []byte("printf R\nIFS= read -r control\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), hangBudget)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", args...)
	cmd.Args[0] = filepath.Join(dir, "gc-int-env-reader", "bin", "gc-drift")
	cmd.Dir = dir
	cmd.Env = append([]string{}, env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdout.Close() })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var ready [1]byte
	if _, err := io.ReadFull(stdout, ready[:]); err != nil {
		t.Fatalf("child readiness: %v", err)
	}
	return cmd
}

func requireSupervisorProcReaderChild(t *testing.T, cmd *exec.Cmd) leakedSupervisorProc {
	t.Helper()
	p, ok := readLeakedSupervisorProc(cmd.Process.Pid)
	if !ok {
		t.Fatal("live supervisor-run child was not recognized")
	}
	return p
}

func TestReadLeakedSupervisorProcPreservesKernelIdentity(t *testing.T) {
	for _, concept := range []string{"pid", "argv including extra arguments", "kernel start ticks"} {
		t.Run(concept, func(t *testing.T) {
			cmd := startSupervisorProcReaderChild(t, []string{"supervisor", "run", "--extra"}, nil)
			p := requireSupervisorProcReaderChild(t, cmd)
			switch concept {
			case "pid":
				if p.PID != cmd.Process.Pid {
					t.Fatalf("PID = %d, want %d", p.PID, cmd.Process.Pid)
				}
			case "argv including extra arguments":
				if !reflect.DeepEqual(p.Argv, cmd.Args) {
					t.Fatalf("argv = %q, want %q", p.Argv, cmd.Args)
				}
			case "kernel start ticks":
				data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "stat"))
				if err != nil {
					t.Fatal(err)
				}
				fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
				if len(fields) <= 19 {
					t.Fatalf("kernel stat has no start ticks: %q", data)
				}
				want, err := strconv.ParseUint(fields[19], 10, 64)
				if err != nil || want == 0 {
					t.Fatalf("kernel start ticks = %q: %v", fields[19], err)
				}
				if p.StartTimeTicks != want {
					t.Fatalf("start ticks = %d, want kernel identity %d", p.StartTimeTicks, want)
				}
			}
		})
	}
}

func TestReadLeakedSupervisorProcFiltersCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"different command", []string{"session", "run"}},
		{"different supervisor verb", []string{"supervisor", "status"}},
		{"missing supervisor verb", []string{"supervisor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := startSupervisorProcReaderChild(t, tc.args, nil)
			if p, ok := readLeakedSupervisorProc(cmd.Process.Pid); ok {
				t.Fatalf("non-supervisor command %q was accepted: %+v", cmd.Args, p)
			}
		})
	}
}

func TestReadLeakedSupervisorProcReportsCWDState(t *testing.T) {
	for _, want := range []string{procPathStateLive, procPathStateDeleted} {
		t.Run(want, func(t *testing.T) {
			cmd := startSupervisorProcReaderChild(t, []string{"supervisor", "run"}, nil)
			if want == procPathStateDeleted {
				if err := os.RemoveAll(cmd.Dir); err != nil {
					t.Fatal(err)
				}
			}
			if got := requireSupervisorProcReaderChild(t, cmd).CWDState; got != want {
				t.Fatalf("cwd state = %q, want %q", got, want)
			}
		})
	}
}

func TestReadLeakedSupervisorProcReportsGCHomeState(t *testing.T) {
	for _, state := range []string{"live", "deleted", "unset", "relative"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			env := []string{"GC_HOME=" + home}
			want := procPathStateLive
			switch state {
			case "deleted":
				want = procPathStateDeleted
			case "unset":
				env, want = nil, procPathStateUnknown
			case "relative":
				env, want = []string{"GC_HOME=missing-relative-home"}, procPathStateUnknown
			}
			cmd := startSupervisorProcReaderChild(t, []string{"supervisor", "run"}, env)
			if state == "deleted" {
				if err := os.Remove(home); err != nil {
					t.Fatal(err)
				}
			}
			if got := requireSupervisorProcReaderChild(t, cmd).GCHomeState; got != want {
				t.Fatalf("GC_HOME state = %q, want %q", got, want)
			}
		})
	}
}

func TestReadLeakedSupervisorProcRejectsExitedChild(t *testing.T) {
	cmd := startSupervisorProcReaderChild(t, []string{"supervisor", "run"}, nil)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child exited successfully")
	}
	if p, ok := readLeakedSupervisorProc(cmd.Process.Pid); ok {
		t.Fatalf("waited child was accepted: %+v", p)
	}
}

func TestDiscoverLeakedSupervisorProcsIncludesLiveChild(t *testing.T) {
	cmd := startSupervisorProcReaderChild(t, []string{"supervisor", "run"}, nil)
	procs, err := discoverLeakedSupervisorProcs()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == cmd.Process.Pid {
			return
		}
	}
	t.Fatalf("discovery omitted live child %d", cmd.Process.Pid)
}

func TestDiscoverLeakedSupervisorProcsExcludesCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), hangBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "supervisor", "run")
	cmd.Args[0] = filepath.Join(t.TempDir(), "gc-int-env-self", "bin", "gc-drift")
	cmd.Env = []string{supervisorProcReaderSelfCheckEnv + "=1"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("matching supervisor caller was not excluded: %v\n%s", err, output)
	}
	if string(output) != "matching caller excluded\n" {
		t.Fatalf("self-check did not run: %q", output)
	}
}
