package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// TestGatherSupervisorBinary wires the process-table and /health probes into
// the doctor.SupervisorBinary the supervisor-binary check consumes.
func TestGatherSupervisorBinary(t *testing.T) {
	oldExe := readSupervisorExeStateHook
	oldStatus := supervisorBinaryStatusHook
	t.Cleanup(func() {
		readSupervisorExeStateHook = oldExe
		supervisorBinaryStatusHook = oldStatus
	})
	t.Setenv(supervisorSystemdUnitEnv, "")

	var gotPID int
	readSupervisorExeStateHook = func(pid int) (string, bool, error) {
		gotPID = pid
		return "/opt/homebrew/bin/gc", true, nil
	}
	supervisorBinaryStatusHook = func() (SupervisorStatus, error) {
		return SupervisorStatus{BuildID: "old1234", Version: "1.4.2"}, nil
	}

	info := gatherSupervisorBinary(4242)
	if gotPID != 4242 {
		t.Fatalf("exe probe pid = %d, want 4242", gotPID)
	}
	if info.ExePath != "/opt/homebrew/bin/gc" || !info.ExeDeleted || info.ExeErr != nil {
		t.Fatalf("exe fields = %q deleted=%v err=%v", info.ExePath, info.ExeDeleted, info.ExeErr)
	}
	if info.BuildID != "old1234" || info.Version != "1.4.2" || info.StatusErr != nil {
		t.Fatalf("status fields = %q %q err=%v", info.BuildID, info.Version, info.StatusErr)
	}
	if info.LocalBuildID != commit || info.LocalVersion != version {
		t.Fatalf("local fields = %q %q, want %q %q", info.LocalBuildID, info.LocalVersion, commit, version)
	}
	for _, want := range []string{"gc supervisor stop --wait", "gc supervisor install"} {
		if !strings.Contains(info.RestartHint, want) {
			t.Fatalf("RestartHint = %q, want it to contain %q", info.RestartHint, want)
		}
	}

	exeErr := errors.New("exe probe failed")
	statusErr := errors.New("health probe failed")
	readSupervisorExeStateHook = func(int) (string, bool, error) { return "", false, exeErr }
	supervisorBinaryStatusHook = func() (SupervisorStatus, error) { return SupervisorStatus{}, statusErr }
	info = gatherSupervisorBinary(4242)
	if !errors.Is(info.ExeErr, exeErr) || !errors.Is(info.StatusErr, statusErr) {
		t.Fatalf("probe errors not propagated: exe=%v status=%v", info.ExeErr, info.StatusErr)
	}
}

// TestReadSupervisorExeStateDetectsRemovedLaunchPath starts a real process
// from a path, removes that path while the process keeps running, and asserts
// readSupervisorExeState reports the path as deleted — the `brew unlink`
// failure mode. Linux sees an unlinked binary through /proc/<pid>/exe; other
// platforms see the launch path reported by ps disappear.
func TestReadSupervisorExeStateDetectsRemovedLaunchPath(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	sleepBin, err = filepath.EvalSymlinks(sleepBin)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(dir, "gc")
	if goruntime.GOOS == "linux" {
		data, err := os.ReadFile(sleepBin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(launch, data, 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Symlink(sleepBin, launch); err != nil {
		// A symlinked launch path mirrors Homebrew's /opt/homebrew/bin/gc and
		// avoids re-signing a copied system binary on macOS.
		t.Fatal(err)
	}

	cmd := exec.Command(launch, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", launch, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid

	path, deleted := waitForSupervisorExeState(t, pid, launch)
	if deleted {
		t.Fatalf("readSupervisorExeState(%d) = %q deleted=true before removal", pid, path)
	}
	if err := os.Remove(launch); err != nil {
		t.Fatal(err)
	}
	path, deleted, err = readSupervisorExeState(pid)
	if err != nil {
		t.Fatalf("readSupervisorExeState after removal: %v", err)
	}
	if !deleted {
		t.Fatalf("readSupervisorExeState(%d) = %q deleted=false after removing %s", pid, path, launch)
	}
	if path != launch {
		t.Fatalf("readSupervisorExeState path = %q, want launch path %q", path, launch)
	}
}

// waitForSupervisorExeState polls until the child has exec'd launch, so a
// probe taken between fork and exec does not report the test binary.
func waitForSupervisorExeState(t *testing.T, pid int, launch string) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		path, deleted, err := readSupervisorExeState(pid)
		if err == nil && path == launch {
			return path, deleted
		}
		if time.Now().After(deadline) {
			t.Fatalf("readSupervisorExeState(%d) never succeeded: path=%q err=%v", pid, path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
