package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessArgsFromPSReturnsWhenPSHangs(t *testing.T) {
	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "ps")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	_, err := processArgsFromPS(os.Getpid(), 100*time.Millisecond)
	if err == nil {
		t.Fatalf("processArgsFromPS succeeded with a hanging ps")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("processArgsFromPS took %s, want bounded timeout", elapsed)
	}
}

func TestFindPortHolderPIDUsesProcBeforeLsof(t *testing.T) {
	if _, err4 := os.Stat("/proc/net/tcp"); err4 != nil {
		if _, err6 := os.Stat("/proc/net/tcp6"); err6 != nil {
			t.Skip("requires Linux /proc TCP tables")
		}
	}

	listener := listenOnRandomPort(t)
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	binDir := t.TempDir()
	psPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(psPath, []byte("#!/bin/sh\nexec sleep 2\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	pid := findPortHolderPID(strconv.Itoa(port))
	if pid != os.Getpid() {
		t.Fatalf("findPortHolderPID(%d) = %d, want current pid %d", port, pid, os.Getpid())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("findPortHolderPID took %s, want /proc path before lsof", elapsed)
	}
}

func TestPIDFromPlainPortLsofOutput(t *testing.T) {
	output := fmt.Sprintf(`COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt    %d user   12u  IPv4 0x1234      0t0  TCP *:3306 (LISTEN)
`, os.Getpid())
	if pid := pidFromPlainPortLsofOutput(output, "3306"); pid != os.Getpid() {
		t.Fatalf("pidFromPlainPortLsofOutput() = %d, want %d", pid, os.Getpid())
	}
}

func TestProcessCWDFromLsofParsesNameRecord(t *testing.T) {
	binDir := t.TempDir()
	lsofPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(lsofPath, []byte("#!/bin/sh\nprintf 'p123\\nfcwd\\nn/private/var/folders/example/.beads/dolt\\n'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	cwd, ok := processCWDFromLsof(123)
	if !ok {
		t.Fatal("processCWDFromLsof did not find cwd")
	}
	if !samePath(cwd, "/var/folders/example/.beads/dolt") {
		t.Fatalf("processCWDFromLsof = %q, want path equivalent to /var/folders/example/.beads/dolt", cwd)
	}
}

func TestCWDFromPlainLsofOutput(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /private/tmp/gc-city/.beads/dolt
`
	cwd, ok := cwdFromPlainLsofOutput(output)
	if !ok {
		t.Fatal("cwdFromPlainLsofOutput did not find cwd")
	}
	if !samePath(cwd, "/tmp/gc-city/.beads/dolt") {
		t.Fatalf("cwdFromPlainLsofOutput = %q, want path equivalent to /tmp/gc-city/.beads/dolt", cwd)
	}
}

func TestCWDFromPlainLsofOutputPreservesSpacesInPath(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /tmp/my city/.beads/dolt
`
	cwd, ok := cwdFromPlainLsofOutput(output)
	if !ok {
		t.Fatal("cwdFromPlainLsofOutput did not find cwd")
	}
	if cwd != "/tmp/my city/.beads/dolt" {
		t.Fatalf("cwdFromPlainLsofOutput = %q, want full spaced path", cwd)
	}
}

func TestDeletedDataInodeTargetsFromLsofParsesNameRecords(t *testing.T) {
	binDir := t.TempDir()
	lsofPath := filepath.Join(binDir, "lsof")
	if err := os.WriteFile(lsofPath, []byte("#!/bin/sh\nprintf 'p123\\nn/private/var/folders/example/.beads/dolt/held.db (deleted)\\nn/private/var/folders/example/.beads/dolt/hq/.dolt/noms/LOCK (deleted)\\n'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	targets := deletedDataInodeTargetsFromLsof(123)
	if len(targets) != 2 {
		t.Fatalf("deletedDataInodeTargetsFromLsof returned %d targets, want 2: %#v", len(targets), targets)
	}
	if !pathWithinOrSame(targets[0], "/var/folders/example/.beads/dolt") {
		t.Fatalf("target %q should be within canonical data dir", targets[0])
	}
	if !benignManagedDeletedInodeTarget(targets[1]) {
		t.Fatalf("target %q should be treated as benign noms lock", targets[1])
	}
}

func TestDeletedDataInodeTargetsFromFormattedLsofIgnoresLiveNameRecords(t *testing.T) {
	targets := deletedDataInodeTargetsFromFormattedLsofOutput("p123\nn/private/tmp/gc-city/.beads/dolt/active.db\n")
	if len(targets) != 0 {
		t.Fatalf("deletedDataInodeTargetsFromFormattedLsofOutput returned live targets: %#v", targets)
	}
}

func TestDeletedDataInodeTargetsFromFormattedLsofUsesZeroLinkCount(t *testing.T) {
	targets := deletedDataInodeTargetsFromFormattedLsofOutput("p123\nf4\nn/private/tmp/gc-city/.beads/dolt/held.db\nk0\n")
	if len(targets) != 1 {
		t.Fatalf("deletedDataInodeTargetsFromFormattedLsofOutput returned %d targets, want 1: %#v", len(targets), targets)
	}
	if !samePath(targets[0], "/tmp/gc-city/.beads/dolt/held.db") {
		t.Fatalf("target = %q, want held.db", targets[0])
	}
}

func TestDeletedDataInodeTargetsFromPlainLsofOutputPreservesSpacesInPath(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
dolt      123 user  cwd    DIR   1,4       96  42 /tmp/my city/.beads/dolt
dolt      123 user    5u   REG   1,4     4096  99 /tmp/my city/.beads/dolt/held.db (deleted)
`
	targets := deletedDataInodeTargetsFromPlainLsofOutput(output)
	if len(targets) != 1 {
		t.Fatalf("deletedDataInodeTargetsFromPlainLsofOutput returned %d targets, want 1: %#v", len(targets), targets)
	}
	if targets[0] != "/tmp/my city/.beads/dolt/held.db" {
		t.Fatalf("target = %q, want full spaced path", targets[0])
	}
}
