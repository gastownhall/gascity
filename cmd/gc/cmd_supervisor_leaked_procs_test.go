package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	leakedTestTempDir  = "/tmp"
	leakedTestDriftBin = "/tmp/gc-int-env-123456/bin/gc-drift"
	realSupervisorBin  = "/home/jaword/go/bin/gc"
)

func leakedProc(pid int, argv0 string, cwd, gcHome string) leakedSupervisorProc {
	return leakedSupervisorProc{
		PID:            pid,
		Argv:           []string{argv0, "supervisor", "run"},
		CWDState:       cwd,
		GCHomeState:    gcHome,
		StartTimeTicks: uint64(1000 + pid),
	}
}

func TestClassifyLeakedTestSupervisor(t *testing.T) {
	tests := []struct {
		name string
		proc leakedSupervisorProc
		want bool
	}{
		{
			name: "leaked test supervisor with deleted cwd is reaped",
			proc: leakedProc(10, leakedTestDriftBin, procPathStateDeleted, procPathStateLive),
			want: true,
		},
		{
			name: "leaked test supervisor with deleted GC_HOME is reaped",
			proc: leakedProc(11, leakedTestDriftBin, procPathStateLive, procPathStateDeleted),
			want: true,
		},
		{
			name: "systemd-variant drift binary is reaped",
			proc: leakedProc(12, "/tmp/gc-int-env-abc/bin/gc-drift-systemd", procPathStateDeleted, procPathStateUnknown),
			want: true,
		},
		{
			name: "test supervisor whose cwd and GC_HOME still exist is protected",
			proc: leakedProc(13, leakedTestDriftBin, procPathStateLive, procPathStateLive),
			want: false,
		},
		{
			name: "unknown path state is never a reap signal",
			proc: leakedProc(14, leakedTestDriftBin, procPathStateUnknown, procPathStateUnknown),
			want: false,
		},
		{
			name: "real unsuffixed supervisor with deleted cwd is never selected",
			proc: leakedProc(15, realSupervisorBin, procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "binary outside a gc-int-env directory is never selected",
			proc: leakedProc(16, "/tmp/some-other-dir/bin/gc", procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "gc-int-env prefix as a bare file name is not a directory match",
			proc: leakedProc(17, "/tmp/gc-int-env-123456", procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "gc-int-env directory outside the temp roots is never selected",
			proc: leakedProc(18, "/home/jaword/gc-int-env-1/bin/gc", procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "relative argv0 is never selected",
			proc: leakedProc(19, "gc-int-env-1/bin/gc", procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "path traversal out of the gc-int-env directory is never selected",
			proc: leakedProc(20, "/tmp/gc-int-env-1/../../home/jaword/go/bin/gc", procPathStateDeleted, procPathStateDeleted),
			want: false,
		},
		{
			name: "test env process that is not supervisor run is protected",
			proc: leakedSupervisorProc{
				PID:         21,
				Argv:        []string{leakedTestDriftBin, "start"},
				CWDState:    procPathStateDeleted,
				GCHomeState: procPathStateDeleted,
			},
			want: false,
		},
		{
			name: "supervisor subcommand other than run is protected",
			proc: leakedSupervisorProc{
				PID:         22,
				Argv:        []string{leakedTestDriftBin, "supervisor", "status"},
				CWDState:    procPathStateDeleted,
				GCHomeState: procPathStateDeleted,
			},
			want: false,
		},
		{
			name: "empty argv is protected",
			proc: leakedSupervisorProc{PID: 23, CWDState: procPathStateDeleted},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := classifyLeakedTestSupervisor(tt.proc, leakedTestTempDir)
			if got != tt.want {
				t.Fatalf("classifyLeakedTestSupervisor() = %v (%q), want %v", got, reason, tt.want)
			}
			if reason == "" {
				t.Errorf("classifyLeakedTestSupervisor() gave no reason")
			}
		})
	}
}

// TestClassifyLeakedTestSupervisorHonorsTempDir pins that os.TempDir() (which
// differs from /tmp under TMPDIR) is an accepted test root, matching the
// gc-int-env-* directories the integration harness creates with os.MkdirTemp.
func TestClassifyLeakedTestSupervisorHonorsTempDir(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "tmp")
	p := leakedProc(30, filepath.Join(tempDir, "gc-int-env-9", "bin", "gc-drift"), procPathStateDeleted, procPathStateLive)
	if got, reason := classifyLeakedTestSupervisor(p, tempDir); !got {
		t.Fatalf("classifyLeakedTestSupervisor() = false (%q), want true under TMPDIR root", reason)
	}
}

type leakedSweepFake struct {
	discovered []leakedSupervisorProc
	reread     map[int]leakedSupervisorProc
	signalErr  map[int]error
	signaled   []int
	signals    []syscall.Signal
}

func (f *leakedSweepFake) ops() leakedSupervisorOps {
	return leakedSupervisorOps{
		Discover: func() ([]leakedSupervisorProc, error) { return f.discovered, nil },
		Reread: func(pid int) (leakedSupervisorProc, bool) {
			p, ok := f.reread[pid]
			return p, ok
		},
		Signal: func(pid int, sig syscall.Signal) error {
			f.signaled = append(f.signaled, pid)
			f.signals = append(f.signals, sig)
			return f.signalErr[pid]
		},
	}
}

func TestSweepLeakedTestSupervisorsSignalsOnlyLeakedTestSupervisors(t *testing.T) {
	leaked := leakedProc(100, leakedTestDriftBin, procPathStateDeleted, procPathStateDeleted)
	live := leakedProc(101, leakedTestDriftBin, procPathStateLive, procPathStateLive)
	realSup := leakedProc(102, realSupervisorBin, procPathStateDeleted, procPathStateDeleted)
	f := &leakedSweepFake{
		discovered: []leakedSupervisorProc{leaked, live, realSup},
		reread: map[int]leakedSupervisorProc{
			100: leaked, 101: live, 102: realSup,
		},
	}
	var stderr bytes.Buffer
	got := sweepLeakedTestSupervisors(&stderr, f.ops(), leakedTestTempDir)

	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("sweep reaped %v, want [100]; stderr=%q", got, stderr.String())
	}
	if len(f.signaled) != 1 || f.signaled[0] != 100 {
		t.Fatalf("signaled %v, want only [100]", f.signaled)
	}
	if f.signals[0] != syscall.SIGTERM {
		t.Errorf("signal = %v, want SIGTERM", f.signals[0])
	}
}

func TestSweepLeakedTestSupervisorsRevalidatesIdentityBeforeSignalling(t *testing.T) {
	leaked := leakedProc(200, leakedTestDriftBin, procPathStateDeleted, procPathStateDeleted)

	reusedPID := leaked
	reusedPID.StartTimeTicks = leaked.StartTimeTicks + 5 // PID recycled to a new process

	becameReal := leaked
	becameReal.Argv = []string{realSupervisorBin, "supervisor", "run"} // exec'd a different image

	cwdRevived := leaked
	cwdRevived.CWDState = procPathStateLive
	cwdRevived.GCHomeState = procPathStateLive

	tests := []struct {
		name   string
		reread map[int]leakedSupervisorProc
	}{
		{"pid reused by a newer process", map[int]leakedSupervisorProc{200: reusedPID}},
		{"process now runs the real supervisor binary", map[int]leakedSupervisorProc{200: becameReal}},
		{"scope no longer looks deleted", map[int]leakedSupervisorProc{200: cwdRevived}},
		{"process vanished", map[int]leakedSupervisorProc{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &leakedSweepFake{
				discovered: []leakedSupervisorProc{leaked},
				reread:     tt.reread,
			}
			var stderr bytes.Buffer
			got := sweepLeakedTestSupervisors(&stderr, f.ops(), leakedTestTempDir)
			if len(got) != 0 || len(f.signaled) != 0 {
				t.Fatalf("reaped %v / signaled %v, want nothing; stderr=%q", got, f.signaled, stderr.String())
			}
		})
	}
}

func TestSweepLeakedTestSupervisorsRefusesZeroStartTicks(t *testing.T) {
	// Without a start-time tick there is no PID-reuse check, so the sweep
	// must not signal even a perfectly classified process.
	leaked := leakedProc(250, leakedTestDriftBin, procPathStateDeleted, procPathStateDeleted)
	leaked.StartTimeTicks = 0
	f := &leakedSweepFake{
		discovered: []leakedSupervisorProc{leaked},
		reread:     map[int]leakedSupervisorProc{250: leaked},
	}
	var stderr bytes.Buffer
	if got := sweepLeakedTestSupervisors(&stderr, f.ops(), leakedTestTempDir); len(got) != 0 || len(f.signaled) != 0 {
		t.Fatalf("reaped %v / signaled %v, want nothing without a start identity", got, f.signaled)
	}
}

func TestSweepLeakedTestSupervisorsWarnsAndContinuesOnSignalError(t *testing.T) {
	a := leakedProc(300, leakedTestDriftBin, procPathStateDeleted, procPathStateDeleted)
	b := leakedProc(301, leakedTestDriftBin, procPathStateDeleted, procPathStateDeleted)
	f := &leakedSweepFake{
		discovered: []leakedSupervisorProc{a, b},
		reread:     map[int]leakedSupervisorProc{300: a, 301: b},
		signalErr:  map[int]error{300: errors.New("operation not permitted")},
	}
	var stderr bytes.Buffer
	got := sweepLeakedTestSupervisors(&stderr, f.ops(), leakedTestTempDir)

	if len(f.signaled) != 2 {
		t.Fatalf("signaled %v, want both attempted", f.signaled)
	}
	if len(got) != 1 || got[0] != 301 {
		t.Fatalf("reaped %v, want only the successful [301]", got)
	}
	if !strings.Contains(stderr.String(), "300") || !strings.Contains(stderr.String(), "operation not permitted") {
		t.Errorf("stderr = %q, want a warning naming pid 300 and the error", stderr.String())
	}
}

func TestSweepLeakedTestSupervisorsDiscoverFailureIsWarningOnly(t *testing.T) {
	var stderr bytes.Buffer
	ops := leakedSupervisorOps{
		Discover: func() ([]leakedSupervisorProc, error) { return nil, errors.New("proc unreadable") },
		Reread:   func(int) (leakedSupervisorProc, bool) { t.Fatal("Reread called"); return leakedSupervisorProc{}, false },
		Signal:   func(int, syscall.Signal) error { t.Fatal("Signal called"); return nil },
	}
	if got := sweepLeakedTestSupervisors(&stderr, ops, leakedTestTempDir); len(got) != 0 {
		t.Fatalf("reaped %v, want none", got)
	}
	if !strings.Contains(stderr.String(), "proc unreadable") {
		t.Errorf("stderr = %q, want the discovery error", stderr.String())
	}
}

// TestSweepStaleIsolatedSupervisorServicesRunsLeakedProcessSweepOnLinux pins
// the wiring: the existing install/uninstall sweep also reaps leaked processes
// on Linux, and does not on a platform without /proc.
func TestSweepStaleIsolatedSupervisorServicesRunsLeakedProcessSweepOnLinux(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldGOOS := supervisorRuntimeGOOS
	oldSweep := sweepLeakedTestSupervisorsFn
	t.Cleanup(func() {
		supervisorRuntimeGOOS = oldGOOS
		sweepLeakedTestSupervisorsFn = oldSweep
	})
	calls := 0
	sweepLeakedTestSupervisorsFn = func(io.Writer) { calls++ }

	supervisorRuntimeGOOS = "linux"
	var stderr bytes.Buffer
	sweepStaleIsolatedSupervisorServices(&stderr)
	if calls != 1 {
		t.Errorf("linux: leaked-process sweep ran %d times, want 1", calls)
	}

	supervisorRuntimeGOOS = "darwin"
	sweepStaleIsolatedSupervisorServices(&stderr)
	if calls != 1 {
		t.Errorf("darwin: leaked-process sweep ran, want it skipped (no /proc)")
	}
}
