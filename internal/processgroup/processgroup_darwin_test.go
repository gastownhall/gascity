//go:build darwin

package processgroup

import (
	"os/exec"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// A leader that has exited but not been reaped is a zombie, and on Darwin
// kill(-pgid) then answers EPERM rather than ESRCH. The group is gone all the
// same, and must read that way.
func TestDarwinAllZombieGroupReadsAsGone(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("true")
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pgid := cmd.Process.Pid

	deadline := time.Now().Add(5 * time.Second)
	for alive(pgid, Options{}) {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still reads alive 5s after its only member exited", pgid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := Terminate(pgid, time.Second, Options{}); err != nil {
		t.Errorf("Terminate() on an all-zombie group = %v, want nil", err)
	}
	if err := KillCommandGroup(cmd, pgid, Options{}); err != nil {
		t.Errorf("KillCommandGroup() on an all-zombie group = %v, want nil", err)
	}
}
