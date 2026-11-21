//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// writeRootEntry writes pid's cmdline and stat under a fake procfs root. An
// empty stat writes no stat file; a nil argv writes no cmdline.
func writeRootEntry(t *testing.T, root string, pid int, argv []string, stat string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if argv != nil {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if stat != "" {
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// procStatLine is a /proc/<pid>/stat record with the given comm and state.
func procStatLine(pid int, comm, state string) string {
	return strconv.Itoa(pid) + " (" + comm + ") " + state +
		" 1 " + strconv.Itoa(pid) + " " + strconv.Itoa(pid) +
		" 0 -1 0 0 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0\n"
}

func TestRootArgvAndAliveReadInjectedProcfs(t *testing.T) {
	root := t.TempDir()
	proxyArgv := []string{"/opt/beads/bd-1.3.0", BDProxyChildVerb, "--root", "/city/.beads/dolt"}
	writeRootEntry(t, root, 100, proxyArgv, procStatLine(100, "bd", "S"))
	writeRootEntry(t, root, 101, []string{"dolt", "sql-server"}, procStatLine(101, "dolt sql (x)", "R"))
	writeRootEntry(t, root, 102, []string{"zombie"}, procStatLine(102, "zombie", "Z"))
	writeRootEntry(t, root, 103, []string{"dead"}, procStatLine(103, "dead", "X"))
	writeRootEntry(t, root, 104, []string{"garbled"}, "104 (garbled) S 1\n")
	writeRootEntry(t, root, 105, nil, procStatLine(105, "nocmdline", "S"))
	SetScanRootForTesting(t, root)

	argv, err := RootArgv(100)
	if err != nil {
		t.Fatalf("RootArgv(100): %v", err)
	}
	if !slices.Equal(argv, proxyArgv) {
		t.Errorf("RootArgv(100) = %q, want %q", argv, proxyArgv)
	}
	for _, pid := range []int{105, 999, 0, -1} {
		if argv, err := RootArgv(pid); err == nil {
			t.Errorf("RootArgv(%d) = %q, want an error", pid, argv)
		}
	}

	for pid, want := range map[int]bool{
		100: true,  // sleeping
		101: true,  // running, comm with spaces and parentheses
		102: false, // zombie
		103: false, // dead
		104: false, // malformed stat
		106: false, // no entry
		0:   false,
		-1:  false,
	} {
		if got := RootAlive(pid); got != want {
			t.Errorf("RootAlive(%d) = %v, want %v", pid, got, want)
		}
	}
}

func TestRootReadsRefuseLiveProcUnderTest(t *testing.T) {
	self := os.Getpid()
	if argv, err := RootArgv(self); err == nil {
		t.Errorf("RootArgv(%d) read the live /proc under go test: %q", self, argv)
	}
	if RootAlive(self) {
		t.Errorf("RootAlive(%d) read the live /proc under go test", self)
	}
}
