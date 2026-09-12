//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestIsInfrastructureKillTargetReadsComm(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, comm string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(100, "tmux: server")
	write(101, "tmux")
	write(102, "claude")
	restore := SetScanRootForTesting(root)
	defer restore()
	for pid, want := range map[int]bool{100: true, 101: true, 102: false, 103: false} {
		if got := isInfrastructureKillTarget(pid); got != want {
			t.Errorf("isInfrastructureKillTarget(%d) = %v, want %v", pid, got, want)
		}
	}
}
