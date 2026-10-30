//go:build integration && linux

package integration

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const driftPdeathsigHelperEnv = "GC_TEST_DRIFT_PDEATHSIG_HELPER"

// TestDriftSupervisorSysProcAttr pins the launch attributes every
// harness-spawned drift supervisor gets: its own process group, the
// caller's credential, and a kernel-delivered SIGKILL when the spawning
// test process dies (ga-rlnmsp).
func TestDriftSupervisorSysProcAttr(t *testing.T) {
	attr := driftSupervisorSysProcAttr(nil)
	if !attr.Setpgid {
		t.Error("Setpgid = false, want true")
	}
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v, want SIGKILL", attr.Pdeathsig)
	}
	if attr.Credential != nil {
		t.Errorf("Credential = %+v, want nil when none requested", attr.Credential)
	}

	cred := &syscall.Credential{Uid: 4242}
	attr = driftSupervisorSysProcAttr(cred)
	if attr.Credential != cred {
		t.Errorf("Credential = %+v, want the requested credential", attr.Credential)
	}
	if attr.Pdeathsig != syscall.SIGKILL || !attr.Setpgid {
		t.Errorf("credentialed attrs lost Setpgid/Pdeathsig: %+v", attr)
	}
}

// TestDriftSupervisorDiesWithParent kills the process that spawned a child
// through driftSupervisorSysProcAttr and requires the child to die with it,
// the property that stops a crashed or interrupted test run from leaking
// supervisors.
func TestDriftSupervisorDiesWithParent(t *testing.T) {
	if os.Getenv(driftPdeathsigHelperEnv) != "" {
		t.Skip("helper-process mode")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDriftPdeathsigHelperParent$")
	cmd.Env = append(os.Environ(), driftPdeathsigHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	parent := cmd.Process
	t.Cleanup(func() { _ = parent.Kill(); _, _ = parent.Wait() })

	var childPID int
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "CHILD_PID="); ok {
			childPID, err = strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("bad child pid %q: %v", rest, err)
			}
			break
		}
	}
	if childPID == 0 {
		t.Fatal("helper parent never reported a child pid")
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d not alive before parent death: %v", childPID, err)
	}
	if err := parent.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = parent.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidRunning(childPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child %d outlived its spawning parent", childPID)
}

// pidRunning reports whether pid is a live, non-zombie process.
func pidRunning(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	return i >= 0 && len(s) > i+2 && s[i+2] != 'Z'
}

// TestDriftPdeathsigHelperParent is the spawning side of
// TestDriftSupervisorDiesWithParent; it only does anything when re-executed
// with the helper env var.
func TestDriftPdeathsigHelperParent(t *testing.T) {
	if os.Getenv(driftPdeathsigHelperEnv) == "" {
		t.Skip("only runs as the re-executed helper parent")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(sleep, "300")
	child.SysProcAttr = driftSupervisorSysProcAttr(nil)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("CHILD_PID=" + strconv.Itoa(child.Process.Pid) + "\n") //nolint:errcheck
	time.Sleep(10 * time.Minute)
}
