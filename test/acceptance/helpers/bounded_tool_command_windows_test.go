//go:build windows

package acceptancehelpers

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestTerminateBoundedToolCommandAfterWaitReportsProcessDone(t *testing.T) {
	shell, err := exec.LookPath("cmd")
	if err != nil {
		t.Fatalf("find cmd.exe: %v", err)
	}
	cmd, _ := boundedToolCommand(t, time.Minute, shell, "/c", "exit 0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cmd.exe failed: %v\n%s", err, out)
	}
	if err := terminateBoundedToolCommand(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("terminate after Wait = %v, want os.ErrProcessDone", err)
	}
}
