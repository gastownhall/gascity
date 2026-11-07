//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestForeignUserServiceReadsInjectedCgroup(t *testing.T) {
	root := t.TempDir()
	SetScanRootForTesting(t, root)
	const (
		detachedPID = 5100
		scopePID    = 5200
		ownPID      = 5300
		systemPID   = 5400
	)
	writeCgroup(t, root, detachedPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/run-detached.service")
	writeCgroup(t, root, scopePID, "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-abc.scope")
	writeCgroup(t, root, ownPID, "/user.slice/user-1000.slice/user@1000.service/app.slice/agent.service")
	writeCgroup(t, root, systemPID, "/system.slice/rogue.service")

	unit, foreign := ForeignUserService(detachedPID, "")
	if !foreign || unit != "run-detached.service" {
		t.Fatalf("detached service = %q, foreign %v; want run-detached.service, true", unit, foreign)
	}
	unit, foreign = ForeignUserService(detachedPID, "run-detached.service")
	if foreign || unit != "run-detached.service" {
		t.Fatalf("session's own service = %q, foreign %v; want run-detached.service, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(scopePID, ""); foreign || unit != "" {
		t.Fatalf("scope = %q, foreign %v; want empty, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(ownPID, "agent.service"); foreign || unit != "agent.service" {
		t.Fatalf("own unit = %q, foreign %v; want agent.service, false", unit, foreign)
	}
	if unit, foreign = ForeignUserService(systemPID, ""); foreign || unit != "" {
		t.Fatalf("system service = %q, foreign %v; want empty, false (not below the user manager)", unit, foreign)
	}
	if unit, foreign = ForeignUserService(99999, ""); foreign || unit != "" {
		t.Fatalf("missing pid = %q, foreign %v; want empty, false", unit, foreign)
	}
}

func writeCgroup(t *testing.T, root string, pid int, path string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}
}
