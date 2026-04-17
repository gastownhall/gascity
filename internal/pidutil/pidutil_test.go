package pidutil

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestAliveTreatsZombieAsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie detection uses /proc on linux")
	}

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(cmd.Process.Pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Alive(%d) stayed true for exited child", cmd.Process.Pid)
}
